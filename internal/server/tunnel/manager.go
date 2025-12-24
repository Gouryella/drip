package tunnel

import (
	"errors"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"

	"drip/internal/shared/utils"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Manager limits
const (
	DefaultMaxTunnels      = 1000            // Maximum total tunnels
	DefaultMaxTunnelsPerIP = 10              // Maximum tunnels per IP
	DefaultRateLimit       = 10              // Registrations per IP per minute
	DefaultRateLimitWindow = 1 * time.Minute // Rate limit window

	// numShards is the number of shards for lock distribution
	// Using 32 shards reduces lock contention by ~32x under high concurrency
	numShards = 32
)

var (
	ErrTooManyTunnels    = errors.New("maximum tunnel limit reached")
	ErrTooManyPerIP      = errors.New("maximum tunnels per IP reached")
	ErrRateLimitExceeded = errors.New("rate limit exceeded, try again later")
)

// rateLimitEntry tracks registration attempts per IP
type rateLimitEntry struct {
	count     int
	windowEnd time.Time
}

// shard holds a subset of tunnels with its own lock
type shard struct {
	tunnels map[string]*Connection
	used    map[string]bool
	mu      sync.RWMutex
}

// Manager manages all active tunnel connections with sharded locking
type Manager struct {
	shards [numShards]shard
	logger *zap.Logger

	// Limits
	maxTunnels      int
	maxTunnelsPerIP int
	rateLimit       int
	rateLimitWindow time.Duration

	// Global counters (atomic for lock-free reads)
	tunnelCount atomic.Int64

	// Per-IP tracking (requires separate lock as it spans shards)
	ipMu        sync.RWMutex
	tunnelsByIP map[string]int             // IP -> tunnel count
	rateLimits  map[string]*rateLimitEntry // IP -> rate limit entry

	// Lifecycle
	stopCh chan struct{}
}

// ManagerConfig holds configuration for the Manager
type ManagerConfig struct {
	MaxTunnels      int
	MaxTunnelsPerIP int
	RateLimit       int           // Registrations per IP per window
	RateLimitWindow time.Duration
}

// DefaultManagerConfig returns default configuration
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		MaxTunnels:      DefaultMaxTunnels,
		MaxTunnelsPerIP: DefaultMaxTunnelsPerIP,
		RateLimit:       DefaultRateLimit,
		RateLimitWindow: DefaultRateLimitWindow,
	}
}

// NewManager creates a new tunnel manager with default config
func NewManager(logger *zap.Logger) *Manager {
	return NewManagerWithConfig(logger, DefaultManagerConfig())
}

// NewManagerWithConfig creates a new tunnel manager with custom config
func NewManagerWithConfig(logger *zap.Logger, cfg ManagerConfig) *Manager {
	if cfg.MaxTunnels <= 0 {
		cfg.MaxTunnels = DefaultMaxTunnels
	}
	if cfg.MaxTunnelsPerIP <= 0 {
		cfg.MaxTunnelsPerIP = DefaultMaxTunnelsPerIP
	}
	if cfg.RateLimit <= 0 {
		cfg.RateLimit = DefaultRateLimit
	}
	if cfg.RateLimitWindow <= 0 {
		cfg.RateLimitWindow = DefaultRateLimitWindow
	}

	logger.Info("Tunnel manager configured",
		zap.Int("max_tunnels", cfg.MaxTunnels),
		zap.Int("max_per_ip", cfg.MaxTunnelsPerIP),
		zap.Int("rate_limit", cfg.RateLimit),
		zap.Duration("rate_window", cfg.RateLimitWindow),
		zap.Int("num_shards", numShards),
	)

	m := &Manager{
		logger:          logger,
		maxTunnels:      cfg.MaxTunnels,
		maxTunnelsPerIP: cfg.MaxTunnelsPerIP,
		rateLimit:       cfg.RateLimit,
		rateLimitWindow: cfg.RateLimitWindow,
		tunnelsByIP:     make(map[string]int),
		rateLimits:      make(map[string]*rateLimitEntry),
		stopCh:          make(chan struct{}),
	}

	// Initialize all shards
	for i := 0; i < numShards; i++ {
		m.shards[i].tunnels = make(map[string]*Connection)
		m.shards[i].used = make(map[string]bool)
	}

	return m
}

// getShard returns the shard for a given subdomain using FNV-1a hash
func (m *Manager) getShard(subdomain string) *shard {
	h := fnv.New32a()
	h.Write([]byte(subdomain))
	return &m.shards[h.Sum32()%numShards]
}

// checkRateLimit checks if the IP has exceeded rate limit (caller must hold ipMu)
func (m *Manager) checkRateLimitLocked(ip string) bool {
	now := time.Now()
	entry, exists := m.rateLimits[ip]

	if !exists || now.After(entry.windowEnd) {
		// New window
		m.rateLimits[ip] = &rateLimitEntry{
			count:     1,
			windowEnd: now.Add(m.rateLimitWindow),
		}
		return true
	}

	if entry.count >= m.rateLimit {
		return false
	}

	entry.count++
	return true
}

// Register registers a new tunnel connection with IP-based limits
func (m *Manager) Register(conn *websocket.Conn, customSubdomain string) (string, error) {
	return m.RegisterWithIP(conn, customSubdomain, "")
}

// RegisterWithIP registers a new tunnel with IP tracking
func (m *Manager) RegisterWithIP(conn *websocket.Conn, customSubdomain string, remoteIP string) (string, error) {
	// Reserve a global slot atomically using CAS loop
	for {
		current := m.tunnelCount.Load()
		if current >= int64(m.maxTunnels) {
			m.logger.Warn("Maximum tunnel limit reached",
				zap.Int64("current", current),
				zap.Int("max", m.maxTunnels),
			)
			return "", ErrTooManyTunnels
		}
		if m.tunnelCount.CompareAndSwap(current, current+1) {
			break
		}
		// CAS failed, another goroutine modified the counter, retry
	}

	// Rollback helper for global counter
	rollbackGlobal := func() {
		m.tunnelCount.Add(-1)
	}

	// Check per-IP limits and reserve slot atomically
	if remoteIP != "" {
		m.ipMu.Lock()
		if !m.checkRateLimitLocked(remoteIP) {
			m.ipMu.Unlock()
			rollbackGlobal()
			m.logger.Warn("Rate limit exceeded",
				zap.String("ip", remoteIP),
				zap.Int("limit", m.rateLimit),
			)
			return "", ErrRateLimitExceeded
		}

		if m.tunnelsByIP[remoteIP] >= m.maxTunnelsPerIP {
			currentPerIP := m.tunnelsByIP[remoteIP]
			m.ipMu.Unlock()
			rollbackGlobal()
			m.logger.Warn("Per-IP tunnel limit reached",
				zap.String("ip", remoteIP),
				zap.Int("current", currentPerIP),
				zap.Int("max", m.maxTunnelsPerIP),
			)
			return "", ErrTooManyPerIP
		}

		// Reserve per-IP slot while still holding the lock
		m.tunnelsByIP[remoteIP]++
		m.ipMu.Unlock()
	}

	// Rollback helper for per-IP counter
	rollbackPerIP := func() {
		if remoteIP != "" {
			m.ipMu.Lock()
			if m.tunnelsByIP[remoteIP] > 0 {
				m.tunnelsByIP[remoteIP]--
				if m.tunnelsByIP[remoteIP] == 0 {
					delete(m.tunnelsByIP, remoteIP)
				}
			}
			m.ipMu.Unlock()
		}
	}

	var subdomain string

	if customSubdomain != "" {
		// Validate custom subdomain
		if !utils.ValidateSubdomain(customSubdomain) {
			rollbackPerIP()
			rollbackGlobal()
			return "", ErrInvalidSubdomain
		}
		if utils.IsReserved(customSubdomain) {
			rollbackPerIP()
			rollbackGlobal()
			return "", ErrReservedSubdomain
		}

		// Check if subdomain is taken in its shard
		s := m.getShard(customSubdomain)
		s.mu.Lock()
		if s.used[customSubdomain] {
			s.mu.Unlock()
			rollbackPerIP()
			rollbackGlobal()
			return "", ErrSubdomainTaken
		}
		subdomain = customSubdomain

		// Register in shard
		tc := NewConnection(subdomain, conn, m.logger)
		tc.remoteIP = remoteIP
		s.tunnels[subdomain] = tc
		s.used[subdomain] = true
		s.mu.Unlock()
	} else {
		// Generate unique random subdomain
		subdomain = m.generateUniqueSubdomain()

		s := m.getShard(subdomain)
		s.mu.Lock()
		tc := NewConnection(subdomain, conn, m.logger)
		tc.remoteIP = remoteIP
		s.tunnels[subdomain] = tc
		s.used[subdomain] = true
		s.mu.Unlock()
	}

	// Get connection and start write pump
	s := m.getShard(subdomain)
	s.mu.RLock()
	tc := s.tunnels[subdomain]
	s.mu.RUnlock()
	if tc != nil {
		go tc.StartWritePump()
	}

	m.logger.Info("Tunnel registered",
		zap.String("subdomain", subdomain),
		zap.String("ip", remoteIP),
		zap.Int64("total_tunnels", m.tunnelCount.Load()),
	)

	return subdomain, nil
}

// Unregister removes a tunnel connection
func (m *Manager) Unregister(subdomain string) {
	s := m.getShard(subdomain)
	s.mu.Lock()

	tc, ok := s.tunnels[subdomain]
	if !ok {
		s.mu.Unlock()
		return
	}

	remoteIP := tc.remoteIP
	tc.Close()
	delete(s.tunnels, subdomain)
	delete(s.used, subdomain)
	s.mu.Unlock()

	// Update counters
	m.tunnelCount.Add(-1)
	if remoteIP != "" {
		m.ipMu.Lock()
		if m.tunnelsByIP[remoteIP] > 0 {
			m.tunnelsByIP[remoteIP]--
			if m.tunnelsByIP[remoteIP] == 0 {
				delete(m.tunnelsByIP, remoteIP)
			}
		}
		m.ipMu.Unlock()
	}

	m.logger.Info("Tunnel unregistered",
		zap.String("subdomain", subdomain),
		zap.Int64("total_tunnels", m.tunnelCount.Load()),
	)
}

// Get retrieves a tunnel connection by subdomain
func (m *Manager) Get(subdomain string) (*Connection, bool) {
	s := m.getShard(subdomain)
	s.mu.RLock()
	tc, ok := s.tunnels[subdomain]
	s.mu.RUnlock()
	return tc, ok
}

// List returns all active tunnel connections
func (m *Manager) List() []*Connection {
	// Pre-allocate with approximate capacity
	connections := make([]*Connection, 0, m.tunnelCount.Load())

	for i := 0; i < numShards; i++ {
		s := &m.shards[i]
		s.mu.RLock()
		for _, tc := range s.tunnels {
			connections = append(connections, tc)
		}
		s.mu.RUnlock()
	}

	return connections
}

// Count returns the number of active tunnels
func (m *Manager) Count() int {
	return int(m.tunnelCount.Load())
}

// CleanupStale removes stale connections that haven't been active
func (m *Manager) CleanupStale(timeout time.Duration) int {
	totalCleaned := 0

	// Clean up each shard independently
	for i := 0; i < numShards; i++ {
		s := &m.shards[i]
		s.mu.Lock()

		var staleSubdomains []string
		for subdomain, tc := range s.tunnels {
			if !tc.IsAlive(timeout) {
				staleSubdomains = append(staleSubdomains, subdomain)
			}
		}

		for _, subdomain := range staleSubdomains {
			if tc, ok := s.tunnels[subdomain]; ok {
				remoteIP := tc.remoteIP
				tc.Close()
				delete(s.tunnels, subdomain)
				delete(s.used, subdomain)

				// Update counters
				m.tunnelCount.Add(-1)
				if remoteIP != "" {
					m.ipMu.Lock()
					if m.tunnelsByIP[remoteIP] > 0 {
						m.tunnelsByIP[remoteIP]--
						if m.tunnelsByIP[remoteIP] == 0 {
							delete(m.tunnelsByIP, remoteIP)
						}
					}
					m.ipMu.Unlock()
				}
			}
		}
		totalCleaned += len(staleSubdomains)
		s.mu.Unlock()
	}

	// Cleanup expired rate limit entries
	m.ipMu.Lock()
	now := time.Now()
	for ip, entry := range m.rateLimits {
		if now.After(entry.windowEnd) {
			delete(m.rateLimits, ip)
		}
	}
	m.ipMu.Unlock()

	if totalCleaned > 0 {
		m.logger.Info("Cleaned up stale tunnels",
			zap.Int("count", totalCleaned),
		)
	}

	return totalCleaned
}

// StartCleanupTask starts a background task to clean up stale connections
func (m *Manager) StartCleanupTask(interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.CleanupStale(timeout)
			case <-m.stopCh:
				return
			}
		}
	}()
}

// generateUniqueSubdomain generates a unique random subdomain
func (m *Manager) generateUniqueSubdomain() string {
	const maxAttempts = 10

	for i := 0; i < maxAttempts; i++ {
		subdomain := utils.GenerateSubdomain(6)
		if utils.IsReserved(subdomain) {
			continue
		}

		s := m.getShard(subdomain)
		s.mu.RLock()
		taken := s.used[subdomain]
		s.mu.RUnlock()

		if !taken {
			return subdomain
		}
	}

	// Fallback: use longer subdomain if collision persists
	return utils.GenerateSubdomain(8)
}

// Shutdown gracefully shuts down all tunnels
func (m *Manager) Shutdown() {
	// Signal cleanup goroutine to stop
	close(m.stopCh)

	m.logger.Info("Shutting down tunnel manager",
		zap.Int64("active_tunnels", m.tunnelCount.Load()),
	)

	// Close all tunnels in each shard
	for i := 0; i < numShards; i++ {
		s := &m.shards[i]
		s.mu.Lock()
		for _, tc := range s.tunnels {
			tc.Close()
		}
		s.tunnels = make(map[string]*Connection)
		s.used = make(map[string]bool)
		s.mu.Unlock()
	}

	m.tunnelCount.Store(0)
}
