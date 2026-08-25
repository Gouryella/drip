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
	stopCh chan struct{}
	cancel func()
	logger *zap.Logger

	// mu guards the resource fields below: they are populated by the connection
	// goroutine as registration progresses, while Close may run concurrently
	// from the listener's shutdown path.
	mu sync.Mutex

	// Resources to clean up
	conn interface {
		Close() error
		SetDeadline(time.Time) error
	}
	frameWriter  *protocol.FrameWriter
	proxy        interface{ Stop() }
	session      *yamux.Session
	portAlloc    *PortAllocator
	port         int
	manager      *tunnel.Manager
	subdomain    string
	tunnelID     string
	groupManager *ConnectionGroupManager
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
	clm.conn = conn
}

// SetFrameWriter sets the frame writer to close.
func (clm *ConnectionLifecycleManager) SetFrameWriter(fw *protocol.FrameWriter) {
	clm.mu.Lock()
	defer clm.mu.Unlock()
	clm.frameWriter = fw
}

// SetProxy sets the proxy to stop.
func (clm *ConnectionLifecycleManager) SetProxy(proxy interface{ Stop() }) {
	clm.mu.Lock()
	defer clm.mu.Unlock()
	clm.proxy = proxy
}

// SetSession sets the yamux session to close.
func (clm *ConnectionLifecycleManager) SetSession(session *yamux.Session) {
	clm.mu.Lock()
	defer clm.mu.Unlock()
	clm.session = session
}

// SetPortAllocation sets the port allocation to release.
func (clm *ConnectionLifecycleManager) SetPortAllocation(portAlloc *PortAllocator, port int) {
	clm.mu.Lock()
	defer clm.mu.Unlock()
	clm.portAlloc = portAlloc
	clm.port = port
}

// SetTunnelRegistration sets the tunnel registration to clean up.
func (clm *ConnectionLifecycleManager) SetTunnelRegistration(
	manager *tunnel.Manager,
	subdomain string,
	tunnelID string,
	groupManager *ConnectionGroupManager,
) {
	clm.mu.Lock()
	defer clm.mu.Unlock()
	clm.manager = manager
	clm.subdomain = subdomain
	clm.tunnelID = tunnelID
	clm.groupManager = groupManager
}

// Close closes the connection and cleans up all resources.
func (clm *ConnectionLifecycleManager) Close() {
	clm.once.Do(func() {
		protocol.UnregisterConnection()
		close(clm.stopCh)

		if clm.cancel != nil {
			clm.cancel()
		}

		// Snapshot the resources, then release the lock: closing a session or
		// unregistering a tunnel takes other locks and must not run under mu.
		clm.mu.Lock()
		conn := clm.conn
		frameWriter := clm.frameWriter
		proxy := clm.proxy
		session := clm.session
		portAlloc := clm.portAlloc
		port := clm.port
		manager := clm.manager
		subdomain := clm.subdomain
		tunnelID := clm.tunnelID
		groupManager := clm.groupManager
		clm.mu.Unlock()

		if conn != nil {
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

		if conn != nil {
			_ = conn.Close()
		}

		if port > 0 && portAlloc != nil {
			portAlloc.Release(port)
		}

		if subdomain != "" && manager != nil {
			manager.Unregister(subdomain)
			if tunnelID != "" && groupManager != nil {
				groupManager.RemoveGroup(tunnelID)
			}
		}

		clm.logger.Info("Connection closed",
			zap.String("subdomain", subdomain),
		)
	})
}
