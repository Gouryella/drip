package tcp

import (
	"context"
	"fmt"

	json "github.com/goccy/go-json"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"drip/internal/server/reservations"
	"drip/internal/server/tunnel"
	"drip/internal/shared/protocol"
	"drip/internal/shared/utils"
)

// tunnelManager is the subset of tunnel.Manager used during registration.
type tunnelManager interface {
	RegisterOwned(conn *websocket.Conn, customSubdomain string, remoteIP string, owner tunnel.Owner) (string, error)
	Get(subdomain string) (*tunnel.Connection, bool)
	Unregister(subdomain string)
}

// RegistrationHandler handles tunnel registration logic.
type RegistrationHandler struct {
	manager      tunnelManager
	portAlloc    *PortAllocator
	groupManager *ConnectionGroupManager
	resolver     *reservations.Resolver
	domain       string
	tunnelDomain string
	publicPort   int
	logger       *zap.Logger
}

// NewRegistrationHandler creates a new registration handler.
func NewRegistrationHandler(
	manager tunnelManager,
	portAlloc *PortAllocator,
	groupManager *ConnectionGroupManager,
	domain, tunnelDomain string,
	publicPort int,
	logger *zap.Logger,
) *RegistrationHandler {
	return &RegistrationHandler{
		manager:      manager,
		portAlloc:    portAlloc,
		groupManager: groupManager,
		domain:       domain,
		tunnelDomain: tunnelDomain,
		publicPort:   publicPort,
		logger:       logger,
	}
}

// RegistrationRequest contains all information needed for registration.
type RegistrationRequest struct {
	TunnelType       protocol.TunnelType
	CustomSubdomain  string
	Token            string
	ConnectionType   string
	PoolCapabilities *protocol.PoolCapabilities
	IPAccess         *protocol.IPAccessControl
	ProxyAuth        *protocol.ProxyAuth
	LocalPort        int
	RemoteIP         string
	// Owner carries the authenticated control-plane identity, if any.
	Owner tunnel.Owner
}

// SetResolver attaches the reservation resolver. Without one every
// registration resolves to an ephemeral tunnel, which is the behavior of a
// server with no control plane database.
func (rh *RegistrationHandler) SetResolver(resolver *reservations.Resolver) {
	rh.resolver = resolver
}

// RegistrationResult contains the result of a registration attempt.
type RegistrationResult struct {
	Subdomain string
	Port      int
	// ReservationID is set when the tunnel bound a reservation.
	ReservationID string
	// Bandwidth is the reservation's per-tunnel override, if it sets one.
	Bandwidth        string
	TunnelURL        string
	TunnelID         string
	SupportsDataConn bool
	RecommendedConns int
	TunnelConn       *tunnel.Connection
}

// Register handles the tunnel registration process.
func (rh *RegistrationHandler) Register(ctx context.Context, req *RegistrationRequest) (*RegistrationResult, error) {
	// Decide what this client is entitled to before touching any resources.
	resolution, err := rh.resolve(ctx, req)
	if err != nil {
		return nil, err
	}
	req.CustomSubdomain = resolution.Subdomain

	// Allocate port for TCP tunnels
	port := 0
	if req.TunnelType == protocol.TunnelTypeTCP {
		if rh.portAlloc == nil {
			return nil, fmt.Errorf("port allocator not configured")
		}

		requestedPort := resolution.TCPPort
		if requestedPort == 0 {
			if parsed, ok := parseTCPSubdomainPort(req.CustomSubdomain); ok {
				requestedPort = parsed
			}
		}

		if requestedPort > 0 {
			allocatedPort, err := rh.portAlloc.AllocateSpecific(requestedPort)
			if err != nil {
				return nil, fmt.Errorf("failed to allocate requested port %d: %w", requestedPort, err)
			}
			port = allocatedPort
		} else {
			allocatedPort, err := rh.portAlloc.Allocate()
			if err != nil {
				return nil, fmt.Errorf("failed to allocate port: %w", err)
			}
			port = allocatedPort

			if req.CustomSubdomain == "" {
				req.CustomSubdomain = fmt.Sprintf("tcp-%d", port)
			}
		}
	}

	// Register with tunnel manager
	subdomain, err := rh.manager.RegisterOwned(nil, req.CustomSubdomain, req.RemoteIP, req.Owner)
	if err != nil {
		if port > 0 && rh.portAlloc != nil {
			rh.portAlloc.Release(port)
		}
		return nil, fmt.Errorf("tunnel registration failed: %w", err)
	}

	releaseRegistration := func() {
		rh.manager.Unregister(subdomain)
		if port > 0 && rh.portAlloc != nil {
			rh.portAlloc.Release(port)
		}
	}

	// Get tunnel connection
	tunnelConn, ok := rh.manager.Get(subdomain)
	if !ok {
		releaseRegistration()
		return nil, fmt.Errorf("failed to get registered tunnel")
	}

	// Configure tunnel
	tunnelConn.SetTunnelType(req.TunnelType)

	if req.IPAccess != nil && (len(req.IPAccess.AllowIPs) > 0 || len(req.IPAccess.DenyIPs) > 0) {
		tunnelConn.SetIPAccessControl(req.IPAccess.AllowIPs, req.IPAccess.DenyIPs)
		rh.logger.Info("IP access control configured",
			zap.String("subdomain", subdomain),
			zap.Strings("allow_ips", req.IPAccess.AllowIPs),
			zap.Strings("deny_ips", req.IPAccess.DenyIPs),
		)
	}

	if req.ProxyAuth != nil && req.ProxyAuth.Enabled {
		tunnelConn.SetProxyAuth(req.ProxyAuth)
		rh.logger.Info("Proxy authentication configured",
			zap.String("subdomain", subdomain),
		)
	}

	// Build tunnel URL
	urlBuilder := utils.NewTunnelURLBuilder(rh.tunnelDomain, rh.publicPort)
	tunnelURL := urlBuilder.BuildURL(subdomain, req.TunnelType, port)

	// Handle connection groups for multi-connection support
	var tunnelID string
	var supportsDataConn bool
	recommendedConns := 0

	if req.PoolCapabilities != nil && req.ConnectionType == "primary" && rh.groupManager != nil {
		// This will be handled by the caller since it needs the connection instance
		supportsDataConn = true
		recommendedConns = 4
	}

	rh.logger.Info("Tunnel registered",
		zap.String("subdomain", subdomain),
		zap.String("tunnel_type", string(req.TunnelType)),
		zap.Int("local_port", req.LocalPort),
		zap.Int("remote_port", port),
		zap.String("client_id", req.Owner.ClientID),
		zap.String("account_id", req.Owner.AccountID),
		zap.String("reservation_id", resolution.ReservationID),
	)

	return &RegistrationResult{
		Subdomain:        subdomain,
		Port:             port,
		ReservationID:    resolution.ReservationID,
		Bandwidth:        resolution.Bandwidth,
		TunnelURL:        tunnelURL,
		TunnelID:         tunnelID,
		SupportsDataConn: supportsDataConn,
		RecommendedConns: recommendedConns,
		TunnelConn:       tunnelConn,
	}, nil
}

// resolve asks the reservation resolver what this registration may bind.
func (rh *RegistrationHandler) resolve(ctx context.Context, req *RegistrationRequest) (*reservations.Resolution, error) {
	if rh.resolver == nil {
		return &reservations.Resolution{Subdomain: req.CustomSubdomain}, nil
	}

	requestedPort := 0
	if req.TunnelType == protocol.TunnelTypeTCP {
		if parsed, ok := parseTCPSubdomainPort(req.CustomSubdomain); ok {
			requestedPort = parsed
		}
	}

	// A reservation is only free if no live tunnel already holds its name.
	isActive := func(subdomain string) bool {
		_, ok := rh.manager.Get(subdomain)
		return ok
	}

	resolution, err := rh.resolver.Resolve(ctx, reservations.Request{
		AccountID:          req.Owner.AccountID,
		ClientID:           req.Owner.ClientID,
		TunnelType:         string(req.TunnelType),
		RequestedSubdomain: req.CustomSubdomain,
		RequestedTCPPort:   requestedPort,
	}, isActive)
	if err != nil {
		return nil, err
	}
	return resolution, nil
}

// BuildRegistrationResponse creates a protocol registration response.
func (rh *RegistrationHandler) BuildRegistrationResponse(result *RegistrationResult) (*protocol.RegisterResponse, error) {
	resp := &protocol.RegisterResponse{
		Subdomain:        result.Subdomain,
		Port:             result.Port,
		URL:              result.TunnelURL,
		Message:          "Tunnel registered successfully",
		TunnelID:         result.TunnelID,
		SupportsDataConn: result.SupportsDataConn,
		RecommendedConns: result.RecommendedConns,
	}
	return resp, nil
}

// SendRegistrationResponse sends the registration response frame.
func (rh *RegistrationHandler) SendRegistrationResponse(conn interface{ Write([]byte) (int, error) }, resp *protocol.RegisterResponse) error {
	respData, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal registration response: %w", err)
	}

	ackFrame := protocol.NewFrame(protocol.FrameTypeRegisterAck, respData)
	return protocol.WriteFrame(conn, ackFrame)
}
