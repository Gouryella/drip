package mux

import "net"

// WrapStream gives stream-like connections the tunnel's close semantics. The
// in-house stream already implements CloseWrite and wakes blocked readers on
// Close, so this is an identity function kept for call-site compatibility.
func WrapStream(conn net.Conn) net.Conn { return conn }

// OpenStream opens a stream and returns it as a net.Conn.
func OpenStream(session *Session) (net.Conn, error) {
	st, err := session.Open()
	if err != nil {
		return nil, err
	}
	return st, nil
}
