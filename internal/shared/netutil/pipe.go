package netutil

import (
	"context"
	"io"
	"sync"
	"time"

	"drip/internal/shared/pool"
)

const pipeWriteTimeout = 30 * time.Second

type closeReader interface {
	CloseRead() error
}

type closeWriter interface {
	CloseWrite() error
}

type readDeadliner interface {
	SetReadDeadline(t time.Time) error
}

type writeDeadliner interface {
	SetWriteDeadline(t time.Time) error
}

// Pipe copies bytes bidirectionally between a and b (gost-like),
// and applies TCP half-close when supported.
func Pipe(ctx context.Context, a, b io.ReadWriteCloser) error {
	return PipeWithCallbacksAndBufferSize(ctx, a, b, pool.SizeMedium, nil, nil)
}

// PipeWithCallbacks is Pipe with optional byte counters for each direction:
// onAToB is called with bytes copied from a -> b, onBToA for b -> a.
func PipeWithCallbacks(ctx context.Context, a, b io.ReadWriteCloser, onAToB func(n int64), onBToA func(n int64)) error {
	return PipeWithCallbacksAndBufferSize(ctx, a, b, pool.SizeMedium, onAToB, onBToA)
}

// PipeWithBufferSize is Pipe with a custom buffer size.
func PipeWithBufferSize(ctx context.Context, a, b io.ReadWriteCloser, bufSize int) error {
	return PipeWithCallbacksAndBufferSize(ctx, a, b, bufSize, nil, nil)
}

// PipeWithCallbacksAndBufferSize is PipeWithCallbacks with a custom buffer size.
func PipeWithCallbacksAndBufferSize(ctx context.Context, a, b io.ReadWriteCloser, bufSize int, onAToB func(n int64), onBToA func(n int64)) error {
	return PipeWithCallbacksAndBufferSizeAndWriteTimeout(ctx, a, b, bufSize, onAToB, onBToA, pipeWriteTimeout)
}

// PipeWithCallbacksAndBufferSizeAndWriteTimeout is PipeWithCallbacksAndBufferSize
// with an explicit per-write deadline. A non-positive timeout disables deadline
// updates for callers that need legacy blocking semantics.
func PipeWithCallbacksAndBufferSizeAndWriteTimeout(ctx context.Context, a, b io.ReadWriteCloser, bufSize int, onAToB func(n int64), onBToA func(n int64), writeTimeout time.Duration) error {
	if bufSize <= 0 {
		bufSize = pool.SizeMedium
	}
	if bufSize > pool.SizeLarge {
		bufSize = pool.SizeLarge
	}

	var wg sync.WaitGroup
	wg.Add(2)

	stopCh := make(chan struct{})
	var closeOnce sync.Once
	closeAll := func() {
		closeOnce.Do(func() {
			close(stopCh)
			// A multiplexed stream may implement Close as a write-side FIN.
			// Deadlines also interrupt outstanding reads during cancellation.
			for _, conn := range []io.ReadWriteCloser{a, b} {
				if rd, ok := conn.(readDeadliner); ok {
					_ = rd.SetReadDeadline(time.Now())
				}
				if wd, ok := conn.(writeDeadliner); ok {
					_ = wd.SetWriteDeadline(time.Now())
				}
			}
			_ = a.Close()
			_ = b.Close()
		})
	}

	errCh := make(chan error, 2)

	go func() {
		defer wg.Done()
		err := pipeBuffer(b, a, bufSize, onAToB, stopCh, writeTimeout)
		if err != nil {
			errCh <- err
			closeAll()
		}
	}()

	go func() {
		defer wg.Done()
		err := pipeBuffer(a, b, bufSize, onBToA, stopCh, writeTimeout)
		if err != nil {
			errCh <- err
			closeAll()
		}
	}()

	if ctx != nil {
		stop := context.AfterFunc(ctx, closeAll)
		defer stop()
	}

	wg.Wait()
	closeAll()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func pipeBuffer(dst io.ReadWriteCloser, src io.ReadWriteCloser, bufSize int, onCopied func(n int64), stopCh <-chan struct{}, writeTimeout time.Duration) error {
	bufPtr := pool.GetBuffer(bufSize)
	defer pool.PutBuffer(bufPtr)

	buf := (*bufPtr)[:bufSize]
	_, err := copyBuffer(dst, src, buf, onCopied, stopCh, writeTimeout)

	if cr, ok := src.(closeReader); ok {
		_ = cr.CloseRead()
	}

	if cw, ok := dst.(closeWriter); ok {
		if e := cw.CloseWrite(); e != nil {
			_ = dst.Close()
		}
	} else {
		_ = dst.Close()
	}

	return err
}

const stopCheckInterval = 64

func copyBuffer(dst io.Writer, src io.Reader, buf []byte, onCopied func(n int64), stopCh <-chan struct{}, writeTimeout time.Duration) (written int64, err error) {
	if wd, ok := dst.(writeDeadliner); ok && writeTimeout > 0 {
		defer func() {
			_ = wd.SetWriteDeadline(time.Time{})
		}()
	}

	for i := 0; ; i++ {
		if i%stopCheckInterval == 0 {
			select {
			case <-stopCh:
				return written, io.EOF
			default:
			}
		}

		nr, er := src.Read(buf)
		if nr > 0 {
			if wd, ok := dst.(writeDeadliner); ok && writeTimeout > 0 {
				_ = wd.SetWriteDeadline(time.Now().Add(writeTimeout))
			}
			nw, ew := dst.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
				if onCopied != nil {
					onCopied(int64(nw))
				}
			}
			if ew != nil {
				return written, ew
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return written, nil
			}
			return written, er
		}
	}
}
