package tcp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"drip/internal/shared/protocol"

	"go.uber.org/zap"
)

const DefaultMaxDataConns = 64

// ConnectionGroupManager manages all connection groups
type ConnectionGroupManager struct {
	groups map[string]*ConnectionGroup // TunnelID -> ConnectionGroup
	mu     sync.RWMutex
	logger *zap.Logger

	maxDataConns int

	// Cleanup
	cleanupInterval time.Duration
	staleTimeout    time.Duration
	stopCh          chan struct{}
	closeOnce       sync.Once
}

// NewConnectionGroupManager creates a new connection group manager
func NewConnectionGroupManager(logger *zap.Logger) *ConnectionGroupManager {
	return NewConnectionGroupManagerWithMaxDataConns(logger, DefaultMaxDataConns)
}

// NewConnectionGroupManagerWithMaxDataConns creates a new connection group manager
// with a server-enforced per-tunnel data connection cap.
func NewConnectionGroupManagerWithMaxDataConns(logger *zap.Logger, maxDataConns int) *ConnectionGroupManager {
	if maxDataConns <= 0 {
		maxDataConns = DefaultMaxDataConns
	}

	m := &ConnectionGroupManager{
		groups:          make(map[string]*ConnectionGroup),
		logger:          logger,
		maxDataConns:    maxDataConns,
		cleanupInterval: 60 * time.Second,
		staleTimeout:    5 * time.Minute,
		stopCh:          make(chan struct{}),
	}

	go m.cleanupLoop()

	return m
}

func (m *ConnectionGroupManager) MaxDataConns() int {
	if m == nil || m.maxDataConns <= 0 {
		return DefaultMaxDataConns
	}
	return m.maxDataConns
}

func (m *ConnectionGroupManager) EffectiveMaxDataConns(clientMaxDataConns int) int {
	if clientMaxDataConns <= 0 {
		return 0
	}

	serverMaxDataConns := m.MaxDataConns()
	if clientMaxDataConns < serverMaxDataConns {
		return clientMaxDataConns
	}
	return serverMaxDataConns
}

// GenerateTunnelID generates a unique tunnel ID
func GenerateTunnelID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// CreateGroup creates a new connection group
func (m *ConnectionGroupManager) CreateGroup(subdomain, token string, primaryConn *Connection, tunnelType protocol.TunnelType) *ConnectionGroup {
	return m.CreateGroupWithMaxDataConns(subdomain, token, primaryConn, tunnelType, m.MaxDataConns())
}

// CreateGroupWithMaxDataConns creates a new connection group with an effective
// per-tunnel data connection cap.
func (m *ConnectionGroupManager) CreateGroupWithMaxDataConns(subdomain, token string, primaryConn *Connection, tunnelType protocol.TunnelType, maxDataConns int) *ConnectionGroup {
	m.mu.Lock()
	defer m.mu.Unlock()

	tunnelID := GenerateTunnelID()

	group := NewConnectionGroupWithMaxDataConns(tunnelID, subdomain, token, primaryConn, tunnelType, maxDataConns, m.logger)

	m.groups[tunnelID] = group

	return group
}

// GetGroup returns a connection group by tunnel ID
func (m *ConnectionGroupManager) GetGroup(tunnelID string) (*ConnectionGroup, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	group, ok := m.groups[tunnelID]
	return group, ok
}

// RemoveGroup removes and closes a connection group
func (m *ConnectionGroupManager) RemoveGroup(tunnelID string) {
	m.mu.Lock()
	group, ok := m.groups[tunnelID]
	if ok {
		delete(m.groups, tunnelID)
	}
	m.mu.Unlock()

	if ok && group != nil {
		group.Close()
	}
}

// cleanupLoop periodically cleans up stale groups
func (m *ConnectionGroupManager) cleanupLoop() {
	ticker := time.NewTicker(m.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.cleanupStaleGroups()
		case <-m.stopCh:
			return
		}
	}
}

func (m *ConnectionGroupManager) cleanupStaleGroups() {
	m.mu.Lock()
	var staleGroups []*ConnectionGroup
	for _, group := range m.groups {
		if group.IsStale(m.staleTimeout) {
			staleGroups = append(staleGroups, group)
		}
	}
	m.mu.Unlock()

	for _, group := range staleGroups {
		if group.PrimaryConn != nil {
			group.PrimaryConn.Close()
			// PrimaryConn lifecycle may call RemoveGroup, but only when the group is
			// still in the map. Always close the group explicitly so heartbeat and
			// data sessions are torn down even if the map entry was already removed.
			group.Close()
			m.removeGroupEntry(group.TunnelID)
			continue
		}
		m.RemoveGroup(group.TunnelID)
	}
}

func (m *ConnectionGroupManager) removeGroupEntry(tunnelID string) {
	m.mu.Lock()
	delete(m.groups, tunnelID)
	m.mu.Unlock()
}

// Close shuts down the manager
func (m *ConnectionGroupManager) Close() {
	m.closeOnce.Do(func() {
		close(m.stopCh)

		// Collect all groups under lock
		m.mu.Lock()
		groups := make([]*ConnectionGroup, 0, len(m.groups))
		for _, group := range m.groups {
			groups = append(groups, group)
		}
		m.groups = make(map[string]*ConnectionGroup)
		m.mu.Unlock()

		// Close groups without holding lock
		for _, group := range groups {
			group.Close()
		}
	})
}
