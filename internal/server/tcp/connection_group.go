package tcp

import (
	"container/heap"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"drip/internal/shared/constants"
	"drip/internal/shared/protocol"

	"go.uber.org/zap"
)

// sessionEntry represents a session with its current stream count for heap operations
type sessionEntry struct {
	id       string
	session  *yamux.Session
	streams  int
	heapIdx  int // index in the heap, managed by heap.Interface
}

// sessionHeap implements heap.Interface for O(log n) session selection
type sessionHeap []*sessionEntry

func (h sessionHeap) Len() int { return len(h) }

func (h sessionHeap) Less(i, j int) bool {
	// Min-heap: session with fewer streams has higher priority
	return h[i].streams < h[j].streams
}

func (h sessionHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].heapIdx = i
	h[j].heapIdx = j
}

func (h *sessionHeap) Push(x interface{}) {
	entry := x.(*sessionEntry)
	entry.heapIdx = len(*h)
	*h = append(*h, entry)
}

func (h *sessionHeap) Pop() interface{} {
	old := *h
	n := len(old)
	entry := old[n-1]
	old[n-1] = nil // avoid memory leak
	entry.heapIdx = -1
	*h = old[0 : n-1]
	return entry
}

// sessionHeapPool reuses heap slices to reduce allocations
var sessionHeapPool = sync.Pool{
	New: func() interface{} {
		h := make(sessionHeap, 0, 16)
		return &h
	},
}

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

	type sessionSnapshot struct {
		id      string
		session *yamux.Session
	}
	sessions := make([]sessionSnapshot, 0, 16)

	for {
		select {
		case <-g.stopCh:
			return
		case <-ticker.C:
		}

		sessions = sessions[:0]
		g.mu.RLock()
		for id, s := range g.Sessions {
			sessions = append(sessions, sessionSnapshot{id: id, session: s})
		}
		g.mu.RUnlock()

		for _, snap := range sessions {
			if snap.session == nil || snap.session.IsClosed() {
				g.RemoveSession(snap.id)
				delete(failureCount, snap.id)
				continue
			}

			done := make(chan error, 1)
			go func(s *yamux.Session) {
				_, err := s.Ping()
				done <- err
			}(snap.session)

			var err error
			select {
			case err = <-done:
			case <-time.After(timeout):
				err = fmt.Errorf("ping timeout")
			case <-g.stopCh:
				return
			}

			if err != nil {
				failureCount[snap.id]++
				g.logger.Debug("Session ping failed",
					zap.String("session_id", snap.id),
					zap.Int("consecutive_failures", failureCount[snap.id]),
					zap.Error(err),
				)

				if failureCount[snap.id] >= maxConsecutiveFailures {
					g.logger.Warn("Session ping failed too many times, removing",
						zap.String("session_id", snap.id),
						zap.Int("failures", failureCount[snap.id]),
					)
					g.RemoveSession(snap.id)
					delete(failureCount, snap.id)
				}
			} else {
				failureCount[snap.id] = 0
				g.mu.Lock()
				g.LastActivity = time.Now()
				g.mu.Unlock()
			}
		}

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

// OpenStream opens a new stream using a min-heap for O(log n) session selection.
func (g *ConnectionGroup) OpenStream() (net.Conn, error) {
	const (
		maxStreamsPerSession = 256
		maxRetries           = 3
		backoffBase          = 5 * time.Millisecond
	)

	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		select {
		case <-g.stopCh:
			return nil, net.ErrClosed
		default:
		}

		h := g.buildSessionHeap(false)
		if h.Len() == 0 {
			h = g.buildSessionHeap(true)
		}
		if h.Len() == 0 {
			return nil, net.ErrClosed
		}

		anyUnderCap := false
		for h.Len() > 0 {
			entry := heap.Pop(h).(*sessionEntry)
			session := entry.session

			if session == nil || session.IsClosed() {
				continue
			}

			currentStreams := session.NumStreams()
			if currentStreams >= maxStreamsPerSession {
				continue
			}
			anyUnderCap = true

			stream, err := session.Open()
			if err == nil {
				*h = (*h)[:0]
				sessionHeapPool.Put(h)
				return stream, nil
			}
			lastErr = err

			if session.IsClosed() {
				g.deleteClosedSessions()
			}
		}

		*h = (*h)[:0]
		sessionHeapPool.Put(h)

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

// buildSessionHeap creates a min-heap of sessions ordered by stream count.
func (g *ConnectionGroup) buildSessionHeap(includePrimary bool) *sessionHeap {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if len(g.Sessions) == 0 {
		h := sessionHeapPool.Get().(*sessionHeap)
		return h
	}

	h := sessionHeapPool.Get().(*sessionHeap)
	*h = (*h)[:0]

	for id, session := range g.Sessions {
		if session == nil || session.IsClosed() {
			continue
		}
		if id == "primary" && !includePrimary {
			continue
		}

		*h = append(*h, &sessionEntry{
			id:      id,
			session: session,
			streams: session.NumStreams(),
		})
	}

	heap.Init(h)
	return h
}

func (g *ConnectionGroup) selectSession() *yamux.Session {
	h := g.buildSessionHeap(false)
	if h.Len() == 0 {
		sessionHeapPool.Put(h)
		h = g.buildSessionHeap(true)
	}
	if h.Len() == 0 {
		sessionHeapPool.Put(h)
		return nil
	}

	entry := heap.Pop(h).(*sessionEntry)
	session := entry.session

	*h = (*h)[:0]
	sessionHeapPool.Put(h)

	if session == nil || session.IsClosed() {
		return nil
	}
	return session
}

func (g *ConnectionGroup) sessionsSnapshot(includePrimary bool) []*yamux.Session {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if len(g.Sessions) == 0 {
		return nil
	}

	sessions := make([]*yamux.Session, 0, len(g.Sessions))
	for id, session := range g.Sessions {
		if session == nil || session.IsClosed() {
			continue
		}
		if id == "primary" && !includePrimary {
			continue
		}
		sessions = append(sessions, session)
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
