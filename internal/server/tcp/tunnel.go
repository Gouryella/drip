package tcp

import (
	"bufio"
	"fmt"
	"io"
	"net"

	"github.com/hashicorp/yamux"

	"drip/internal/shared/constants"
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

	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	cfg.AcceptBacklog = constants.YamuxAcceptBacklog

	session, err := yamux.Client(bc, cfg)
	if err != nil {
		return fmt.Errorf("failed to init yamux session: %w", err)
	}
	c.session = session

	openStream := session.Open
	if c.groupManager != nil {
		if group, ok := c.groupManager.GetGroup(c.tunnelID); ok && group != nil {
			group.AddSession("primary", session)
			openStream = group.OpenStream
		}
	}

	c.proxy = NewProxy(c.ctx, c.port, c.subdomain, openStream, c.tunnelConn, c.logger)
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
	// Public server acts as yamux Client, client connector acts as yamux Server.
	bc := &bufferedConn{
		Conn:   c.conn,
		reader: reader,
	}

	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	cfg.AcceptBacklog = constants.YamuxAcceptBacklog

	session, err := yamux.Client(bc, cfg)
	if err != nil {
		return fmt.Errorf("failed to init yamux session: %w", err)
	}
	c.session = session

	openStream := session.Open
	if c.groupManager != nil {
		if group, ok := c.groupManager.GetGroup(c.tunnelID); ok && group != nil {
			group.AddSession("primary", session)
			openStream = group.OpenStream
		}
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
