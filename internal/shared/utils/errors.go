package utils

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
)

const (
	TransferResultOK               = "ok"
	TransferResultEOF              = "eof"
	TransferResultCanceled         = "canceled"
	TransferResultClientDisconnect = "client_disconnect"
	TransferResultError            = "error"
)

// IsNetworkError checks if an error message indicates a common network error
// that should be handled gracefully (not logged as severe errors).
func IsNetworkError(errStr string) bool {
	return strings.Contains(errStr, "EOF") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "websocket: close")
}

// IsProtocolError checks if an error message indicates a protocol-level error
// (invalid requests, malformed data, etc.).
func IsProtocolError(errStr string) bool {
	return strings.Contains(errStr, "payload too large") ||
		strings.Contains(errStr, "failed to read registration frame") ||
		strings.Contains(errStr, "expected register frame") ||
		strings.Contains(errStr, "failed to parse registration request") ||
		strings.Contains(errStr, "failed to parse HTTP request") ||
		strings.Contains(errStr, "tunnel type not allowed")
}

// ContainsAny checks if a string contains any of the given substrings.
func ContainsAny(s string, substrings ...string) bool {
	for _, substr := range substrings {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// ClassifyTransferError maps copy/pipe outcomes into low-cardinality buckets
// suitable for logs and metrics.
func ClassifyTransferError(ctx context.Context, err error) string {
	if err == nil {
		return TransferResultOK
	}
	if ctx != nil && ctx.Err() != nil {
		return TransferResultCanceled
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return TransferResultCanceled
	}
	if errors.Is(err, io.EOF) {
		return TransferResultEOF
	}
	if errors.Is(err, net.ErrClosed) {
		return TransferResultClientDisconnect
	}
	if IsNetworkError(err.Error()) {
		return TransferResultClientDisconnect
	}
	return TransferResultError
}

func IsExpectedTransferResult(result string) bool {
	return result == TransferResultOK ||
		result == TransferResultEOF ||
		result == TransferResultCanceled ||
		result == TransferResultClientDisconnect
}
