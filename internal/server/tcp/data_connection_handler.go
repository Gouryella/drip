package tcp

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"time"

	json "github.com/goccy/go-json"
	"github.com/hashicorp/yamux"
	"go.uber.org/zap"

	"drip/internal/shared/mux"
	"drip/internal/shared/protocol"
	"drip/internal/shared/utils"
)

// DataConnectionHandler handles data connection requests for multi-connection support.
type DataConnectionHandler struct {
	conn             net.Conn
	reader           *bufio.Reader
	authToken        string
	allowAnonymous   bool
	groupManager     *ConnectionGroupManager
	stopCh           <-chan struct{}
	logger           *zap.Logger
	onSessionCreated func(*yamux.Session)
	onTunnelIDSet    func(string)
}

// NewDataConnectionHandler creates a new data connection handler.
func NewDataConnectionHandler(
	conn net.Conn,
	reader *bufio.Reader,
	authToken string,
	allowAnonymous bool,
	groupManager *ConnectionGroupManager,
	stopCh <-chan struct{},
	logger *zap.Logger,
) *DataConnectionHandler {
	return &DataConnectionHandler{
		conn:           conn,
		reader:         reader,
		authToken:      authToken,
		allowAnonymous: allowAnonymous,
		groupManager:   groupManager,
		stopCh:         stopCh,
		logger:         logger,
	}
}

// SetSessionCreatedHandler sets the callback for when a session is created.
func (h *DataConnectionHandler) SetSessionCreatedHandler(handler func(*yamux.Session)) {
	h.onSessionCreated = handler
}

// SetTunnelIDHandler sets the callback for when tunnel ID is set.
func (h *DataConnectionHandler) SetTunnelIDHandler(handler func(string)) {
	h.onTunnelIDSet = handler
}

// Handle processes the data connection request.
func (h *DataConnectionHandler) Handle(frame *protocol.Frame) error {
	var req protocol.DataConnectRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		h.rejectDataConnection(req, "invalid_request", "Failed to parse data connect request")
		return fmt.Errorf("failed to parse data connect request: %w", err)
	}

	h.logger.Info("Data connection request received",
		zap.String("tunnel_id", req.TunnelID),
		zap.String("connection_id", req.ConnectionID),
	)

	if !isAuthTokenAccepted(req.Token, h.authToken, h.allowAnonymous) {
		h.sendError(authFailureCode, authFailureMessage)
		return fmt.Errorf("authentication failed for data connection")
	}

	if h.groupManager == nil {
		h.rejectDataConnection(req, "not_supported", "Multi-connection not supported")
		return fmt.Errorf("group manager not available")
	}

	group, ok := h.groupManager.GetGroup(req.TunnelID)
	if !ok || group == nil {
		h.rejectDataConnection(req, "join_failed", "Tunnel not found")
		return fmt.Errorf("tunnel not found: %s", req.TunnelID)
	}

	if !utils.ConstantTimeEqualString(req.Token, group.Token) {
		h.rejectDataConnection(req, authFailureCode, authFailureMessage)
		return fmt.Errorf("authentication failed for data connection")
	}

	if err := group.ReserveDataSession(req.ConnectionID); err != nil {
		code := dataConnectionRejectCode(err)
		h.rejectDataConnection(req, code, err.Error(),
			zap.Int("active_data_conns", group.DataSessionCount()),
			zap.Int("pending_data_conns", group.PendingDataSessionCount()),
			zap.Int("max_data_conns", group.MaxDataConns),
		)
		return fmt.Errorf("data connection rejected: %w", err)
	}
	reservationActive := true
	defer func() {
		if reservationActive {
			group.ReleaseDataSessionReservation(req.ConnectionID)
		}
	}()

	if h.onTunnelIDSet != nil {
		h.onTunnelIDSet(req.TunnelID)
	}

	resp := protocol.DataConnectResponse{
		Accepted:     true,
		ConnectionID: req.ConnectionID,
		Message:      "Data connection accepted",
	}

	respData, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("failed to marshal data connect response: %w", err)
	}
	ackFrame := protocol.NewFrame(protocol.FrameTypeDataConnectAck, respData)

	if err := protocol.WriteFrame(h.conn, ackFrame); err != nil {
		return fmt.Errorf("failed to send data connect ack: %w", err)
	}

	h.logger.Info("Data connection accepted",
		zap.String("tunnel_id", req.TunnelID),
		zap.String("connection_id", req.ConnectionID),
		zap.Int("active_data_conns", group.DataSessionCount()),
		zap.Int("pending_data_conns", group.PendingDataSessionCount()),
		zap.Int("max_data_conns", group.MaxDataConns),
	)

	_ = h.conn.SetReadDeadline(time.Time{})

	// Server acts as yamux Client, client connector acts as yamux Server
	bc := &bufferedConn{
		Conn:   h.conn,
		reader: h.reader,
	}

	// Use optimized mux config for server
	cfg := mux.NewServerConfig()

	session, err := yamux.Client(bc, cfg)
	if err != nil {
		return fmt.Errorf("failed to init yamux session: %w", err)
	}

	if err := group.CommitReservedSession(req.ConnectionID, session); err != nil {
		_ = session.Close()
		return fmt.Errorf("failed to register data connection session: %w", err)
	}
	reservationActive = false

	h.logger.Info("Data connection session registered",
		zap.String("tunnel_id", req.TunnelID),
		zap.String("connection_id", req.ConnectionID),
		zap.Int("active_data_conns", group.DataSessionCount()),
		zap.Int("max_data_conns", group.MaxDataConns),
	)

	if h.onSessionCreated != nil {
		h.onSessionCreated(session)
	}
	defer group.RemoveSession(req.ConnectionID)

	select {
	case <-h.stopCh:
		return nil
	case <-session.CloseChan():
		return nil
	}
}

func dataConnectionRejectCode(err error) string {
	switch {
	case errors.Is(err, ErrDataConnectionLimitExceeded):
		return "connection_limit_exceeded"
	case errors.Is(err, ErrDuplicateDataConnectionID):
		return "duplicate_connection_id"
	case errors.Is(err, ErrReservedDataConnectionID), errors.Is(err, ErrInvalidDataConnectionID):
		return "invalid_connection_id"
	case errors.Is(err, ErrConnectionGroupClosed):
		return "tunnel_closed"
	default:
		return "join_failed"
	}
}

func (h *DataConnectionHandler) rejectDataConnection(req protocol.DataConnectRequest, code, message string, fields ...zap.Field) {
	logFields := []zap.Field{
		zap.String("tunnel_id", req.TunnelID),
		zap.String("connection_id", req.ConnectionID),
		zap.String("reason", code),
		zap.String("message", message),
	}
	logFields = append(logFields, fields...)
	h.logger.Warn("Data connection rejected", logFields...)

	h.sendError(code, message)
	_ = h.conn.Close()
}

// sendError sends an error response to the client.
func (h *DataConnectionHandler) sendError(code, message string) {
	resp := protocol.DataConnectResponse{
		Accepted: false,
		Message:  fmt.Sprintf("%s: %s", code, message),
	}
	respData, err := json.Marshal(resp)
	if err != nil {
		h.logger.Error("Failed to marshal data connect error", zap.Error(err))
		return
	}
	frame := protocol.NewFrame(protocol.FrameTypeDataConnectAck, respData)
	_ = protocol.WriteFrame(h.conn, frame)
}
