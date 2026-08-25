package tunnel

import (
	"errors"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"

	"drip/internal/server/metrics"
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
	ErrTooManyTunnels            = errors.New("maximum tunnel limit reached")
	ErrTooManyPerIP              = errors.New("maximum tunnels per IP reached")
	ErrRateLimitExceeded         = errors.New("rate limit exceeded, try again later")
	ErrSubdomainGenerationFailed = errors.New("failed to generate unique subdomain")
)

// Owner identifies the control-plane client and account behind a registration.
// A zero Owner means the registration is unauthenticated (anonymous server) or
// authenticated with the legacy shared token, neither of which has an identity
// in the database.
type Owner struct {
	ClientID  string
	AccountID string
	// MaxTunnels caps concurrent tunnels for the account. 0 means unlimited.
	MaxTunnels int
}

// IsIdentified reports whether the owner came from the control plane.
func (o Owner) IsIdentified() bool { return o.ClientID != "" }

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

	// Global counters (atomic for lock-free reads)
	tunnelCount atomic.Int64

	// Per-IP tracking (requires separate lock as it spans shards)
	ipMu        sync.RWMutex
	tunnelsByIP map[string]int // IP -> tunnel count

	// Per-account tracking for authenticated clients
	accountMu        sync.RWMutex
	tunnelsByAccount map[string]int // account ID -> tunnel count

	// Rate limiting
	rateLimiter *RateLimiter

	// Lifecycle
	stopCh       chan struct{}
	shutdownOnce sync.Once
}

// ManagerConfig holds configuration for the Manager
type ManagerConfig struct {
	MaxTunnels      int
	MaxTunnelsPerIP int
	RateLimit       int // Registrations per IP per window
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
		logger:           logger,
		maxTunnels:       cfg.MaxTunnels,
		maxTunnelsPerIP:  cfg.MaxTunnelsPerIP,
		tunnelsByIP:      make(map[string]int),
		tunnelsByAccount: make(map[string]int),
		rateLimiter:      NewRateLimiter(cfg.RateLimit, cfg.RateLimitWindow, logger),
		stopCh:           make(chan struct{}),
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
	_, _ = h.Write([]byte(subdomain))
	return &m.shards[h.Sum32()%numShards]
}

// Register registers a new tunnel connection with IP-based limits
func (m *Manager) Register(conn *websocket.Conn, customSubdomain string) (string, error) {
	return m.RegisterWithIP(conn, customSubdomain, "")
}

// RegisterWithIP registers a new tunnel with IP tracking and no owner identity.
func (m *Manager) RegisterWithIP(conn *websocket.Conn, customSubdomain string, remoteIP string) (string, error) {
	return m.RegisterOwned(conn, customSubdomain, remoteIP, Owner{})
}

// RegisterOwned registers a new tunnel on behalf of a control-plane identity.
//
// Authenticated registrations are exempt from the per-IP tunnel cap and the
// per-IP registration rate limit: those exist to stop anonymous abuse, and they
// would otherwise punish a whole fleet of legitimate clients sharing one NAT
// egress address. Authenticated clients are bounded by their account limit and
// by the global tunnel cap instead.
func (m *Manager) RegisterOwned(conn *websocket.Conn, customSubdomain string, remoteIP string, owner Owner) (string, error) {
	// Reserve a global slot atomically using CAS loop
	for {
		current := m.tunnelCount.Load()
		if current >= int64(m.maxTunnels) {
			m.logger.Warn("Maximum tunnel limit reached",
				zap.Int64("current", current),
				zap.Int("max", m.maxTunnels),
			)
			metrics.TunnelRegistrationFailures.WithLabelValues("max_tunnels").Inc()
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

	rateLimitConsumed := false
	rollbackRateLimit := func() {
		if rateLimitConsumed && remoteIP != "" {
			m.rateLimiter.Decrement(remoteIP)
			rateLimitConsumed = false
		}
	}

	rollbackPerIP := func() {
		if remoteIP != "" && !owner.IsIdentified() {
			m.ipMu.Lock()
			if m.tunnelsByIP[remoteIP] > 0 {
				m.tunnelsByIP[remoteIP]--
				if m.tunnelsByIP[remoteIP] == 0 {
					delete(m.tunnelsByIP, remoteIP)
					metrics.TunnelsByIP.DeleteLabelValues(remoteIP)
				} else {
					metrics.TunnelsByIP.WithLabelValues(remoteIP).Set(float64(m.tunnelsByIP[remoteIP]))
				}
			}
			m.ipMu.Unlock()
		}
	}

	rollbackPerAccount := func() {
		if owner.AccountID != "" {
			m.accountMu.Lock()
			if m.tunnelsByAccount[owner.AccountID] > 0 {
				m.tunnelsByAccount[owner.AccountID]--
				if m.tunnelsByAccount[owner.AccountID] == 0 {
					delete(m.tunnelsByAccount, owner.AccountID)
				}
			}
			m.accountMu.Unlock()
		}
	}

	// Reserve the account slot for authenticated registrations.
	if owner.AccountID != "" {
		m.accountMu.Lock()
		if owner.MaxTunnels > 0 && m.tunnelsByAccount[owner.AccountID] >= owner.MaxTunnels {
			current := m.tunnelsByAccount[owner.AccountID]
			m.accountMu.Unlock()
			rollbackGlobal()
			m.logger.Warn("Per-account tunnel limit reached",
				zap.String("account_id", owner.AccountID),
				zap.Int("current", current),
				zap.Int("max", owner.MaxTunnels),
			)
			metrics.TunnelRegistrationFailures.WithLabelValues("max_per_account").Inc()
			return "", ErrTooManyForAccount
		}
		m.tunnelsByAccount[owner.AccountID]++
		m.accountMu.Unlock()
	}

	// Check per-IP limits and reserve slot atomically
	if remoteIP != "" && !owner.IsIdentified() {
		// Check rate limit first (has its own lock)
		if !m.rateLimiter.CheckAndIncrement(remoteIP) {
			rollbackPerAccount()
			rollbackGlobal()
			metrics.TunnelRegistrationFailures.WithLabelValues("rate_limit").Inc()
			return "", ErrRateLimitExceeded
		}
		rateLimitConsumed = true

		m.ipMu.Lock()
		if m.tunnelsByIP[remoteIP] >= m.maxTunnelsPerIP {
			currentPerIP := m.tunnelsByIP[remoteIP]
			m.ipMu.Unlock()
			rollbackRateLimit()
			rollbackPerAccount()
			rollbackGlobal()
			m.logger.Warn("Per-IP tunnel limit reached",
				zap.String("ip", remoteIP),
				zap.Int("current", currentPerIP),
				zap.Int("max", m.maxTunnelsPerIP),
			)
			metrics.TunnelRegistrationFailures.WithLabelValues("max_per_ip").Inc()
			return "", ErrTooManyPerIP
		}

		// Reserve per-IP slot while still holding the lock
		m.tunnelsByIP[remoteIP]++
		metrics.TunnelsByIP.WithLabelValues(remoteIP).Set(float64(m.tunnelsByIP[remoteIP]))
		m.ipMu.Unlock()
	}

	var subdomain string

	registerSubdomain := func(candidate string) bool {
		s := m.getShard(candidate)
		s.mu.Lock()
		defer s.mu.Unlock()

		if s.used[candidate] {
			return false
		}

		tc := NewConnection(candidate, conn, m.logger)
		tc.remoteIP = remoteIP
		tc.clientID = owner.ClientID
		tc.accountID = owner.AccountID
		s.tunnels[candidate] = tc
		s.used[candidate] = true
		subdomain = candidate
		return true
	}

	if customSubdomain != "" {
		// Validate custom subdomain
		if !utils.ValidateSubdomain(customSubdomain) {
			rollbackRateLimit()
			rollbackPerIP()
			rollbackPerAccount()
			rollbackGlobal()
			return "", ErrInvalidSubdomain
		}
		if utils.IsReserved(customSubdomain) {
			rollbackRateLimit()
			rollbackPerIP()
			rollbackPerAccount()
			rollbackGlobal()
			return "", ErrReservedSubdomain
		}

		if !registerSubdomain(customSubdomain) {
			rollbackRateLimit()
			rollbackPerIP()
			rollbackPerAccount()
			rollbackGlobal()
			return "", ErrSubdomainTaken
		}
	} else {
		const maxAttempts = 32
		registered := false

		for i := 0; i < maxAttempts; i++ {
			candidate := utils.GenerateSubdomain(6)
			if utils.IsReserved(candidate) {
				continue
			}
			if registerSubdomain(candidate) {
				registered = true
				break
			}
		}

		if !registered {
			for i := 0; i < maxAttempts; i++ {
				candidate := utils.GenerateSubdomain(8)
				if utils.IsReserved(candidate) {
					continue
				}
				if registerSubdomain(candidate) {
					registered = true
					break
				}
			}
		}

		if !registered {
			rollbackRateLimit()
			rollbackPerIP()
			rollbackPerAccount()
			rollbackGlobal()
			return "", ErrSubdomainGenerationFailed
		}
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

	// Update Prometheus metrics
	metrics.TunnelRegistrations.Inc()
	metrics.TunnelCount.Set(float64(m.tunnelCount.Load()))

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
	_, accountID := tc.Owner()
	tunnelType := tc.GetTunnelType().String()
	tc.Close()
	delete(s.tunnels, subdomain)
	delete(s.used, subdomain)
	s.mu.Unlock()

	// Clean up per-tunnel Prometheus labels to prevent cardinality explosion
	metrics.TunnelBytesReceived.DeleteLabelValues(subdomain, subdomain, tunnelType)
	metrics.TunnelBytesSent.DeleteLabelValues(subdomain, subdomain, tunnelType)
	metrics.TunnelActiveConnections.DeleteLabelValues(subdomain, subdomain, tunnelType)

	// Update counters
	m.tunnelCount.Add(-1)
	if accountID != "" {
		m.accountMu.Lock()
		if m.tunnelsByAccount[accountID] > 0 {
			m.tunnelsByAccount[accountID]--
			if m.tunnelsByAccount[accountID] == 0 {
				delete(m.tunnelsByAccount, accountID)
			}
		}
		m.accountMu.Unlock()
	}
	if remoteIP != "" {
		m.ipMu.Lock()
		if m.tunnelsByIP[remoteIP] > 0 {
			m.tunnelsByIP[remoteIP]--
			if m.tunnelsByIP[remoteIP] == 0 {
				delete(m.tunnelsByIP, remoteIP)
				metrics.TunnelsByIP.DeleteLabelValues(remoteIP)
			} else {
				metrics.TunnelsByIP.WithLabelValues(remoteIP).Set(float64(m.tunnelsByIP[remoteIP]))
			}
		}
		m.ipMu.Unlock()
	}

	m.logger.Info("Tunnel unregistered",
		zap.String("subdomain", subdomain),
		zap.Int64("total_tunnels", m.tunnelCount.Load()),
	)

	// Update Prometheus metrics
	metrics.TunnelCount.Set(float64(m.tunnelCount.Load()))
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

	// Collect stale subdomains under read lock, then unregister outside lock
	// to avoid lock ordering issues (shard.mu -> ipMu vs RegisterWithIP's ipMu -> shard.mu)
	for i := 0; i < numShards; i++ {
		s := &m.shards[i]
		s.mu.RLock()

		var staleSubdomains []string
		for subdomain, tc := range s.tunnels {
			if !tc.IsAlive(timeout) {
				staleSubdomains = append(staleSubdomains, subdomain)
			}
		}
		s.mu.RUnlock()

		// Unregister outside shard lock — Unregister handles its own locking safely
		for _, subdomain := range staleSubdomains {
			m.Unregister(subdomain)
		}
		totalCleaned += len(staleSubdomains)
	}

	// Cleanup expired rate limit entries
	m.rateLimiter.Cleanup()

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

// Shutdown gracefully shuts down all tunnels
func (m *Manager) Shutdown() {
	m.shutdownOnce.Do(func() {
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

		m.accountMu.Lock()
		m.tunnelsByAccount = make(map[string]int)
		m.accountMu.Unlock()

		m.tunnelCount.Store(0)
	})
}
