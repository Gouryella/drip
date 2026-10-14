package protocol

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type FrameWriter struct {
	conn         io.Writer
	queue        chan *Frame
	controlQueue chan *Frame
	batch        []*Frame
	mu           sync.Mutex
	enqueueMu    sync.RWMutex
	done         chan struct{}
	closed       bool
	closeOnce    sync.Once

	maxBatch     int
	maxBatchWait time.Duration

	heartbeatInterval time.Duration
	heartbeatCallback func() *Frame
	heartbeatEnabled  bool
	heartbeatControl  chan struct{}

	// Error handling
	writeErr     error
	errOnce      sync.Once
	onWriteError func(error) // Callback for write errors

	// Adaptive flushing
	adaptiveFlush           bool // Enable adaptive flush based on queue depth
	lowConcurrencyThreshold int  // Queue depth threshold for immediate flush

	// Hooks
	preWriteHook func(*Frame) // Called right before a frame is written to conn

	// Backlog tracking
	queuedFrames atomic.Int64
	queuedBytes  atomic.Int64
}

func NewFrameWriter(conn io.Writer) *FrameWriter {
	w := NewFrameWriterWithConfig(conn, 256, 2*time.Millisecond, 4096)
	w.EnableAdaptiveFlush(16)
	return w
}

func NewFrameWriterWithConfig(conn io.Writer, maxBatch int, maxBatchWait time.Duration, queueSize int) *FrameWriter {
	w := &FrameWriter{
		conn:  conn,
		queue: make(chan *Frame, queueSize),
		controlQueue: make(chan *Frame, func() int {
			if queueSize < 256 {
				return queueSize
			}
			return 256
		}()), // control path needs small, fast buffer
		batch:            make([]*Frame, 0, maxBatch),
		maxBatch:         maxBatch,
		maxBatchWait:     maxBatchWait,
		done:             make(chan struct{}),
		heartbeatControl: make(chan struct{}, 1),
	}
	go w.writeLoop()
	return w
}

func (w *FrameWriter) writeLoop() {
	batchTicker := time.NewTicker(w.maxBatchWait)
	defer batchTicker.Stop()

	var heartbeatTicker *time.Ticker
	var heartbeatCh <-chan time.Time

	w.mu.Lock()
	if w.heartbeatEnabled && w.heartbeatInterval > 0 {
		heartbeatTicker = time.NewTicker(w.heartbeatInterval)
		heartbeatCh = heartbeatTicker.C
	}
	w.mu.Unlock()

	defer func() {
		if heartbeatTicker != nil {
			heartbeatTicker.Stop()
		}
	}()

	for {
		// Always drain control queue first to prioritize control/heartbeat frames.
		select {
		case frame, ok := <-w.controlQueue:
			if !ok {
				w.mu.Lock()
				w.flushBatchLocked()
				w.mu.Unlock()
				return
			}
			w.mu.Lock()
			w.flushFrameLocked(frame)
			w.mu.Unlock()
			continue
		default:
		}

		select {
		case frame, ok := <-w.queue:
			if !ok {
				w.mu.Lock()
				w.flushBatchLocked()
				w.mu.Unlock()
				return
			}

			w.mu.Lock()
			w.batch = append(w.batch, frame)

			shouldFlushNow := len(w.batch) >= w.maxBatch ||
				(w.adaptiveFlush && len(w.queue) <= w.lowConcurrencyThreshold)

			if shouldFlushNow {
				w.flushBatchLocked()
			}
			w.mu.Unlock()

		case <-batchTicker.C:
			w.mu.Lock()
			if len(w.batch) > 0 {
				w.flushBatchLocked()
			}
			w.mu.Unlock()

		case <-heartbeatCh:
			w.mu.Lock()
			if w.heartbeatCallback != nil {
				if frame := w.heartbeatCallback(); frame != nil {
					w.flushFrameLocked(frame)
				}
			}
			w.mu.Unlock()

		case <-w.heartbeatControl:
			w.mu.Lock()
			if heartbeatTicker != nil {
				heartbeatTicker.Stop()
				heartbeatTicker = nil
				heartbeatCh = nil
			}
			if w.heartbeatEnabled && w.heartbeatInterval > 0 {
				heartbeatTicker = time.NewTicker(w.heartbeatInterval)
				heartbeatCh = heartbeatTicker.C
			}
			w.mu.Unlock()

		case <-w.done:
			w.mu.Lock()
			w.flushBatchLocked()
			w.mu.Unlock()
			return
		}
	}
}

func (w *FrameWriter) flushBatchLocked() {
	if len(w.batch) == 0 {
		return
	}

	for i, frame := range w.batch {
		w.flushFrameLocked(frame)
		w.batch[i] = nil
	}

	w.batch = w.batch[:0]
}

// flushFrameLocked writes a single frame immediately. Caller must hold w.mu.
func (w *FrameWriter) flushFrameLocked(frame *Frame) {
	if frame == nil {
		return
	}
	if w.writeErr != nil {
		w.unmarkQueued(frame)
		frame.Release()
		return
	}

	if w.preWriteHook != nil {
		w.preWriteHook(frame)
	}

	if err := WriteFrame(w.conn, frame); err != nil {
		w.errOnce.Do(func() {
			w.writeErr = err
			if w.onWriteError != nil {
				go w.onWriteError(err)
			}
			w.closed = true
			go w.Close()
		})
	}

	w.unmarkQueued(frame)
	frame.Release()
}

func (w *FrameWriter) WriteFrame(frame *Frame) error {
	return w.WriteFrameWithCancel(frame, nil)
}

// WriteFrameWithCancel writes a frame with an optional cancellation channel
// If cancel is closed, the write will be aborted immediately
func (w *FrameWriter) WriteFrameWithCancel(frame *Frame, cancel <-chan struct{}) error {
	if frame == nil {
		return nil
	}

	w.enqueueMu.RLock()
	defer w.enqueueMu.RUnlock()

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		if w.writeErr != nil {
			return w.writeErr
		}
		return errors.New("writer closed")
	}
	w.mu.Unlock()

	size := int64(len(frame.Payload) + FrameHeaderSize)
	w.queuedFrames.Add(1)
	w.queuedBytes.Add(size)
	frame.queuedBytes.Store(size)

	// Try non-blocking first for best performance
	select {
	case w.queue <- frame:
		return nil
	case <-w.done:
		w.queuedFrames.Add(-1)
		w.queuedBytes.Add(-size)
		frame.queuedBytes.Store(0)
		w.mu.Lock()
		err := w.writeErr
		w.mu.Unlock()
		if err != nil {
			return err
		}
		return errors.New("writer closed")
	default:
	}

	// Queue full - block with cancellation support
	if cancel != nil {
		select {
		case w.queue <- frame:
			return nil
		case <-w.done:
			w.queuedFrames.Add(-1)
			w.queuedBytes.Add(-size)
			frame.queuedBytes.Store(0)
			w.mu.Lock()
			err := w.writeErr
			w.mu.Unlock()
			if err != nil {
				return err
			}
			return errors.New("writer closed")
		case <-cancel:
			w.queuedFrames.Add(-1)
			w.queuedBytes.Add(-size)
			frame.queuedBytes.Store(0)
			return errors.New("write cancelled")
		}
	}

	// No cancel channel - block with timeout
	select {
	case w.queue <- frame:
		return nil
	case <-w.done:
		w.queuedFrames.Add(-1)
		w.queuedBytes.Add(-size)
		frame.queuedBytes.Store(0)

		w.mu.Lock()
		err := w.writeErr
		w.mu.Unlock()
		if err != nil {
			return err
		}
		return errors.New("writer closed")
	case <-time.After(30 * time.Second):
		w.queuedFrames.Add(-1)
		w.queuedBytes.Add(-size)
		frame.queuedBytes.Store(0)
		return errors.New("write queue full timeout")
	}
}

func (w *FrameWriter) Close() error {
	w.closeOnce.Do(func() {
		// Wake blocked enqueuers before taking their exclusive lock.
		close(w.done)
		w.enqueueMu.Lock()
		defer w.enqueueMu.Unlock()

		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()

		close(w.queue)
		close(w.controlQueue)

		for frame := range w.queue {
			w.unmarkQueued(frame)
			frame.Release()
		}
		for frame := range w.controlQueue {
			w.unmarkQueued(frame)
			frame.Release()
		}

	})
	return nil
}

func (w *FrameWriter) Flush() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}

	for {
		select {
		case frame, ok := <-w.queue:
			if !ok {
				goto done
			}
			w.batch = append(w.batch, frame)
		default:
			goto done
		}
	}
done:
	w.flushBatchLocked()
	w.mu.Unlock()
}

func (w *FrameWriter) EnableHeartbeat(interval time.Duration, callback func() *Frame) {
	w.mu.Lock()
	w.heartbeatInterval = interval
	w.heartbeatCallback = callback
	w.heartbeatEnabled = true
	w.mu.Unlock()

	select {
	case w.heartbeatControl <- struct{}{}:
	default:
	}
}

func (w *FrameWriter) DisableHeartbeat() {
	w.mu.Lock()
	w.heartbeatEnabled = false
	w.mu.Unlock()

	select {
	case w.heartbeatControl <- struct{}{}:
	default:
	}
}

func (w *FrameWriter) SetWriteErrorHandler(handler func(error)) {
	w.mu.Lock()
	w.onWriteError = handler
	w.mu.Unlock()
}

func (w *FrameWriter) EnableAdaptiveFlush(lowConcurrencyThreshold int) {
	w.mu.Lock()
	w.adaptiveFlush = true
	w.lowConcurrencyThreshold = lowConcurrencyThreshold
	w.mu.Unlock()
}

func (w *FrameWriter) DisableAdaptiveFlush() {
	w.mu.Lock()
	w.adaptiveFlush = false
	w.mu.Unlock()
}

// WriteControl enqueues a control/prioritized frame to be written ahead of data frames.
func (w *FrameWriter) WriteControl(frame *Frame) error {
	if frame == nil {
		return nil
	}

	w.enqueueMu.RLock()
	defer w.enqueueMu.RUnlock()

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		if w.writeErr != nil {
			return w.writeErr
		}
		return errors.New("writer closed")
	}
	w.mu.Unlock()

	size := int64(len(frame.Payload) + FrameHeaderSize)
	w.queuedFrames.Add(1)
	w.queuedBytes.Add(size)
	frame.queuedBytes.Store(size)

	// Try non-blocking first
	select {
	case w.controlQueue <- frame:
		return nil
	case <-w.done:
		w.queuedFrames.Add(-1)
		w.queuedBytes.Add(-size)
		frame.queuedBytes.Store(0)
		w.mu.Lock()
		err := w.writeErr
		w.mu.Unlock()
		if err != nil {
			return err
		}
		return errors.New("writer closed")
	default:
	}

	// Queue full - wait with timeout
	select {
	case w.controlQueue <- frame:
		return nil
	case <-w.done:
		w.queuedFrames.Add(-1)
		w.queuedBytes.Add(-size)
		frame.queuedBytes.Store(0)

		w.mu.Lock()
		err := w.writeErr
		w.mu.Unlock()
		if err != nil {
			return err
		}
		return errors.New("writer closed")
	case <-time.After(50 * time.Millisecond):
		// Control frames should have priority, shorter timeout
		w.queuedFrames.Add(-1)
		w.queuedBytes.Add(-size)
		frame.queuedBytes.Store(0)
		return errors.New("control queue full timeout")
	}
}

// SetPreWriteHook registers a callback invoked just before a frame is written to the underlying writer.
func (w *FrameWriter) SetPreWriteHook(hook func(*Frame)) {
	w.mu.Lock()
	w.preWriteHook = hook
	w.mu.Unlock()
}

// QueuedFrames returns the number of frames currently queued (data + control).
func (w *FrameWriter) QueuedFrames() int64 {
	return w.queuedFrames.Load()
}

// QueuedBytes returns the approximate number of bytes currently queued.
func (w *FrameWriter) QueuedBytes() int64 {
	return w.queuedBytes.Load()
}

// unmarkQueued decrements backlog counters for a frame once it is written or discarded.
func (w *FrameWriter) unmarkQueued(frame *Frame) {
	if frame == nil {
		return
	}
	size := frame.queuedBytes.Swap(0)
	if size <= 0 {
		return
	}
	w.queuedFrames.Add(-1)
	w.queuedBytes.Add(-size)
}
