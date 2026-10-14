package qos

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// connDeadline wakes token waiters when a net.Conn deadline changes. The
// underlying socket deadline alone cannot interrupt a rate limiter's timer.
type connDeadline struct {
	mu      sync.Mutex
	time    time.Time
	changed chan struct{}
}

func (d *connDeadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.time = t
	if d.changed != nil {
		close(d.changed)
	}
	d.changed = make(chan struct{})
}

func (d *connDeadline) snapshot() (time.Time, <-chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.changed == nil {
		d.changed = make(chan struct{})
	}
	return d.time, d.changed
}

func (c *LimitedConn) waitN(n int, deadline *connDeadline) (err error) {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	if d, _ := deadline.snapshot(); !d.IsZero() && !now.Before(d) {
		return os.ErrDeadlineExceeded
	}
	reservation := c.limiter.RateLimiter().ReserveN(now, n)
	if !reservation.OK() {
		return fmt.Errorf("rate limit reservation exceeds burst: %d bytes", n)
	}
	delay := reservation.DelayFrom(now)
	if delay <= 0 {
		return nil
	}
	defer func() {
		if err != nil {
			reservation.Cancel()
		}
	}()
	readyAt := now.Add(delay)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		d, changed := deadline.snapshot()
		wait := time.Until(readyAt)
		if !d.IsZero() {
			remaining := time.Until(d)
			if remaining <= 0 {
				return os.ErrDeadlineExceeded
			}
			wait = min(wait, remaining)
		}
		if wait <= 0 {
			return c.ctx.Err()
		}
		timer.Reset(wait)
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-changed:
		case <-timer.C:
		}
	}
}
