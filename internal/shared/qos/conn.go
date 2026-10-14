package qos

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

var limitedConnBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 32*1024)
		return &b
	},
}

type LimitedConn struct {
	net.Conn
	limiter       *Limiter
	ctx           context.Context
	cancel        context.CancelFunc
	readDeadline  connDeadline
	writeDeadline connDeadline
}

func NewLimitedConn(ctx context.Context, conn net.Conn, limiter *Limiter) *LimitedConn {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	return &LimitedConn{
		Conn:    conn,
		limiter: limiter,
		ctx:     ctx,
		cancel:  cancel,
	}
}

func (c *LimitedConn) Read(b []byte) (n int, err error) {
	if c.limiter == nil || !c.limiter.IsLimited() {
		return c.Conn.Read(b)
	}

	burst := c.limiter.RateLimiter().Burst()
	if len(b) > burst {
		b = b[:burst]
	}

	n, err = c.Conn.Read(b)
	if n > 0 {
		if waitErr := c.waitN(n, &c.readDeadline); waitErr != nil {
			if err == nil {
				err = waitErr
			}
		}
	}
	return n, err
}

func (c *LimitedConn) Write(b []byte) (n int, err error) {
	if c.limiter == nil || !c.limiter.IsLimited() {
		return c.Conn.Write(b)
	}

	burst := c.limiter.RateLimiter().Burst()
	total := 0

	for len(b) > 0 {
		chunk := min(len(b), burst)

		if err := c.waitN(chunk, &c.writeDeadline); err != nil {
			return total, err
		}

		nw, err := c.Conn.Write(b[:chunk])
		total += nw
		if err != nil {
			return total, err
		}
		if nw != chunk {
			return total, io.ErrShortWrite
		}
		b = b[chunk:]
	}

	return total, nil
}

func (c *LimitedConn) ReadFrom(r io.Reader) (n int64, err error) {
	bufPtr := limitedConnBufPool.Get().(*[]byte)
	buf := *bufPtr
	defer limitedConnBufPool.Put(bufPtr)
	for {
		nr, er := r.Read(buf)
		if nr > 0 {
			nw, ew := c.Write(buf[:nr])
			n += int64(nw)
			if ew != nil {
				err = ew
				break
			}
			if nw != nr {
				return n, io.ErrShortWrite
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return n, err
}

func (c *LimitedConn) WriteTo(w io.Writer) (n int64, err error) {
	bufPtr := limitedConnBufPool.Get().(*[]byte)
	buf := *bufPtr
	defer limitedConnBufPool.Put(bufPtr)
	for {
		nr, er := c.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			n += int64(nw)
			if ew != nil {
				err = ew
				break
			}
			if nw != nr {
				return n, io.ErrShortWrite
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return n, err
}

func (c *LimitedConn) Close() error {
	c.cancel()
	return c.Conn.Close()
}

func (c *LimitedConn) CloseRead() error {
	if conn, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return conn.CloseRead()
	}
	return nil
}

func (c *LimitedConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return c.Conn.Close()
}

func (c *LimitedConn) SetDeadline(t time.Time) error {
	c.readDeadline.set(t)
	c.writeDeadline.set(t)
	return c.Conn.SetDeadline(t)
}

func (c *LimitedConn) SetReadDeadline(t time.Time) error {
	c.readDeadline.set(t)
	return c.Conn.SetReadDeadline(t)
}

func (c *LimitedConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline.set(t)
	return c.Conn.SetWriteDeadline(t)
}
