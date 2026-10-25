package protocol

import (
	"fmt"
	"net"

	"encoding/json"

	"go.uber.org/zap"
)

// ErrorSender handles sending error frames over connections.
type ErrorSender struct {
	conn        net.Conn
	frameWriter *FrameWriter
	logger      *zap.Logger
}

// NewErrorSender creates a new error sender.
func NewErrorSender(conn net.Conn, frameWriter *FrameWriter, logger *zap.Logger) *ErrorSender {
	return &ErrorSender{
		conn:        conn,
		frameWriter: frameWriter,
		logger:      logger,
	}
}

// SendError sends an error frame with the given code and message.
func (e *ErrorSender) SendError(code, message string) error {
	errMsg := ErrorMessage{
		Code:    code,
		Message: message,
	}

	data, err := json.Marshal(errMsg)
	if err != nil {
		e.logger.Error("Failed to marshal error message", zap.Error(err))
		return fmt.Errorf("failed to marshal error: %w", err)
	}

	errFrame := NewFrame(FrameTypeError, data)

	if e.frameWriter == nil {
		return WriteFrame(e.conn, errFrame)
	}

	return e.frameWriter.WriteFrame(errFrame)
}

// SendAuthenticationError sends an authentication failed error.
func (e *ErrorSender) SendAuthenticationError() error {
	return e.SendError("authentication_failed", "Invalid authentication token")
}

// SendRegistrationError sends a registration failed error.
func (e *ErrorSender) SendRegistrationError(message string) error {
	return e.SendError("registration_failed", message)
}

// SendPortAllocationError sends a port allocation failed error.
func (e *ErrorSender) SendPortAllocationError(message string) error {
	return e.SendError("port_allocation_failed", message)
}

// SendTunnelTypeNotAllowedError sends a tunnel type not allowed error.
func (e *ErrorSender) SendTunnelTypeNotAllowedError(tunnelType string) error {
	return e.SendError("tunnel_type_not_allowed",
		fmt.Sprintf("Tunnel type '%s' is not allowed on this server", tunnelType))
}
