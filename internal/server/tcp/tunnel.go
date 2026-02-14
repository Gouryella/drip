package tcp

import (
	"bufio"
	"fmt"
	"net"

	"github.com/hashicorp/yamux"

	"drip/internal/shared/mux"
)

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// initMuxSession creates a yamux session over the buffered connection and
// returns the openStream function (possibly group-aware).
func (c *Connection) initMuxSession(reader *bufio.Reader) (func() (net.Conn, error), *yamux.Session, error) {
	bc := &bufferedConn{
		Conn:   c.conn,
		reader: reader,
	}

	session, err := yamux.Client(bc, mux.NewServerConfig())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to init yamux session: %w", err)
	}
	c.session = session

	openStream := session.Open
	if c.groupManager != nil {
		if group, ok := c.groupManager.GetGroup(c.tunnelID); ok && group != nil {
			group.AddSession("primary", session)
			openStream = group.OpenStream
		}
	}

	return openStream, session, nil
}

func (c *Connection) handleTCPTunnel(reader *bufio.Reader) error {
	openStream, session, err := c.initMuxSession(reader)
	if err != nil {
		return err
	}

	c.proxy = NewProxy(c.ctx, c.port, c.subdomain, openStream, c.tunnelConn, c.logger)
	if c.tunnelConn != nil && c.tunnelConn.HasIPAccessControl() {
		c.proxy.SetIPAccessCheck(c.tunnelConn.IsIPAllowed)
	}
	if c.tunnelConn != nil {
		c.proxy.SetLimiter(c.tunnelConn.GetLimiter())
	}

	if err := c.proxy.Start(); err != nil {
		return fmt.Errorf("failed to start tcp proxy: %w", err)
	}

	select {
	case <-c.stopCh:
		return nil
	case <-session.CloseChan():
		return nil
	}
}

func (c *Connection) handleHTTPProxyTunnel(reader *bufio.Reader) error {
	openStream, session, err := c.initMuxSession(reader)
	if err != nil {
		return err
	}

	if c.tunnelConn != nil {
		c.tunnelConn.SetOpenStream(openStream)
	}

	select {
	case <-c.stopCh:
		return nil
	case <-session.CloseChan():
		return nil
	}
}
