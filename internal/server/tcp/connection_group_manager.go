package tcp

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"drip/internal/shared/protocol"

	"go.uber.org/zap"
)

// ConnectionGroupManager manages all connection groups
type ConnectionGroupManager struct {
	groups map[string]*ConnectionGroup // TunnelID -> ConnectionGroup
	mu     sync.RWMutex
	logger *zap.Logger

	// Cleanup
	cleanupInterval time.Duration
	staleTimeout    time.Duration
	stopCh          chan struct{}
}

// NewConnectionGroupManager creates a new connection group manager
func NewConnectionGroupManager(logger *zap.Logger) *ConnectionGroupManager {
	m := &ConnectionGroupManager{
		groups:          make(map[string]*ConnectionGroup),
		logger:          logger,
		cleanupInterval: 60 * time.Second,
		staleTimeout:    5 * time.Minute,
		stopCh:          make(chan struct{}),
	}

	go m.cleanupLoop()

	return m
}

// GenerateTunnelID generates a unique tunnel ID
func GenerateTunnelID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateGroup creates a new connection group
func (m *ConnectionGroupManager) CreateGroup(subdomain, token string, primaryConn *Connection, tunnelType protocol.TunnelType) *ConnectionGroup {
	m.mu.Lock()
	defer m.mu.Unlock()

	tunnelID := GenerateTunnelID()

	group := NewConnectionGroup(tunnelID, subdomain, token, primaryConn, tunnelType, m.logger)

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
	// Collect stale groups under lock
	m.mu.Lock()
	var staleGroups []*ConnectionGroup
	var staleIDs []string
	for tunnelID, group := range m.groups {
		if group.IsStale(m.staleTimeout) {
			staleIDs = append(staleIDs, tunnelID)
			staleGroups = append(staleGroups, group)
		}
	}

	// Remove from map while holding lock
	for _, tunnelID := range staleIDs {
		delete(m.groups, tunnelID)
	}
	m.mu.Unlock()

	// Close groups without holding lock to avoid blocking other operations
	for _, group := range staleGroups {
		group.Close()
	}
}

// Close shuts down the manager
func (m *ConnectionGroupManager) Close() {
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
}
