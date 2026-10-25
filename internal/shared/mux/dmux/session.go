package dmux

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Session multiplexes streams over a single reliable byte stream.
type Session struct {
	conn    net.Conn
	config  *Config
	localID uint32 // parity of locally opened stream IDs: 1 client, 0 server

	nextStreamID atomic.Uint32

	streamsMu  sync.RWMutex
	streams    map[uint32]*Stream
	numStreams atomic.Int32

	acceptCh chan *Stream

	sendMu   sync.Mutex
	sendBuf  []byte
	outCh    chan []byte
	sendPool sync.Pool
	recvPool sync.Pool

	pingMu  sync.Mutex
	pings   map[uint64]chan struct{}
	pingSeq atomic.Uint64

	goAwayLocal atomic.Bool
	goAwayPeer  atomic.Bool

	closeOnce sync.Once
	closeCh   chan struct{}
	closeErr  atomic.Pointer[error]
}

// Client creates a session that opens odd-numbered streams.
func Client(conn net.Conn, cfg *Config) (*Session, error) {
	return newSession(conn, cfg, true)
}

// Server creates a session that opens even-numbered streams.
func Server(conn net.Conn, cfg *Config) (*Session, error) {
	return newSession(conn, cfg, false)
}

func newSession(conn net.Conn, cfg *Config, client bool) (*Session, error) {
	if conn == nil {
		return nil, fmt.Errorf("dmux: nil connection")
	}
	cfg = cloneConfig(cfg)
	cfg.normalize()

	s := &Session{
		conn:     conn,
		config:   cfg,
		streams:  make(map[uint32]*Stream),
		acceptCh: make(chan *Stream, cfg.AcceptBacklog),
		outCh:    make(chan []byte, outQueueDepth),
		pings:    make(map[uint64]chan struct{}),
		closeCh:  make(chan struct{}),
	}
	if client {
		s.localID = 1
		s.nextStreamID.Store(1)
	} else {
		s.localID = 0
		s.nextStreamID.Store(2)
	}
	s.sendPool.New = func() any {
		b := make([]byte, batchSize)
		return &b
	}
	s.recvPool.New = func() any {
		b := make([]byte, maxFramePayload)
		return &b
	}
	s.sendBuf = s.getSendBuf()

	go s.sendLoop()
	go s.recvLoop()
	if cfg.EnableKeepAlive {
		go s.keepAliveLoop()
	}
	return s, nil
}

func cloneConfig(cfg *Config) *Config {
	if cfg == nil {
		return DefaultConfig()
	}
	c := *cfg
	return &c
}

func (s *Session) getSendBuf() []byte {
	p := s.sendPool.Get().(*[]byte)
	// Return an empty slice: the buffer is filled with append-style writes.
	return (*p)[:0]
}

func (s *Session) putSendBuf(b []byte) {
	if cap(b) != batchSize {
		return
	}
	b = b[:batchSize]
	s.sendPool.Put(&b)
}

// Open creates a new locally initiated stream. No frame is sent until the
// first Write or CloseWrite, so opening costs no round trip.
func (s *Session) Open() (*Stream, error) {
	if s.isClosed() {
		return nil, s.err()
	}
	if s.goAwayLocal.Load() || s.goAwayPeer.Load() {
		return nil, ErrGoAway
	}
	id := s.nextStreamID.Add(2) - 2
	st := newStream(s, id, true)
	s.streamsMu.Lock()
	if s.isClosed() {
		s.streamsMu.Unlock()
		return nil, s.err()
	}
	s.streams[id] = st
	s.streamsMu.Unlock()
	s.numStreams.Add(1)
	// Announce the stream immediately. This is optimistic: the frame is not
	// awaited, so Open still costs no round trip, but the peer learns about
	// the stream even if the caller writes nothing.
	if err := s.enqueue(frameHeader{
		typ:      frameData,
		flags:    flagSYN,
		streamID: id,
	}, nil); err != nil {
		s.removeStream(st)
		return nil, err
	}
	st.synSent.Store(true)
	return st, nil
}

// OpenStream is an alias for Open.
func (s *Session) OpenStream() (*Stream, error) { return s.Open() }

// Accept returns the next stream opened by the peer.
func (s *Session) Accept() (*Stream, error) {
	for {
		select {
		case st := <-s.acceptCh:
			return st, nil
		case <-s.closeCh:
			// Drain any stream that raced with the close before reporting it.
			select {
			case st := <-s.acceptCh:
				return st, nil
			default:
				return nil, s.err()
			}
		}
	}
}

// AcceptStream is an alias for Accept.
func (s *Session) AcceptStream() (*Stream, error) { return s.Accept() }

// Ping measures a round trip to the peer.
func (s *Session) Ping() (time.Duration, error) {
	if s.isClosed() {
		return 0, s.err()
	}
	id := s.pingSeq.Add(1)
	ch := make(chan struct{}, 1)
	s.pingMu.Lock()
	s.pings[id] = ch
	s.pingMu.Unlock()

	var payload [8]byte
	binary.BigEndian.PutUint64(payload[:], id)
	start := time.Now()
	if err := s.enqueue(frameHeader{typ: framePing}, payload[:]); err != nil {
		s.removePing(id)
		return 0, err
	}

	timer := time.NewTimer(s.config.KeepAliveTimeout)
	defer timer.Stop()
	select {
	case <-ch:
		s.removePing(id)
		return time.Since(start), nil
	case <-timer.C:
		s.removePing(id)
		return 0, ErrTimeout
	case <-s.closeCh:
		s.removePing(id)
		return 0, s.err()
	}
}

func (s *Session) removePing(id uint64) {
	s.pingMu.Lock()
	delete(s.pings, id)
	s.pingMu.Unlock()
}

// GoAway tells the peer that no new streams will be opened and refuses new
// local streams. Existing streams keep working.
func (s *Session) GoAway() error {
	if s.goAwayLocal.Swap(true) {
		return nil
	}
	if s.isClosed() {
		return s.err()
	}
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], s.lastLocalStreamID())
	return s.enqueue(frameHeader{typ: frameGoAway}, payload[:])
}

func (s *Session) lastLocalStreamID() uint32 {
	id := s.nextStreamID.Load()
	if id < 2 {
		return 0
	}
	return id - 2
}

// NumStreams reports the number of streams that have not been closed.
func (s *Session) NumStreams() int { return int(s.numStreams.Load()) }

// IsClosed reports whether the session has terminated.
func (s *Session) IsClosed() bool { return s.isClosed() }

// CloseChan is closed when the session terminates.
func (s *Session) CloseChan() <-chan struct{} { return s.closeCh }

// LocalAddr returns the local transport address.
func (s *Session) LocalAddr() net.Addr { return s.conn.LocalAddr() }

// RemoteAddr returns the peer transport address.
func (s *Session) RemoteAddr() net.Addr { return s.conn.RemoteAddr() }

// Close terminates the session and all streams.
func (s *Session) Close() error {
	s.closeWith(ErrSessionClosed)
	return nil
}

func (s *Session) isClosed() bool {
	select {
	case <-s.closeCh:
		return true
	default:
		return false
	}
}

func (s *Session) err() error {
	if e := s.closeErr.Load(); e != nil {
		return *e
	}
	return ErrSessionClosed
}

func (s *Session) closeWith(err error) {
	s.closeOnce.Do(func() {
		if err == nil {
			err = ErrSessionClosed
		}
		s.closeErr.Store(&err)
		close(s.closeCh)
		_ = s.conn.Close()

		s.streamsMu.Lock()
		streams := s.streams
		s.streams = make(map[uint32]*Stream)
		s.streamsMu.Unlock()
		for _, st := range streams {
			st.sessionClosed(err)
		}
		s.numStreams.Store(0)

		s.pingMu.Lock()
		for id, ch := range s.pings {
			select {
			case ch <- struct{}{}:
			default:
			}
			delete(s.pings, id)
		}
		s.pingMu.Unlock()
	})
}

func (s *Session) removeStream(st *Stream) {
	s.streamsMu.Lock()
	if cur, ok := s.streams[st.id]; ok && cur == st {
		delete(s.streams, st.id)
		s.numStreams.Add(-1)
	}
	s.streamsMu.Unlock()
}

func (s *Session) lookupStream(id uint32) *Stream {
	s.streamsMu.RLock()
	st := s.streams[id]
	s.streamsMu.RUnlock()
	return st
}

// enqueue appends one frame to the session send buffer and hands the batch to
// the writer. The payload is copied, so callers keep ownership of their buffer
// and Write never waits for a syscall.
func (s *Session) enqueue(h frameHeader, payload []byte) error {
	if headerSize+len(payload) > batchSize {
		return fmt.Errorf("%w: frame of %d bytes exceeds batch", ErrProtocol, headerSize+len(payload))
	}
	h.length = uint32(len(payload))
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if len(s.sendBuf)+headerSize+len(payload) > cap(s.sendBuf) {
		if err := s.flushLocked(); err != nil {
			return err
		}
	}
	off := len(s.sendBuf)
	s.sendBuf = s.sendBuf[:off+headerSize+len(payload)]
	h.encode(s.sendBuf[off : off+headerSize])
	copy(s.sendBuf[off+headerSize:], payload)
	return s.flushLocked()
}

func (s *Session) flushLocked() error {
	if len(s.sendBuf) == 0 {
		return nil
	}
	batch := s.sendBuf
	s.sendBuf = s.getSendBuf()
	select {
	case s.outCh <- batch:
		return nil
	case <-s.closeCh:
		s.putSendBuf(batch)
		return s.err()
	}
}

func (s *Session) sendLoop() {
	defer s.Close()

	batches := make([][]byte, 0, outQueueDepth)
	toReturn := make([][]byte, 0, outQueueDepth)
	bufs := make(net.Buffers, 0, outQueueDepth)
	var lastDeadline time.Time

	for {
		var first []byte
		select {
		case first = <-s.outCh:
		case <-s.closeCh:
			return
		}
		batches = append(batches[:0], first)
		// Drain everything already queued so a burst becomes one writev.
	drain:
		for len(batches) < outQueueDepth {
			select {
			case b := <-s.outCh:
				batches = append(batches, b)
			default:
				break drain
			}
		}

		toReturn = append(toReturn[:0], batches...)

		// Re-arming the socket deadline on every batch costs a netpoller
		// timer update. Refresh at most once per half timeout: a stuck write
		// is still detected within 1.5x ConnectionWriteTimeout.
		now := time.Now()
		if lastDeadline.IsZero() || now.Sub(lastDeadline) >= s.config.ConnectionWriteTimeout/2 {
			_ = s.conn.SetWriteDeadline(now.Add(s.config.ConnectionWriteTimeout))
			lastDeadline = now
		}

		var err error
		if len(batches) == 1 {
			// Single batch: a plain Write avoids the net.Buffers machinery.
			_, err = s.conn.Write(batches[0])
		} else {
			bufs = append(bufs[:0], batches...)
			_, err = bufs.WriteTo(s.conn)
		}
		for _, b := range toReturn {
			s.putSendBuf(b)
		}
		if err != nil {
			s.closeWith(err)
			return
		}
	}
}

func (s *Session) keepAliveLoop() {
	ticker := time.NewTicker(s.config.KeepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := s.Ping(); err != nil {
				s.closeWith(err)
				return
			}
		case <-s.closeCh:
			return
		}
	}
}

func (s *Session) recvLoop() {
	br := bufio.NewReaderSize(s.conn, 64*1024)
	var hdr [headerSize]byte
	// Control frames are at most 8 bytes (validated in decodeHeader). Reuse a
	// stack buffer so they never touch the 32KB receive pool.
	var ctrl [8]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			s.closeWith(err)
			return
		}
		h, err := decodeHeader(hdr[:])
		if err != nil {
			s.closeWith(err)
			return
		}
		if h.length == 0 {
			if _, err := s.handleFrame(h, nil); err != nil {
				s.closeWith(err)
				return
			}
			continue
		}
		var buf []byte
		pooled := false
		if h.typ == frameData {
			buf = s.getRecvBuf()[:h.length]
			pooled = true
		} else {
			buf = ctrl[:h.length]
		}
		if _, err := io.ReadFull(br, buf); err != nil {
			if pooled {
				s.putRecvBuf(buf)
			}
			s.closeWith(err)
			return
		}
		keep, err := s.handleFrame(h, buf)
		if pooled && !keep {
			s.putRecvBuf(buf)
		}
		if err != nil {
			s.closeWith(err)
			return
		}
	}
}

func (s *Session) getRecvBuf() []byte {
	p := s.recvPool.Get().(*[]byte)
	return (*p)[:maxFramePayload]
}

func (s *Session) putRecvBuf(b []byte) {
	if cap(b) != maxFramePayload {
		return
	}
	b = b[:maxFramePayload]
	s.recvPool.Put(&b)
}

// handleFrame processes one frame. It reports whether the payload buffer was
// retained by a stream (DATA frames), in which case the caller must not return
// it to the pool.
func (s *Session) handleFrame(h frameHeader, payload []byte) (bool, error) {
	switch h.typ {
	case frameData:
		return s.handleData(h, payload)
	case frameWindowUpdate:
		st := s.lookupStream(h.streamID)
		if st != nil {
			st.addWindow(int64(binary.BigEndian.Uint32(payload)))
		}
		return false, nil
	case framePing:
		if err := s.enqueue(frameHeader{typ: framePong}, payload); err != nil {
			return false, err
		}
		return false, nil
	case framePong:
		id := binary.BigEndian.Uint64(payload)
		s.pingMu.Lock()
		ch := s.pings[id]
		delete(s.pings, id)
		s.pingMu.Unlock()
		if ch != nil {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
		return false, nil
	case frameReset:
		if st := s.lookupStream(h.streamID); st != nil {
			st.handleReset()
		}
		return false, nil
	case frameGoAway:
		s.goAwayPeer.Store(true)
		return false, nil
	default:
		return false, fmt.Errorf("%w: frame type %d", ErrProtocol, h.typ)
	}
}

func (s *Session) handleData(h frameHeader, payload []byte) (bool, error) {
	st := s.lookupStream(h.streamID)
	if st == nil {
		if h.flags&flagSYN == 0 {
			// Data for an unknown stream: reject it instead of failing the
			// whole session.
			_ = s.sendReset(h.streamID)
			return false, nil
		}
		var err error
		st, err = s.acceptRemoteStream(h.streamID)
		if err != nil {
			return false, err
		}
		if st == nil {
			// Backlog full; the stream was reset.
			return false, nil
		}
	}
	if !st.push(payload, h.flags&flagFIN != 0) {
		return false, nil
	}
	return true, nil
}

// acceptRemoteStream registers a peer-initiated stream. It returns (nil, nil)
// when the stream had to be rejected, so the session survives.
func (s *Session) acceptRemoteStream(id uint32) (*Stream, error) {
	if id%2 == s.localID {
		return nil, fmt.Errorf("%w: unexpected stream id %d", ErrProtocol, id)
	}
	s.streamsMu.Lock()
	if s.goAwayLocal.Load() {
		s.streamsMu.Unlock()
		_ = s.sendReset(id)
		return nil, nil
	}
	if _, exists := s.streams[id]; exists {
		s.streamsMu.Unlock()
		_ = s.sendReset(id)
		return nil, nil
	}
	st := newStream(s, id, false)
	s.streams[id] = st
	s.streamsMu.Unlock()
	s.numStreams.Add(1)

	select {
	case s.acceptCh <- st:
		return st, nil
	default:
		// Accept backlog exhausted: reset rather than block the reader and
		// stall every other stream on the session.
		s.removeStream(st)
		_ = s.sendReset(id)
		return nil, nil
	}
}

func (s *Session) sendReset(id uint32) error {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], 1) // generic reset code
	return s.enqueue(frameHeader{typ: frameReset, streamID: id}, payload[:])
}
