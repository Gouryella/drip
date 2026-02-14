package qos

import (
	"context"
	"io"
	"net"
)

type LimitedConn struct {
	net.Conn
	limiter *Limiter
	ctx     context.Context
}

func NewLimitedConn(ctx context.Context, conn net.Conn, limiter *Limiter) *LimitedConn {
	return &LimitedConn{
		Conn:    conn,
		limiter: limiter,
		ctx:     ctx,
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
		if waitErr := c.limiter.RateLimiter().WaitN(c.ctx, n); waitErr != nil {
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

		if err := c.limiter.RateLimiter().WaitN(c.ctx, chunk); err != nil {
			return total, err
		}

		nw, err := c.Conn.Write(b[:chunk])
		total += nw
		if err != nil {
			return total, err
		}
		b = b[chunk:]
	}

	return total, nil
}

func (c *LimitedConn) ReadFrom(r io.Reader) (n int64, err error) {
	buf := make([]byte, 32*1024)
	for {
		nr, er := r.Read(buf)
		if nr > 0 {
			nw, ew := c.Write(buf[:nr])
			n += int64(nw)
			if ew != nil {
				err = ew
				break
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
	buf := make([]byte, 32*1024)
	for {
		nr, er := c.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			n += int64(nw)
			if ew != nil {
				err = ew
				break
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
