package tcp

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"drip/internal/shared/constants"
	"drip/internal/shared/mux"
	"drip/internal/shared/protocol"

	"go.uber.org/zap"
)

const primarySessionID = "primary"

var (
	ErrInvalidDataConnectionID       = errors.New("invalid data connection id")
	ErrReservedDataConnectionID      = errors.New("reserved data connection id")
	ErrDuplicateDataConnectionID     = errors.New("duplicate data connection id")
	ErrDataConnectionLimitExceeded   = errors.New("data connection limit exceeded")
	ErrDataConnectionReservationLost = errors.New("data connection reservation lost")
	ErrConnectionGroupClosed         = errors.New("connection group closed")
)

type ConnectionGroup struct {
	TunnelID     string
	Subdomain    string
	Token        string
	PrimaryConn  *Connection
	Sessions     map[string]*yamux.Session
	MaxDataConns int
	TunnelType   protocol.TunnelType
	RegisteredAt time.Time
	LastActivity time.Time
	mu           sync.RWMutex
	stopCh       chan struct{}
	logger       *zap.Logger

	pendingDataSessions map[string]struct{}

	heartbeatStarted bool
}

func NewConnectionGroup(tunnelID, subdomain, token string, primaryConn *Connection, tunnelType protocol.TunnelType, logger *zap.Logger) *ConnectionGroup {
	return NewConnectionGroupWithMaxDataConns(tunnelID, subdomain, token, primaryConn, tunnelType, DefaultMaxDataConns, logger)
}

func NewConnectionGroupWithMaxDataConns(tunnelID, subdomain, token string, primaryConn *Connection, tunnelType protocol.TunnelType, maxDataConns int, logger *zap.Logger) *ConnectionGroup {
	return &ConnectionGroup{
		TunnelID:            tunnelID,
		Subdomain:           subdomain,
		Token:               token,
		PrimaryConn:         primaryConn,
		Sessions:            make(map[string]*yamux.Session),
		MaxDataConns:        maxDataConns,
		TunnelType:          tunnelType,
		RegisteredAt:        time.Now(),
		LastActivity:        time.Now(),
		stopCh:              make(chan struct{}),
		logger:              logger.With(zap.String("tunnel_id", tunnelID)),
		pendingDataSessions: make(map[string]struct{}),
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
	pingInFlight := make(map[string]bool)
	// pendingDone tracks Ping results that outlived their wait timeout so we
	// never start overlapping pings (and leak goroutines) on a wedged session.
	pendingDone := make(map[string]<-chan error)

	type sessionSnapshot struct {
		id      string
		session *yamux.Session
	}
	sessions := make([]sessionSnapshot, 0, 16)

	handlePingResult := func(id string, err error) {
		if err != nil {
			failureCount[id]++
			g.logger.Debug("Session ping failed",
				zap.String("session_id", id),
				zap.Int("consecutive_failures", failureCount[id]),
				zap.Error(err),
			)

			if failureCount[id] >= maxConsecutiveFailures {
				if id == "primary" {
					g.logger.Warn("Primary session ping failed repeatedly, keeping session alive",
						zap.String("session_id", id),
						zap.Int("failures", failureCount[id]),
					)
					failureCount[id] = 0
				} else {
					g.logger.Warn("Session ping failed too many times, removing",
						zap.String("session_id", id),
						zap.Int("failures", failureCount[id]),
					)
					g.RemoveSession(id)
					delete(failureCount, id)
					delete(pingInFlight, id)
					delete(pendingDone, id)
				}
			}
			return
		}

		failureCount[id] = 0
		g.mu.Lock()
		g.LastActivity = time.Now()
		g.mu.Unlock()
	}

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
				delete(pingInFlight, snap.id)
				delete(pendingDone, snap.id)
				continue
			}

			if ch, ok := pendingDone[snap.id]; ok {
				select {
				case err := <-ch:
					delete(pendingDone, snap.id)
					delete(pingInFlight, snap.id)
					// Timeout already counted as a failure; only apply a late
					// success so we can clear the failure streak.
					if err == nil {
						handlePingResult(snap.id, nil)
					}
				default:
					// Previous ping still running; do not start another.
				}
				continue
			}

			if pingInFlight[snap.id] {
				continue
			}
			pingInFlight[snap.id] = true

			done := make(chan error, 1)
			go func(s *yamux.Session) {
				_, err := s.Ping()
				done <- err
			}(snap.session)

			var err error
			select {
			case err = <-done:
				delete(pingInFlight, snap.id)
				handlePingResult(snap.id, err)
			case <-time.After(timeout):
				// Keep pingInFlight set and wait for the real completion on a
				// later tick so overlapping Ping goroutines cannot accumulate.
				pendingDone[snap.id] = done
				handlePingResult(snap.id, fmt.Errorf("ping timeout"))
			case <-g.stopCh:
				delete(pingInFlight, snap.id)
				delete(pendingDone, snap.id)
				return
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
	g.pendingDataSessions = make(map[string]struct{})

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

func validateDataConnectionID(connID string) error {
	if strings.TrimSpace(connID) == "" {
		return fmt.Errorf("%w: empty connection id", ErrInvalidDataConnectionID)
	}
	if connID == primarySessionID {
		return fmt.Errorf("%w: %q is reserved", ErrReservedDataConnectionID, primarySessionID)
	}
	return nil
}

func (g *ConnectionGroup) ReserveDataSession(connID string) error {
	if err := validateDataConnectionID(connID); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.Sessions == nil {
		g.Sessions = make(map[string]*yamux.Session)
	}
	if g.pendingDataSessions == nil {
		g.pendingDataSessions = make(map[string]struct{})
	}
	select {
	case <-g.stopCh:
		return ErrConnectionGroupClosed
	default:
	}

	if _, exists := g.Sessions[connID]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateDataConnectionID, connID)
	}
	if _, exists := g.pendingDataSessions[connID]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateDataConnectionID, connID)
	}

	activeDataConns := g.dataSessionCountLocked()
	pendingDataConns := len(g.pendingDataSessions)
	if g.MaxDataConns <= 0 || activeDataConns+pendingDataConns >= g.MaxDataConns {
		return fmt.Errorf("%w: active=%d pending=%d max=%d",
			ErrDataConnectionLimitExceeded,
			activeDataConns,
			pendingDataConns,
			g.MaxDataConns,
		)
	}

	g.pendingDataSessions[connID] = struct{}{}
	g.LastActivity = time.Now()
	return nil
}

func (g *ConnectionGroup) CommitReservedSession(connID string, session *yamux.Session) error {
	if err := validateDataConnectionID(connID); err != nil {
		return err
	}
	if session == nil {
		return fmt.Errorf("%w: nil yamux session", ErrInvalidDataConnectionID)
	}

	g.mu.Lock()
	if g.Sessions == nil {
		g.Sessions = make(map[string]*yamux.Session)
	}
	if g.pendingDataSessions == nil {
		g.pendingDataSessions = make(map[string]struct{})
	}
	select {
	case <-g.stopCh:
		g.mu.Unlock()
		return ErrConnectionGroupClosed
	default:
	}

	if _, reserved := g.pendingDataSessions[connID]; !reserved {
		g.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrDataConnectionReservationLost, connID)
	}
	delete(g.pendingDataSessions, connID)

	if existing := g.Sessions[connID]; existing != nil && existing != session {
		g.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrDuplicateDataConnectionID, connID)
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
	return nil
}

func (g *ConnectionGroup) ReleaseDataSessionReservation(connID string) {
	if connID == "" {
		return
	}

	g.mu.Lock()
	if g.pendingDataSessions != nil {
		delete(g.pendingDataSessions, connID)
	}
	g.mu.Unlock()
}

func (g *ConnectionGroup) AddSession(connID string, session *yamux.Session) {
	if connID == "" || session == nil {
		return
	}

	var oldSession *yamux.Session

	g.mu.Lock()
	select {
	case <-g.stopCh:
		g.mu.Unlock()
		_ = session.Close()
		return
	default:
	}
	if g.Sessions == nil {
		g.Sessions = make(map[string]*yamux.Session)
	}
	if existing := g.Sessions[connID]; existing != nil && existing != session {
		oldSession = existing
	}
	g.Sessions[connID] = session
	g.LastActivity = time.Now()

	// Start heartbeat on first session
	shouldStartHeartbeat := !g.heartbeatStarted
	if shouldStartHeartbeat {
		g.heartbeatStarted = true
	}
	g.mu.Unlock()

	if oldSession != nil {
		_ = oldSession.Close()
	}

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

func (g *ConnectionGroup) DataSessionCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.dataSessionCountLocked()
}

func (g *ConnectionGroup) PendingDataSessionCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.pendingDataSessions)
}

func (g *ConnectionGroup) dataSessionCountLocked() int {
	count := 0
	for id, session := range g.Sessions {
		if id == primarySessionID || session == nil || session.IsClosed() {
			continue
		}
		count++
	}
	return count
}

const maxStreamsPerSession = 256

// OpenStream selects the least-loaded data session, falling back to the
// primary when all data sessions are unavailable. Scanning once avoids building
// and allocating an entire heap for every request.
func (g *ConnectionGroup) OpenStream() (net.Conn, error) {
	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		select {
		case <-g.stopCh:
			return nil, net.ErrClosed
		default:
		}

		stream, err := g.openStreamAttempt()
		if err == nil || err == net.ErrClosed {
			return stream, err

		}
		lastErr = err
		if attempt < maxRetries-1 {
			timer := time.NewTimer(5 * time.Millisecond * time.Duration(attempt+1))
			select {
			case <-g.stopCh:
				timer.Stop()
				return nil, net.ErrClosed
			case <-timer.C:
			}
		}
	}
	return nil, lastErr
}

// openStreamAttempt keeps the normal path O(n) with no candidate allocation.
// After a failed open, sort one snapshot instead of rescanning for each failure.
// A retry round therefore costs O(n log n) in the worst case, excluding I/O.
func (g *ConnectionGroup) openStreamAttempt() (net.Conn, error) {
	session, hasSessions := g.pickSession()
	if session == nil {
		if !hasSessions {
			return nil, net.ErrClosed
		}
		return nil, fmt.Errorf("all sessions are at stream capacity (%d)", maxStreamsPerSession)
	}
	stream, lastErr := mux.OpenStream(session)
	if lastErr == nil {
		return stream, nil
	}

	for _, candidate := range g.fallbackSessions(session) {
		select {
		case <-g.stopCh:
			return nil, net.ErrClosed
		default:
		}
		if candidate.session.IsClosed() || candidate.session.NumStreams() >= maxStreamsPerSession {
			continue
		}
		stream, err := mux.OpenStream(candidate.session)
		if err == nil {
			return stream, nil
		}
		lastErr = err
	}
	// One cleanup scan per round, including sessions closed during an open.
	g.deleteClosedSessions()
	return nil, lastErr
}

type streamCandidate struct {
	session *yamux.Session
	streams int
	primary bool
}

func (g *ConnectionGroup) fallbackSessions(exclude *yamux.Session) []streamCandidate {
	g.mu.RLock()
	candidates := make([]streamCandidate, 0, len(g.Sessions))
	for id, session := range g.Sessions {
		if session == nil || session == exclude || session.IsClosed() {
			continue
		}
		candidates = append(candidates, streamCandidate{session: session, streams: session.NumStreams(), primary: id == primarySessionID})
	}
	g.mu.RUnlock()
	slices.SortFunc(candidates, func(a, b streamCandidate) int {
		if a.primary != b.primary {
			if a.primary {
				return 1
			}
			return -1
		}
		return cmp.Compare(a.streams, b.streams)
	})
	return candidates
}

func (g *ConnectionGroup) pickSession() (*yamux.Session, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var best, primary *yamux.Session
	bestStreams := maxStreamsPerSession
	hasSessions := false
	for id, session := range g.Sessions {
		if session == nil || session.IsClosed() {
			continue
		}
		hasSessions = true
		streams := session.NumStreams()
		if streams >= maxStreamsPerSession {
			continue
		}
		if id == primarySessionID {
			primary = session
		} else if best == nil || streams < bestStreams {
			best, bestStreams = session, streams
		}
	}
	if best != nil {
		return best, hasSessions
	}
	return primary, hasSessions
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
