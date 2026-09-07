package netutil

import "net"

type CountingConn struct {
	net.Conn
	OnRead  func(int64)
	OnWrite func(int64)
}

func NewCountingConn(conn net.Conn, onRead, onWrite func(int64)) *CountingConn {
	return &CountingConn{
		Conn:    conn,
		OnRead:  onRead,
		OnWrite: onWrite,
	}
}

func (c *CountingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.OnRead != nil {
		c.OnRead(int64(n))
	}
	return n, err
}

func (c *CountingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 && c.OnWrite != nil {
		c.OnWrite(int64(n))
	}
	return n, err
}

func (c *CountingConn) CloseRead() error {
	if conn, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return conn.CloseRead()
	}
	return nil
}

func (c *CountingConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return c.Conn.Close()
}
