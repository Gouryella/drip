package mux

import (
	"net"
	"time"

	"github.com/hashicorp/yamux"
)

// Stream gives yamux streams net.Conn close semantics. Yamux's Close only
// sends FIN; readers otherwise remain blocked until the peer also closes.
type Stream struct{ *yamux.Stream }

func (s *Stream) Close() error {
	_ = s.Stream.SetDeadline(time.Now())
	return s.Stream.Close()
}

func (s *Stream) CloseWrite() error { return s.Stream.Close() }

func WrapStream(conn net.Conn) net.Conn {
	if stream, ok := conn.(*yamux.Stream); ok {
		return &Stream{Stream: stream}
	}
	return conn
}

func OpenStream(session *yamux.Session) (net.Conn, error) {
	conn, err := session.Open()
	if err != nil {
		return nil, err
	}
	return WrapStream(conn), nil
}
