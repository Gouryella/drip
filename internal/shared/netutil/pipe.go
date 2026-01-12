package netutil

import (
	"context"
	"io"
	"sync"
	"time"

	"drip/internal/shared/pool"
)

const tcpWaitTimeout = 10 * time.Second

type closeReader interface {
	CloseRead() error
}

type closeWriter interface {
	CloseWrite() error
}

type readDeadliner interface {
	SetReadDeadline(t time.Time) error
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
			_ = a.Close()
			_ = b.Close()
		})
	}

	errCh := make(chan error, 2)

	go func() {
		defer wg.Done()
		err := pipeBuffer(b, a, bufSize, onAToB, stopCh)
		if err != nil {
			errCh <- err
		}
		closeAll()
	}()

	go func() {
		defer wg.Done()
		err := pipeBuffer(a, b, bufSize, onBToA, stopCh)
		if err != nil {
			errCh <- err
		}
		closeAll()
	}()

	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				closeAll()
			case <-stopCh:
			}
		}()
	}

	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func pipeBuffer(dst io.ReadWriteCloser, src io.ReadWriteCloser, bufSize int, onCopied func(n int64), stopCh <-chan struct{}) error {
	bufPtr := pool.GetBuffer(bufSize)
	defer pool.PutBuffer(bufPtr)

	buf := (*bufPtr)[:bufSize]
	_, err := copyBuffer(dst, src, buf, onCopied, stopCh)

	if cr, ok := src.(closeReader); ok {
		_ = cr.CloseRead()
	}

	if cw, ok := dst.(closeWriter); ok {
		if e := cw.CloseWrite(); e != nil {
			_ = dst.Close()
		}
		if rd, ok := dst.(readDeadliner); ok {
			_ = rd.SetReadDeadline(time.Now().Add(tcpWaitTimeout))
		}
	} else {
		_ = dst.Close()
		if rd, ok := dst.(readDeadliner); ok {
			_ = rd.SetReadDeadline(time.Now().Add(tcpWaitTimeout))
		}
	}

	return err
}

func copyBuffer(dst io.Writer, src io.Reader, buf []byte, onCopied func(n int64), stopCh <-chan struct{}) (written int64, err error) {
	for {
		select {
		case <-stopCh:
			return written, io.EOF
		default:
		}

		nr, er := src.Read(buf)
		if nr > 0 {
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
