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

func (c *Connection) handleTCPTunnel(reader *bufio.Reader) error {
	// Public server acts as yamux Client, client connector acts as yamux Server.
	bc := &bufferedConn{
		Conn:   c.conn,
		reader: reader,
	}

	// Use optimized mux config for server
	cfg := mux.NewServerConfig()

	session, err := yamux.Client(bc, cfg)
	if err != nil {
		return fmt.Errorf("failed to init yamux session: %w", err)
	}
	c.session = session

	// Update lifecycle manager with session
	if c.lifecycleManager != nil {
		c.lifecycleManager.SetSession(session)
	}

	openStream := func() (net.Conn, error) { return mux.OpenStream(session) }
	if c.groupManager != nil {
		if group, ok := c.groupManager.GetGroup(c.tunnelID); ok && group != nil {
			group.AddSession("primary", session)
			openStream = group.OpenStream
		}
	}

	c.proxy = NewProxy(c.ctx, c.port, c.subdomain, openStream, c.tunnelConn, c.logger)
	if c.tunnelConn != nil && c.tunnelConn.HasIPAccessControl() {
		c.proxy.SetIPAccessCheck(c.tunnelConn.IsIPAllowed)
	}
	if c.tunnelConn != nil {
		c.proxy.SetLimiter(c.tunnelConn.GetLimiter())
	}

	// Update lifecycle manager with proxy
	if c.lifecycleManager != nil {
		c.lifecycleManager.SetProxy(c.proxy)
	}

	if err := c.proxy.StartWithListener(c.portAlloc.TakeListener(c.port)); err != nil {
		return fmt.Errorf("failed to start tcp proxy: %w", err)
	}
	c.markReady()

	select {
	case <-c.stopCh:
		return nil
	case <-session.CloseChan():
		return nil
	}
}

func (c *Connection) handleHTTPProxyTunnel(reader *bufio.Reader) error {
	// Public server acts as yamux Client, client connector acts as yamux Server.
	bc := &bufferedConn{
		Conn:   c.conn,
		reader: reader,
	}

	// Use optimized mux config for server
	cfg := mux.NewServerConfig()

	session, err := yamux.Client(bc, cfg)
	if err != nil {
		return fmt.Errorf("failed to init yamux session: %w", err)
	}
	c.session = session

	// Update lifecycle manager with session
	if c.lifecycleManager != nil {
		c.lifecycleManager.SetSession(session)
	}

	openStream := func() (net.Conn, error) { return mux.OpenStream(session) }
	if c.groupManager != nil {
		if group, ok := c.groupManager.GetGroup(c.tunnelID); ok && group != nil {
			group.AddSession("primary", session)
			openStream = group.OpenStream
		}
	}

	if c.tunnelConn != nil {
		c.tunnelConn.SetOpenStream(openStream)
	}
	c.markReady()

	select {
	case <-c.stopCh:
		return nil
	case <-session.CloseChan():
		return nil
	}
}
