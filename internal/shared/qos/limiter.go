package qos

import (
	"golang.org/x/time/rate"
)

type Config struct {
	Bandwidth int64
	Burst     int
}

type Limiter struct {
	limiter *rate.Limiter
}

func NewLimiter(cfg Config) *Limiter {
	l := &Limiter{}
	if cfg.Bandwidth > 0 {
		burst := cfg.Burst
		if burst <= 0 {
			burst = int(cfg.Bandwidth * 2)
		}
		l.limiter = rate.NewLimiter(rate.Limit(cfg.Bandwidth), burst)
	}
	return l
}

func (l *Limiter) RateLimiter() *rate.Limiter {
	return l.limiter
}

func (l *Limiter) IsLimited() bool {
	return l.limiter != nil
}
