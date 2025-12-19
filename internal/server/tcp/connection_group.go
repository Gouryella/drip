package tcp

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"drip/internal/shared/constants"
	"drip/internal/shared/protocol"

	"go.uber.org/zap"
)

type ConnectionGroup struct {
	TunnelID     string
	Subdomain    string
	Token        string
	PrimaryConn  *Connection
	Sessions     map[string]*yamux.Session
	TunnelType   protocol.TunnelType
	RegisteredAt time.Time
	LastActivity time.Time
	sessionIdx   uint32
	mu           sync.RWMutex
	stopCh       chan struct{}
	logger       *zap.Logger

	heartbeatStarted bool
}

func NewConnectionGroup(tunnelID, subdomain, token string, primaryConn *Connection, tunnelType protocol.TunnelType, logger *zap.Logger) *ConnectionGroup {
	return &ConnectionGroup{
		TunnelID:     tunnelID,
		Subdomain:    subdomain,
		Token:        token,
		PrimaryConn:  primaryConn,
		Sessions:     make(map[string]*yamux.Session),
		TunnelType:   tunnelType,
		RegisteredAt: time.Now(),
		LastActivity: time.Now(),
		stopCh:       make(chan struct{}),
		logger:       logger.With(zap.String("tunnel_id", tunnelID)),
	}
}

// StartHeartbeat starts a goroutine that periodically pings all sessions
// and removes dead ones. The caller should ensure this is only called once.
func (g *ConnectionGroup) StartHeartbeat(interval, timeout time.Duration) {
	go g.heartbeatLoop(interval, timeout)
}

func (g *ConnectionGroup) heartbeatLoop(interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	const maxConsecutiveFailures = 3
	failureCount := make(map[string]int)

	for {
		select {
		case <-g.stopCh:
			return
		case <-ticker.C:
		}

		g.mu.RLock()
		sessions := make(map[string]*yamux.Session, len(g.Sessions))
		for id, s := range g.Sessions {
			sessions[id] = s
		}
		g.mu.RUnlock()

		for id, session := range sessions {
			if session == nil || session.IsClosed() {
				g.RemoveSession(id)
				delete(failureCount, id)
				continue
			}

			// Ping with timeout
			done := make(chan error, 1)
			go func(s *yamux.Session) {
				_, err := s.Ping()
				done <- err
			}(session)

			var err error
			select {
			case err = <-done:
			case <-time.After(timeout):
				err = fmt.Errorf("ping timeout")
			case <-g.stopCh:
				return
			}

			if err != nil {
				failureCount[id]++
				g.logger.Debug("Session ping failed",
					zap.String("session_id", id),
					zap.Int("consecutive_failures", failureCount[id]),
					zap.Error(err),
				)

				if failureCount[id] >= maxConsecutiveFailures {
					g.logger.Warn("Session ping failed too many times, removing",
						zap.String("session_id", id),
						zap.Int("failures", failureCount[id]),
					)
					g.RemoveSession(id)
					delete(failureCount, id)
				}
			} else {
				// Reset on success
				failureCount[id] = 0
				g.mu.Lock()
				g.LastActivity = time.Now()
				g.mu.Unlock()
			}
		}

		// Check if all sessions are gone
		g.mu.RLock()
		sessionCount := len(g.Sessions)
		g.mu.RUnlock()

		if sessionCount == 0 {
			g.logger.Info("All sessions closed, tunnel will be cleaned up")
		}
	}
}

func (g *ConnectionGroup) Close() {
	g.mu.Lock()

	select {
	case <-g.stopCh:
		g.mu.Unlock()
		return
	default:
		close(g.stopCh)
	}

	sessions := make([]*yamux.Session, 0, len(g.Sessions))
	for _, session := range g.Sessions {
		if session != nil {
			sessions = append(sessions, session)
		}
	}
	g.Sessions = make(map[string]*yamux.Session)

	g.mu.Unlock()

	for _, session := range sessions {
		_ = session.Close()
	}
}

func (g *ConnectionGroup) IsStale(timeout time.Duration) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return time.Since(g.LastActivity) > timeout
}

func (g *ConnectionGroup) AddSession(connID string, session *yamux.Session) {
	if connID == "" || session == nil {
		return
	}

	g.mu.Lock()
	if g.Sessions == nil {
		g.Sessions = make(map[string]*yamux.Session)
	}
	g.Sessions[connID] = session
	g.LastActivity = time.Now()

	// Start heartbeat on first session
	shouldStartHeartbeat := !g.heartbeatStarted
	if shouldStartHeartbeat {
		g.heartbeatStarted = true
	}
	g.mu.Unlock()

	if shouldStartHeartbeat {
		g.StartHeartbeat(constants.HeartbeatInterval, constants.HeartbeatTimeout)
	}
}

func (g *ConnectionGroup) RemoveSession(connID string) {
	if connID == "" {
		return
	}

	var session *yamux.Session

	g.mu.Lock()
	if g.Sessions != nil {
		session = g.Sessions[connID]
		delete(g.Sessions, connID)
	}
	g.mu.Unlock()

	if session != nil {
		_ = session.Close()
	}
}

func (g *ConnectionGroup) SessionCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.Sessions)
}

func (g *ConnectionGroup) OpenStream() (net.Conn, error) {
	const (
		maxStreamsPerSession = 256
		maxRetries           = 3
		backoffBase          = 25 * time.Millisecond
	)

	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		select {
		case <-g.stopCh:
			return nil, net.ErrClosed
		default:
		}

		// Prefer data sessions for data-plane traffic; keep the primary session
		// as control-plane (client ping/latency), and only fall back to primary
		// when no data session exists.
		sessions := g.sessionsSnapshot(false)
		if len(sessions) == 0 {
			sessions = g.sessionsSnapshot(true)
		}
		if len(sessions) == 0 {
			return nil, net.ErrClosed
		}

		tried := make([]bool, len(sessions))
		anyUnderCap := false
		start := int(atomic.AddUint32(&g.sessionIdx, 1) - 1)

		for range sessions {
			bestIdx := -1
			minStreams := int(^uint(0) >> 1)

			for i := 0; i < len(sessions); i++ {
				idx := (start + i) % len(sessions)
				if tried[idx] {
					continue
				}

				session := sessions[idx]
				if session == nil || session.IsClosed() {
					tried[idx] = true
					continue
				}

				n := session.NumStreams()
				if n >= maxStreamsPerSession {
					continue
				}
				anyUnderCap = true

				if n < minStreams {
					minStreams = n
					bestIdx = idx
				}
			}

			if bestIdx == -1 {
				break
			}

			tried[bestIdx] = true
			session := sessions[bestIdx]
			if session == nil || session.IsClosed() {
				continue
			}

			stream, err := session.Open()
			if err == nil {
				return stream, nil
			}
			lastErr = err

			if session.IsClosed() {
				g.deleteClosedSessions()
			}
		}

		if !anyUnderCap {
			lastErr = fmt.Errorf("all sessions are at stream capacity (%d)", maxStreamsPerSession)
		}

		if attempt < maxRetries-1 {
			select {
			case <-g.stopCh:
				return nil, net.ErrClosed
			case <-time.After(backoffBase * time.Duration(attempt+1)):
			}
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("failed to open stream")
	}
	return nil, lastErr
}

func (g *ConnectionGroup) selectSession() *yamux.Session {
	sessions := g.sessionsSnapshot(false)
	if len(sessions) == 0 {
		sessions = g.sessionsSnapshot(true)
	}
	if len(sessions) == 0 {
		return nil
	}

	start := int(atomic.AddUint32(&g.sessionIdx, 1) - 1)
	minStreams := int(^uint(0) >> 1)
	var best *yamux.Session

	for i := 0; i < len(sessions); i++ {
		session := sessions[(start+i)%len(sessions)]
		if session == nil || session.IsClosed() {
			continue
		}
		if n := session.NumStreams(); n < minStreams {
			minStreams = n
			best = session
		}
	}

	return best
}

func (g *ConnectionGroup) sessionsSnapshot(includePrimary bool) []*yamux.Session {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(g.Sessions) == 0 {
		return nil
	}

	sessions := make([]*yamux.Session, 0, len(g.Sessions))
	for id, session := range g.Sessions {
		if session == nil || session.IsClosed() {
			delete(g.Sessions, id)
			continue
		}
		if id == "primary" && !includePrimary {
			continue
		}
		sessions = append(sessions, session)
	}

	if len(sessions) > 0 {
		g.LastActivity = time.Now()
	}

	return sessions
}

func (g *ConnectionGroup) deleteClosedSessions() {
	g.mu.Lock()
	for id, session := range g.Sessions {
		if session == nil || session.IsClosed() {
			delete(g.Sessions, id)
		}
	}
	g.mu.Unlock()
}
