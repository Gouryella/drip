package proxy

import (
	"context"
	"net/http"

	"go.uber.org/zap"

	"drip/internal/server/metrics"
	"drip/internal/shared/utils"
)

const (
	proxyAuthOutcomeFailure = "failure"
	proxyAuthOutcomeLockout = "lockout"

	proxyAuthReasonInvalidPassword         = "invalid_password"
	proxyAuthReasonInvalidToken            = "invalid_token"
	proxyAuthReasonMissingToken            = "missing_token"
	proxyAuthReasonRateLimited             = "rate_limited"
	proxyAuthReasonMissingOrInvalidSession = "missing_or_invalid_session"

	proxyAuthTypeBearer   = "bearer"
	proxyAuthTypePassword = "password"

	proxyTransferHTTPResponse          = "http_response"
	proxyTransferHTTPStreamingResponse = "http_streaming_response"
	proxyTransferWebSocket             = "websocket"
)

func (h *Handler) recordProxyAuthFailure(r *http.Request, authType, reason, subdomain, clientIP string) {
	h.recordProxyAuthEvent(r, authType, proxyAuthOutcomeFailure, reason, subdomain, clientIP)
}

func (h *Handler) recordProxyAuthLockout(r *http.Request, authType, subdomain, clientIP string) {
	h.recordProxyAuthEvent(r, authType, proxyAuthOutcomeLockout, proxyAuthReasonRateLimited, subdomain, clientIP)
}

func (h *Handler) recordProxyAuthEvent(r *http.Request, authType, outcome, reason, subdomain, clientIP string) {
	if authType == "" {
		authType = proxyAuthTypePassword
	}
	if reason == "" {
		reason = "unknown"
	}

	metrics.ProxyAuthEvents.WithLabelValues(authType, outcome, reason).Inc()

	if h.logger == nil {
		return
	}

	path := ""
	queryKeys := []string(nil)
	if r != nil {
		path = utils.URLPathForLog(r.URL)
		queryKeys = utils.QueryKeysForLog(r.URL)
	}

	fields := []zap.Field{
		zap.String("auth_type", authType),
		zap.String("outcome", outcome),
		zap.String("reason", reason),
		zap.String("path", path),
		zap.Strings("query_keys", queryKeys),
		zap.Bool("client_ip_present", clientIP != ""),
		zap.String("client_ip_hash", utils.HashForLog(clientIP)),
		zap.Bool("subdomain_present", subdomain != ""),
		zap.String("subdomain_hash", utils.HashForLog(subdomain)),
	}

	if outcome == proxyAuthOutcomeLockout {
		h.logger.Warn("Proxy authentication lockout", fields...)
		return
	}
	h.logger.Warn("Proxy authentication failed", fields...)
}

func (h *Handler) recordProxyTransferResult(ctx context.Context, operation string, bytesCopied int64, err error) {
	recordProxyTransferResult(h.logger, ctx, operation, bytesCopied, err)
}

func recordProxyTransferResult(logger *zap.Logger, ctx context.Context, operation string, bytesCopied int64, err error) {
	result := utils.ClassifyTransferError(ctx, err)
	metrics.ProxyTransferResults.WithLabelValues(operation, result).Inc()

	if err == nil || logger == nil {
		return
	}

	fields := []zap.Field{
		zap.String("operation", operation),
		zap.String("result", result),
		zap.Int64("bytes_copied", bytesCopied),
		zap.Error(err),
	}
	if utils.IsExpectedTransferResult(result) {
		logger.Debug("Proxy transfer ended", fields...)
		return
	}
	logger.Warn("Proxy transfer failed", fields...)
}
