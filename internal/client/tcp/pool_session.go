package tcp

import (
	"container/heap"
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	json "github.com/goccy/go-json"
	"github.com/hashicorp/yamux"

	"drip/internal/shared/mux"
	"drip/internal/shared/protocol"
)

var dataConnCounter atomic.Uint64

// sessionHandle wraps a yamux session with metadata.
type sessionHandle struct {
	id         string
	conn       net.Conn
	session    *yamux.Session
	active     atomic.Int64
	lastActive atomic.Int64 // unix nanos
	closed     atomic.Bool
	draining   atomic.Bool
}

func (h *sessionHandle) touch() {
	h.lastActive.Store(time.Now().UnixNano())
}

func (h *sessionHandle) lastActiveTime() time.Time {
	n := h.lastActive.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// warmupSessions pre-creates initial sessions in parallel to eliminate cold-start latency.
func (c *PoolClient) warmupSessions() {
	if c.IsClosed() || c.tunnelID == "" {
		return
	}

	c.mu.RLock()
	desired := c.desiredTotal
	c.mu.RUnlock()

	current := c.sessionCount()
	toCreate := desired - current
	if toCreate <= 0 {
		return
	}

	var wg sync.WaitGroup
	for i := 0; i < toCreate; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.addDataSession()
		}()
	}
	wg.Wait()

}

// addDataSession creates a new data session.
func (c *PoolClient) addDataSession() error {
	if err := c.reserveSessionSlot(); err != nil {
		return err
	}
	slotReserved := true
	defer func() {
		if slotReserved {
			c.releaseSessionSlot()
		}
	}()

	if c.tunnelID == "" {
		return fmt.Errorf("server does not support data connections")
	}

	conn, err := c.dialer.DialContext(c.ctx)
	if err != nil {
		return err
	}
	stopHandshake := context.AfterFunc(c.ctx, func() { _ = conn.Close() })
	defer stopHandshake()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))

	if c.closedDuringHandshake(conn) {
		return net.ErrClosed
	}

	connID := fmt.Sprintf("data-%d", dataConnCounter.Add(1))

	req := protocol.DataConnectRequest{
		TunnelID:     c.tunnelID,
		Token:        c.token,
		ConnectionID: connID,
	}

	payload, err := json.Marshal(req)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to marshal data connect request: %w", err)
	}

	if err := protocol.WriteFrame(conn, protocol.NewFrame(protocol.FrameTypeDataConnect, payload)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to send data connect: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	ack, err := protocol.ReadFrame(conn)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to read data connect ack: %w", err)
	}
	defer ack.Release()
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})

	if c.closedDuringHandshake(conn) {
		return net.ErrClosed
	}

	if ack.Type == protocol.FrameTypeError {
		var errMsg protocol.ErrorMessage
		if e := json.Unmarshal(ack.Payload, &errMsg); e == nil {
			_ = conn.Close()
			return fmt.Errorf("data connect error: %s - %s", errMsg.Code, errMsg.Message)
		}
		_ = conn.Close()
		return fmt.Errorf("data connect error")
	}
	if ack.Type != protocol.FrameTypeDataConnectAck {
		_ = conn.Close()
		return fmt.Errorf("unexpected data connect ack frame: %s", ack.Type)
	}

	var resp protocol.DataConnectResponse
	if err := json.Unmarshal(ack.Payload, &resp); err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to parse data connect response: %w", err)
	}
	if !resp.Accepted {
		_ = conn.Close()
		return fmt.Errorf("data connection rejected: %s", resp.Message)
	}

	if c.IsClosed() {
		_ = conn.Close()
		return net.ErrClosed
	}

	yamuxCfg := mux.NewClientConfig()

	session, err := yamux.Server(conn, yamuxCfg)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("failed to init yamux session: %w", err)
	}

	h := &sessionHandle{
		id:      connID,
		conn:    conn,
		session: session,
	}
	h.touch()

	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		_ = session.Close()
		_ = conn.Close()
		return net.ErrClosed
	}
	c.pendingSessions--
	slotReserved = false
	c.dataSessions[connID] = h
	c.wg.Add(3)
	c.mu.Unlock()

	go c.acceptLoop(h, false)

	go c.sessionWatcher(h, false)

	go c.pingLoop(h)

	return nil
}

func (c *PoolClient) reserveSessionSlot() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed.Load() {
		return net.ErrClosed
	}

	select {
	case <-c.stopCh:
		return net.ErrClosed
	default:
	}

	if c.maxSessions > 0 && c.sessionCountLocked() >= c.maxSessions {
		return fmt.Errorf("max sessions reached")
	}

	c.pendingSessions++
	return nil
}

func (c *PoolClient) releaseSessionSlot() {
	c.mu.Lock()
	if c.pendingSessions > 0 {
		c.pendingSessions--
	}
	c.mu.Unlock()
}

func (c *PoolClient) closedDuringHandshake(conn net.Conn) bool {
	if c.closed.Load() {
		_ = conn.Close()
		return true
	}
	select {
	case <-c.stopCh:
		_ = conn.Close()
		return true
	default:
		return false
	}
}

func (c *PoolClient) sessionCountLocked() int {
	count := len(c.dataSessions) + c.pendingSessions
	if c.primary != nil {
		count++
	}
	return count
}

type idleSessionCandidate struct {
	id         string
	lastActive int64
}

type idleSessionHeap []idleSessionCandidate

func (h idleSessionHeap) Len() int           { return len(h) }
func (h idleSessionHeap) Less(i, j int) bool { return h[i].lastActive < h[j].lastActive }
func (h idleSessionHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *idleSessionHeap) Push(value any)    { *h = append(*h, value.(idleSessionCandidate)) }
func (h *idleSessionHeap) Pop() any {
	i := len(*h) - 1
	value := (*h)[i]
	(*h)[i] = idleSessionCandidate{}
	*h = (*h)[:i]
	return value
}

// removeIdleSessions builds one O(n) heap and spends O(log n) per attempted
// retirement, instead of scanning all sessions again for every candidate.
func (c *PoolClient) removeIdleSessions(n int) int {
	if n <= 0 {
		return 0
	}

	c.mu.RLock()
	candidates := make(idleSessionHeap, 0, len(c.dataSessions))
	for id, h := range c.dataSessions {
		if h == nil || h.active.Load() != 0 || h.draining.Load() {
			continue
		}
		candidates = append(candidates, idleSessionCandidate{
			id:         id,
			lastActive: h.lastActive.Load(),
		})
	}
	c.mu.RUnlock()
	heap.Init(&candidates)

	removed := 0
	for removed < n && len(candidates) > 0 {
		best := heap.Pop(&candidates).(idleSessionCandidate)
		if c.retireDataSession(best.id) {
			removed++
		}
	}
	return removed
}

func (c *PoolClient) retireDataSession(id string) bool {
	c.mu.Lock()
	h := c.dataSessions[id]
	if c.closed.Load() || h == nil || h.session == nil || h.active.Load() != 0 || h.session.NumStreams() != 0 || h.draining.Load() {
		c.mu.Unlock()
		return false
	}
	h.draining.Store(true)
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer c.removeDataSession(id)
		// Stop the peer from assigning more streams before waiting for any
		// streams that were already being opened to finish.
		if err := h.session.GoAway(); err != nil {
			return
		}
		if _, err := h.session.Ping(); err != nil {
			return
		}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for h.active.Load() != 0 || h.session.NumStreams() != 0 {
			select {
			case <-c.stopCh:
				return
			case <-ticker.C:
			}
		}
	}()
	return true
}

// removeDataSession removes a data session by ID.
func (c *PoolClient) removeDataSession(id string) bool {
	var h *sessionHandle

	c.mu.Lock()
	h = c.dataSessions[id]
	delete(c.dataSessions, id)
	c.mu.Unlock()

	if h == nil {
		return false
	}

	if !h.closed.CompareAndSwap(false, true) {
		return false
	}

	if h.session != nil {
		_ = h.session.Close()
	}
	if h.conn != nil {
		_ = h.conn.Close()
	}

	return true
}

// sessionCount returns the total number of active sessions.
func (c *PoolClient) sessionCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionCountLocked()
}

// SessionStats holds per-session statistics.
type SessionStats struct {
	ID           string
	IsPrimary    bool
	ActiveCount  int64
	LastActiveAt time.Time
}

// GetSessionStats returns statistics for all sessions.
func (c *PoolClient) GetSessionStats() []SessionStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stats := make([]SessionStats, 0, len(c.dataSessions)+1)

	if c.primary != nil {
		stats = append(stats, SessionStats{
			ID:           c.primary.id,
			IsPrimary:    true,
			ActiveCount:  c.primary.active.Load(),
			LastActiveAt: c.primary.lastActiveTime(),
		})
	}

	for _, h := range c.dataSessions {
		stats = append(stats, SessionStats{
			ID:           h.id,
			IsPrimary:    false,
			ActiveCount:  h.active.Load(),
			LastActiveAt: h.lastActiveTime(),
		})
	}

	return stats
}
