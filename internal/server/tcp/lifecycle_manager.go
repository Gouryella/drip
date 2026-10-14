package tcp

import (
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"go.uber.org/zap"

	"drip/internal/server/tunnel"
	"drip/internal/shared/protocol"
)

// ConnectionLifecycleManager manages the lifecycle of a connection.
type ConnectionLifecycleManager struct {
	once   sync.Once
	mu     sync.Mutex
	closed bool
	stopCh chan struct{}
	cancel func()
	logger *zap.Logger

	// Resources to clean up
	conn interface {
		Close() error
		SetDeadline(time.Time) error
	}
	frameWriter      *protocol.FrameWriter
	proxy            interface{ Stop() }
	session          *yamux.Session
	portAlloc        *PortAllocator
	port             int
	manager          *tunnel.Manager
	subdomain        string
	tunnelID         string
	groupManager     *ConnectionGroupManager
	registeredTunnel *tunnel.Connection
}

// NewConnectionLifecycleManager creates a new lifecycle manager.
func NewConnectionLifecycleManager(
	stopCh chan struct{},
	cancel func(),
	logger *zap.Logger,
) *ConnectionLifecycleManager {
	return &ConnectionLifecycleManager{
		stopCh: stopCh,
		cancel: cancel,
		logger: logger,
	}
}

// SetConnection sets the connection to manage.
func (clm *ConnectionLifecycleManager) SetConnection(conn interface {
	Close() error
	SetDeadline(time.Time) error
}) {
	clm.mu.Lock()
	defer clm.mu.Unlock()
	if clm.closed {
		_ = conn.Close()
		return
	}
	clm.conn = conn
}

// SetFrameWriter sets the frame writer to close.
func (clm *ConnectionLifecycleManager) SetFrameWriter(fw *protocol.FrameWriter) {
	clm.mu.Lock()
	defer clm.mu.Unlock()
	if clm.closed {
		_ = fw.Close()
		return
	}
	clm.frameWriter = fw
}

// SetProxy sets the proxy to stop.
func (clm *ConnectionLifecycleManager) SetProxy(proxy interface{ Stop() }) {
	clm.mu.Lock()
	defer clm.mu.Unlock()
	if clm.closed {
		proxy.Stop()
		return
	}
	clm.proxy = proxy
}

// SetSession sets the yamux session to close.
func (clm *ConnectionLifecycleManager) SetSession(session *yamux.Session) {
	clm.mu.Lock()
	defer clm.mu.Unlock()
	if clm.closed {
		_ = session.Close()
		return
	}
	clm.session = session
}

// SetPortAllocation sets the port allocation to release.
func (clm *ConnectionLifecycleManager) SetPortAllocation(portAlloc *PortAllocator, port int) {
	clm.mu.Lock()
	defer clm.mu.Unlock()
	if clm.closed {
		if portAlloc != nil && port > 0 {
			portAlloc.Release(port)
		}
		return
	}
	clm.portAlloc = portAlloc
	clm.port = port
}

// SetTunnelRegistration sets the tunnel registration to clean up.
func (clm *ConnectionLifecycleManager) SetTunnelRegistration(
	manager *tunnel.Manager,
	subdomain string,
	tunnelID string,
	groupManager *ConnectionGroupManager,
	registeredTunnel ...*tunnel.Connection,
) {
	clm.mu.Lock()
	defer clm.mu.Unlock()
	if clm.closed {
		if manager != nil && subdomain != "" {
			if len(registeredTunnel) > 0 {
				manager.UnregisterIf(subdomain, registeredTunnel[0])
			}
		}
		if groupManager != nil && tunnelID != "" {
			groupManager.RemoveGroup(tunnelID)
		}
		return
	}
	clm.manager = manager
	clm.subdomain = subdomain
	clm.tunnelID = tunnelID
	clm.groupManager = groupManager
	if len(registeredTunnel) > 0 {
		clm.registeredTunnel = registeredTunnel[0]
	}
}

// Close closes the connection and cleans up all resources.
func (clm *ConnectionLifecycleManager) Close() {
	clm.close(true)
}

// ClosePreservingConnection cleans up lifecycle resources while leaving the
// underlying net.Conn open for a component that has taken ownership of it.
func (clm *ConnectionLifecycleManager) ClosePreservingConnection() {
	clm.close(false)
}

func (clm *ConnectionLifecycleManager) close(closeConn bool) {
	clm.once.Do(func() {
		clm.mu.Lock()
		clm.closed = true
		conn, frameWriter, proxy, session := clm.conn, clm.frameWriter, clm.proxy, clm.session
		portAlloc, port := clm.portAlloc, clm.port
		manager, subdomain := clm.manager, clm.subdomain
		registeredTunnel := clm.registeredTunnel
		tunnelID, groupManager := clm.tunnelID, clm.groupManager
		clm.mu.Unlock()
		protocol.UnregisterConnection()
		close(clm.stopCh)

		if clm.cancel != nil {
			clm.cancel()
		}

		if closeConn && conn != nil {
			_ = conn.SetDeadline(time.Now())
		}

		if frameWriter != nil {
			_ = frameWriter.Close()
		}

		if proxy != nil {
			proxy.Stop()
		}

		if session != nil {
			_ = session.Close()
		}

		if closeConn && conn != nil {
			_ = conn.Close()
		}

		if port > 0 && portAlloc != nil {
			portAlloc.Release(port)
		}

		if subdomain != "" && manager != nil {
			manager.UnregisterIf(subdomain, registeredTunnel)
			if tunnelID != "" && groupManager != nil {
				groupManager.RemoveGroup(tunnelID)
			}
		}

		clm.logger.Info("Connection closed",
			zap.String("subdomain", subdomain),
		)
	})
}
