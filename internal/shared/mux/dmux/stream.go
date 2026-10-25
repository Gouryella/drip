package dmux

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type chunk struct {
	buf []byte
	off int
}

// Stream is a bidirectional byte stream multiplexed over a Session. It
// implements net.Conn plus CloseWrite for half-close.
type Stream struct {
	sess *Session
	id   uint32

	synSent      atomic.Bool
	localClosed  atomic.Bool
	remoteClosed atomic.Bool
	reset        atomic.Bool

	// Receive side.
	rmu           sync.Mutex
	chunks        []chunk
	rerr          error
	pendingWindow int64
	rsignal       chan struct{}
	rdeadline     deadline

	// Send side. wmu serializes writers to preserve per-stream frame order;
	// windowMu only protects the credit counter so the reader can replenish
	// it while a writer is blocked.
	wmu        sync.Mutex
	windowMu   sync.Mutex
	sendWindow int64
	wsignal    chan struct{}
	wdeadline  deadline
}

func newStream(s *Session, id uint32, local bool) *Stream {
	st := &Stream{
		sess:       s,
		id:         id,
		rsignal:    make(chan struct{}, 1),
		wsignal:    make(chan struct{}, 1),
		sendWindow: int64(s.config.MaxStreamWindowSize),
	}
	st.synSent.Store(!local)
	return st
}

func (st *Stream) ID() uint32 { return st.id }

func (st *Stream) signalRead() {
	select {
	case st.rsignal <- struct{}{}:
	default:
	}
}

func (st *Stream) signalWrite() {
	select {
	case st.wsignal <- struct{}{}:
	default:
	}
}

// push appends received data and reports whether the payload was retained.
func (st *Stream) push(payload []byte, fin bool) bool {
	remove := false
	st.rmu.Lock()
	if st.rerr != nil {
		st.rmu.Unlock()
		return false
	}
	if len(payload) > 0 {
		st.chunks = append(st.chunks, chunk{buf: payload})
	}
	if fin {
		st.rerr = io.EOF
		st.remoteClosed.Store(true)
		remove = st.localClosed.Load()
	}
	st.rmu.Unlock()
	st.signalRead()
	if remove {
		st.sess.removeStream(st)
	}
	return len(payload) > 0
}

func (st *Stream) addWindow(inc int64) {
	if inc <= 0 {
		return
	}
	st.windowMu.Lock()
	st.sendWindow += inc
	if st.sendWindow > int64(maxWindow) {
		st.sendWindow = int64(maxWindow)
	}
	st.windowMu.Unlock()
	st.signalWrite()
}

func (st *Stream) Read(p []byte) (int, error) {
	for {
		st.rmu.Lock()
		if len(st.chunks) > 0 {
			c := &st.chunks[0]
			n := copy(p, c.buf[c.off:])
			c.off += n
			var full []byte
			if c.off == len(c.buf) {
				full = c.buf
				st.popChunkLocked()
			}
			// Account for consumed bytes while the lock is already held; the
			// WINDOW_UPDATE itself is sent after unlocking.
			var inc int64
			if n > 0 {
				st.pendingWindow += int64(n)
				if threshold := int64(st.sess.config.MaxStreamWindowSize / 2); st.pendingWindow >= threshold {
					inc = st.pendingWindow
					st.pendingWindow = 0
				}
			}
			st.rmu.Unlock()
			if full != nil {
				st.sess.putRecvBuf(full)
			}
			if inc > 0 {
				st.sendWindowUpdate(inc)
			}
			return n, nil
		}
		if st.rerr != nil {
			err := st.rerr
			st.rmu.Unlock()
			return 0, err
		}
		st.rmu.Unlock()
		if len(p) == 0 {
			return 0, nil
		}
		if err := st.waitRead(); err != nil {
			return 0, err
		}
	}
}

func (st *Stream) popChunkLocked() {
	if len(st.chunks) > 1 {
		copy(st.chunks, st.chunks[1:])
		st.chunks[len(st.chunks)-1] = chunk{}
		st.chunks = st.chunks[:len(st.chunks)-1]
		return
	}
	st.chunks[0] = chunk{}
	st.chunks = st.chunks[:0]
}

func (st *Stream) waitRead() error {
	for {
		t, changed := st.rdeadline.snapshot()
		var timer *time.Timer
		var timeout <-chan time.Time
		if !t.IsZero() {
			d := time.Until(t)
			if d <= 0 {
				return os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			timeout = timer.C
		}
		select {
		case <-st.rsignal:
		case <-changed:
		case <-timeout:
			return os.ErrDeadlineExceeded
		case <-st.sess.closeCh:
			if timer != nil {
				timer.Stop()
			}
			return st.sess.err()
		}
		if timer != nil {
			timer.Stop()
		}
		st.rmu.Lock()
		ready := len(st.chunks) > 0 || st.rerr != nil
		st.rmu.Unlock()
		if ready {
			return nil
		}
	}
}

// sendWindowUpdate replenishes the peer's send window after the application
// consumed bytes. Sending from the application goroutine (not the reader)
// keeps the reader from ever blocking on the write path.
func (st *Stream) sendWindowUpdate(inc int64) {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(inc)) // #nosec G115
	_ = st.sess.enqueue(frameHeader{typ: frameWindowUpdate, streamID: st.id}, payload[:])
}

// Write copies into the session batch and returns without waiting for a syscall.
func (st *Stream) Write(p []byte) (int, error) {
	st.wmu.Lock()
	defer st.wmu.Unlock()
	if st.localClosed.Load() {
		return 0, ErrStreamClosed
	}
	if st.reset.Load() {
		return 0, ErrStreamReset
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxFramePayload {
			n = maxFramePayload
		}
		if err := st.acquireWindow(n); err != nil {
			return total, err
		}
		var flags uint16
		if st.synSent.CompareAndSwap(false, true) {
			flags |= flagSYN
		}
		if err := st.sess.enqueue(frameHeader{
			typ:      frameData,
			flags:    flags,
			streamID: st.id,
		}, p[:n]); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

func (st *Stream) acquireWindow(n int) error {
	for {
		st.windowMu.Lock()
		if st.sendWindow >= int64(n) {
			st.sendWindow -= int64(n)
			st.windowMu.Unlock()
			return nil
		}
		st.windowMu.Unlock()

		if st.localClosed.Load() {
			return ErrStreamClosed
		}
		if st.reset.Load() {
			return ErrStreamReset
		}

		t, changed := st.wdeadline.snapshot()
		var timer *time.Timer
		var timeout <-chan time.Time
		if !t.IsZero() {
			d := time.Until(t)
			if d <= 0 {
				return os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			timeout = timer.C
		}
		select {
		case <-st.wsignal:
		case <-changed:
		case <-timeout:
			return os.ErrDeadlineExceeded
		case <-st.sess.closeCh:
			if timer != nil {
				timer.Stop()
			}
			return st.sess.err()
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// CloseWrite half-closes the stream: the peer sees EOF for reads, but this
// side can still read.
func (st *Stream) CloseWrite() error {
	if st.localClosed.Swap(true) {
		return nil
	}
	st.signalWrite()

	st.wmu.Lock()
	defer st.wmu.Unlock()
	var flags uint16 = flagFIN
	if st.synSent.CompareAndSwap(false, true) {
		flags |= flagSYN
	}
	err := st.sess.enqueue(frameHeader{
		typ:      frameData,
		flags:    flags,
		streamID: st.id,
	}, nil)
	if st.remoteClosed.Load() {
		st.sess.removeStream(st)
	}
	return err
}

// Close closes both directions and releases the stream.
func (st *Stream) Close() error {
	st.rmu.Lock()
	if st.rerr == nil {
		st.rerr = ErrStreamClosed
	}
	chunks := st.chunks
	st.chunks = nil
	st.rmu.Unlock()
	for _, c := range chunks {
		st.sess.putRecvBuf(c.buf)
	}
	st.signalRead()

	_ = st.CloseWrite()
	st.sess.removeStream(st)
	return nil
}

// handleReset aborts the stream after a peer RESET.
func (st *Stream) handleReset() {
	st.reset.Store(true)
	st.rmu.Lock()
	if st.rerr == nil {
		st.rerr = ErrStreamReset
	}
	chunks := st.chunks
	st.chunks = nil
	st.rmu.Unlock()
	for _, c := range chunks {
		st.sess.putRecvBuf(c.buf)
	}
	st.signalRead()
	st.signalWrite()
	st.sess.removeStream(st)
}

// sessionClosed wakes every blocked operation after the session dies.
func (st *Stream) sessionClosed(err error) {
	st.rmu.Lock()
	if st.rerr == nil {
		st.rerr = err
	}
	chunks := st.chunks
	st.chunks = nil
	st.rmu.Unlock()
	for _, c := range chunks {
		st.sess.putRecvBuf(c.buf)
	}
	st.signalRead()
	st.signalWrite()
}

func (st *Stream) SetDeadline(t time.Time) error {
	st.rdeadline.set(t)
	st.wdeadline.set(t)
	return nil
}

func (st *Stream) SetReadDeadline(t time.Time) error {
	st.rdeadline.set(t)
	return nil
}

func (st *Stream) SetWriteDeadline(t time.Time) error {
	st.wdeadline.set(t)
	return nil
}

func (st *Stream) LocalAddr() net.Addr { return st.sess.conn.LocalAddr() }

func (st *Stream) RemoteAddr() net.Addr { return st.sess.conn.RemoteAddr() }

type deadline struct {
	mu sync.Mutex
	t  time.Time
	ch chan struct{}
}

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	d.t = t
	if d.ch != nil {
		close(d.ch)
	}
	d.ch = make(chan struct{})
	d.mu.Unlock()
}

func (d *deadline) snapshot() (time.Time, <-chan struct{}) {
	d.mu.Lock()
	if d.ch == nil {
		d.ch = make(chan struct{})
	}
	t, ch := d.t, d.ch
	d.mu.Unlock()
	return t, ch
}
