package tcp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"drip/internal/shared/httputil"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"go.uber.org/zap"
)

const (
	// h3AltSvcMaxAge is how long browsers may keep using HTTP/3 for a host
	// after seeing the Alt-Svc advertisement.
	h3AltSvcMaxAge = 86400

	// h3ShutdownTimeout bounds the graceful QUIC shutdown in Listener.Stop.
	h3ShutdownTimeout = 5 * time.Second
)

// http3Server serves the public HTTP handler over QUIC on the same port as the
// TCP listener. It owns the UDP socket because http3.Server does not close a
// caller-provided net.PacketConn.
type http3Server struct {
	server     *http3.Server
	conn       net.PacketConn
	logger     *zap.Logger
	advertised int

	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// newHTTP3Server binds the UDP port matching tcpAddr and prepares an HTTP/3
// server. tlsConfig is cloned by http3.ConfigureTLSConfig, which resets ALPN to
// h3 only; the TCP listener keeps its own h2/http1.1 configuration.
func newHTTP3Server(tcpAddr net.Addr, publicPort int, tlsConfig *tls.Config, handler http.Handler, logger *zap.Logger) (*http3Server, error) {
	if tlsConfig == nil {
		return nil, errors.New("HTTP/3 requires TLS")
	}

	conn, err := net.ListenPacket("udp", tcpAddr.String())
	if err != nil {
		return nil, fmt.Errorf("failed to listen on UDP %s: %w", tcpAddr, err)
	}

	advertised := publicPort
	if advertised <= 0 {
		if udpAddr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			advertised = udpAddr.Port
		}
	}

	return &http3Server{
		conn:       conn,
		logger:     logger,
		advertised: advertised,
		server: &http3.Server{
			Handler:        handler,
			TLSConfig:      http3.ConfigureTLSConfig(tlsConfig),
			QUICConfig:     defaultQUICConfig(),
			MaxHeaderBytes: 1 << 18, // 256KB, matching the TCP HTTP server
			IdleTimeout:    120 * time.Second,
		},
	}, nil
}

func defaultQUICConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:        120 * time.Second,
		MaxIncomingStreams:    1000,
		MaxIncomingUniStreams: 100,
	}
}

// Start begins serving QUIC in the background. Errors are logged; a failure
// here must not take down the HTTP/1.1 and HTTP/2 listener.
func (s *http3Server) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.logger.Info("HTTP/3 (QUIC) listener started",
			zap.String("address", s.conn.LocalAddr().String()),
			zap.Int("alt_svc_port", s.advertised),
		)
		if err := s.server.Serve(s.conn); err != nil &&
			!errors.Is(err, http.ErrServerClosed) &&
			!errors.Is(err, net.ErrClosed) {
			s.logger.Error("HTTP/3 server error", zap.Error(err))
		}
	}()
}

// Close gracefully shuts down the HTTP/3 server within a bounded context and
// then closes the UDP socket. It is safe to call more than once.
func (s *http3Server) Close() error {
	s.closeOnce.Do(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), h3ShutdownTimeout)
		defer cancel()

		if err := s.server.Shutdown(shutdownCtx); err != nil {
			// A grace-period expiry is expected for long-lived requests;
			// Shutdown has already forced the connections closed in that case.
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				s.closeErr = err
			}
			_ = s.server.Close()
		}

		if err := s.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) && s.closeErr == nil {
			s.closeErr = err
		}

		s.wg.Wait()
	})
	return s.closeErr
}

// withAltSvc advertises HTTP/3 on HTTP/1.1 and HTTP/2 responses. The wrapper is
// never installed on the HTTP/3 server itself, and the protocol check keeps it
// correct if that ever changes.
func withAltSvc(next http.Handler, port int) http.Handler {
	altSvc := fmt.Sprintf(`h3=":%d"; ma=%d`, port, h3AltSvcMaxAge)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor < 3 {
			w.Header().Set("Alt-Svc", altSvc)
		}
		next.ServeHTTP(w, r)
	})
}

// newHTTP3Handler rejects connection upgrades before they reach the proxy.
// HTTP/3 cannot Hijack, so WebSocket upgrades must use HTTP/1.1 or HTTP/2.
func newHTTP3Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if httputil.IsWebSocketUpgrade(r) {
			http.Error(w, "WebSocket upgrades are not supported over HTTP/3; use HTTP/1.1 or HTTP/2", http.StatusNotImplemented)
			return
		}
		next.ServeHTTP(w, r)
	})
}
