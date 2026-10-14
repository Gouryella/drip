package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"drip/internal/server/tunnel"
	"drip/internal/shared/httputil"
	"drip/internal/shared/netutil"
	"drip/internal/shared/protocol"
	"drip/internal/shared/qos"
	"drip/internal/shared/wsutil"
)

type bufferedReadWriteCloser struct {
	*bufio.Reader
	net.Conn
}

func (b *bufferedReadWriteCloser) Read(p []byte) (int, error) {
	return b.Reader.Read(p)
}

func (h *Handler) handleWebSocket(w http.ResponseWriter, r *http.Request, tconn *tunnel.Connection) {
	stream, err := h.openStreamWithTimeout(tconn)
	if err != nil {
		http.Error(w, "Tunnel unavailable", http.StatusBadGateway)
		return
	}

	tconn.IncActiveConnections()

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = stream.Close()
		tconn.DecActiveConnections()
		http.Error(w, "WebSocket not supported", http.StatusInternalServerError)
		return
	}

	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		_ = stream.Close()
		tconn.DecActiveConnections()
		http.Error(w, "Failed to hijack connection", http.StatusInternalServerError)
		return
	}
	defer stream.Close()
	defer clientConn.Close()
	defer tconn.DecActiveConnections()

	_ = clientConn.SetDeadline(time.Time{})
	_ = stream.SetWriteDeadline(time.Now().Add(defaultForwardWriteTimeout))

	if err := r.Write(stream); err != nil {
		return
	}
	_ = stream.SetWriteDeadline(time.Time{})

	var limitedStream net.Conn = stream
	if limiter := tconn.GetLimiter(); limiter != nil && limiter.IsLimited() {
		if l, ok := limiter.(*qos.Limiter); ok {
			limitedStream = qos.NewLimitedConn(r.Context(), stream, l)
		}
	}

	var clientRW io.ReadWriteCloser = clientConn
	if clientBuf != nil && clientBuf.Reader.Buffered() > 0 {
		clientRW = &bufferedReadWriteCloser{
			Reader: clientBuf.Reader,
			Conn:   clientConn,
		}
	}

	var bytesOut atomic.Int64
	var bytesIn atomic.Int64
	err = netutil.PipeWithCallbacks(r.Context(), limitedStream, clientRW,
		func(n int64) {
			bytesOut.Add(n)
			tconn.AddBytesOut(n)
		},
		func(n int64) {
			bytesIn.Add(n)
			tconn.AddBytesIn(n)
		},
	)
	h.recordProxyTransferResult(r.Context(), proxyTransferWebSocket, bytesOut.Load()+bytesIn.Load(), err)
}

func (h *Handler) handleTunnelWebSocket(w http.ResponseWriter, r *http.Request) {
	if !h.IsTransportAllowed("wss") {
		http.Error(w, "WebSocket transport not allowed on this server", http.StatusForbidden)
		return
	}

	if h.wsConnHandler == nil {
		http.Error(w, "WebSocket tunnel not configured", http.StatusServiceUnavailable)
		return
	}
	if authenticator, ok := h.wsConnHandler.(interface{ AuthenticateWebSocket(string) bool }); ok {
		if !authenticator.AuthenticateWebSocket(extractBearerToken(r.Header.Get("Authorization"))) {
			h.serveBearerAuthRequired(w, "drip")
			return
		}
	}

	ws, err := h.wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("WebSocket upgrade failed", zap.Error(err))
		return
	}

	ws.SetReadLimit(protocol.MaxFrameSize + protocol.FrameHeaderSize + 1024)

	remoteAddr := netutil.ExtractClientIPWithTrustedProxies(r, h.trustedProxies)

	h.logger.Info("WebSocket tunnel connection established",
		zap.String("remote_addr", remoteAddr),
	)

	conn := wsutil.NewConnWithPing(ws, 30*time.Second)

	h.wsConnHandler.HandleWSConnection(conn, remoteAddr)
}

func (h *Handler) isWebSocketUpgrade(r *http.Request) bool {
	return httputil.IsWebSocketUpgrade(r)
}
