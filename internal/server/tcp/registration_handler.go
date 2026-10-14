package tcp

import (
	"errors"
	"fmt"
	"strings"

	json "github.com/goccy/go-json"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"drip/internal/server/tunnel"
	"drip/internal/shared/netutil"
	"drip/internal/shared/protocol"
	"drip/internal/shared/utils"
)

// tunnelManager is the subset of tunnel.Manager used during registration.
type tunnelManager interface {
	RegisterWithIP(conn *websocket.Conn, customSubdomain string, remoteIP string) (string, error)
	Get(subdomain string) (*tunnel.Connection, bool)
	Unregister(subdomain string)
}

// RegistrationHandler handles tunnel registration logic.
type RegistrationHandler struct {
	manager      tunnelManager
	portAlloc    *PortAllocator
	groupManager *ConnectionGroupManager
	domain       string
	tunnelDomain string
	publicPort   int
	logger       *zap.Logger
}

const (
	publicRegistrationFailureCode    = "registration_failed"
	publicRegistrationFailureMessage = "Unable to register tunnel"
)

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
}

// RegistrationResult contains the result of a registration attempt.
type RegistrationResult struct {
	Subdomain        string
	Port             int
	TunnelURL        string
	TunnelID         string
	SupportsDataConn bool
	RecommendedConns int
	MaxDataConns     int
	TunnelConn       *tunnel.Connection
}

func publicRegistrationError(err error) (string, string) {
	return publicRegistrationFailureCode, publicRegistrationFailureMessage
}

func registrationFailureReason(err error) string {
	switch {
	case err == nil:
		return "unknown"
	case errors.Is(err, tunnel.ErrTooManyTunnels):
		return "max_tunnels"
	case errors.Is(err, tunnel.ErrTooManyPerIP):
		return "max_per_ip"
	case errors.Is(err, tunnel.ErrRateLimitExceeded):
		return "rate_limited"
	case errors.Is(err, tunnel.ErrSubdomainGenerationFailed):
		return "subdomain_generation_failed"
	case errors.Is(err, tunnel.ErrSubdomainTaken):
		return "subdomain_taken"
	case errors.Is(err, tunnel.ErrInvalidSubdomain):
		return "invalid_subdomain"
	case errors.Is(err, tunnel.ErrReservedSubdomain):
		return "reserved_subdomain"
	}

	errText := strings.ToLower(err.Error())
	switch {
	case strings.Contains(errText, "port allocator"):
		return "port_allocator_unavailable"
	case strings.Contains(errText, "allocate requested port") || strings.Contains(errText, "allocate port"):
		return "port_allocation"
	case strings.Contains(errText, "registered tunnel"):
		return "registered_tunnel_lookup"
	default:
		return "unknown"
	}
}

// Register handles the tunnel registration process.
func (rh *RegistrationHandler) Register(req *RegistrationRequest) (*RegistrationResult, error) {
	// Allocate port for TCP tunnels
	port := 0
	if req.TunnelType == protocol.TunnelTypeTCP {
		if rh.portAlloc == nil {
			return nil, fmt.Errorf("port allocator not configured")
		}

		if requestedPort, ok := parseTCPSubdomainPort(req.CustomSubdomain); ok {
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

	hasIPAccessRules := req.IPAccess != nil && (len(req.IPAccess.AllowIPs) > 0 || len(req.IPAccess.DenyIPs) > 0)
	if hasIPAccessRules {
		if err := netutil.ValidateIPAccessRules(req.IPAccess.AllowIPs, req.IPAccess.DenyIPs); err != nil {
			if port > 0 && rh.portAlloc != nil {
				rh.portAlloc.Release(port)
			}
			return nil, fmt.Errorf("invalid IP access rules: %w", err)
		}
	}

	// Register with tunnel manager
	subdomain, err := rh.manager.RegisterWithIP(nil, req.CustomSubdomain, req.RemoteIP)
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

	if hasIPAccessRules {
		if err := tunnelConn.SetIPAccessControl(req.IPAccess.AllowIPs, req.IPAccess.DenyIPs); err != nil {
			rh.manager.Unregister(subdomain)
			if port > 0 && rh.portAlloc != nil {
				rh.portAlloc.Release(port)
			}
			return nil, fmt.Errorf("invalid IP access rules: %w", err)
		}
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
	maxDataConns := 0

	if req.PoolCapabilities != nil && req.ConnectionType == "primary" && rh.groupManager != nil {
		maxDataConns = rh.groupManager.EffectiveMaxDataConns(req.PoolCapabilities.MaxDataConns)
		if maxDataConns > 0 {
			// This will be handled by the caller since it needs the connection instance.
			supportsDataConn = true
			recommendedConns = min(4, maxDataConns)
		}
	}

	rh.logger.Info("Tunnel registered",
		zap.String("subdomain", subdomain),
		zap.String("tunnel_type", string(req.TunnelType)),
		zap.Int("local_port", req.LocalPort),
		zap.Int("remote_port", port),
	)

	return &RegistrationResult{
		Subdomain:        subdomain,
		Port:             port,
		TunnelURL:        tunnelURL,
		TunnelID:         tunnelID,
		SupportsDataConn: supportsDataConn,
		RecommendedConns: recommendedConns,
		MaxDataConns:     maxDataConns,
		TunnelConn:       tunnelConn,
	}, nil
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
