package stats

import (
	"sync"
	"sync/atomic"
	"time"
)

type TrafficStats struct {
	totalBytesIn  int64
	totalBytesOut int64

	totalRequests     int64
	activeConnections int64

	lastBytesIn  int64
	lastBytesOut int64
	lastTime     time.Time
	speedMu      sync.Mutex

	speedIn  int64
	speedOut int64

	startTime time.Time
}

func NewTrafficStats() *TrafficStats {
	now := time.Now()
	return &TrafficStats{
		startTime: now,
		lastTime:  now,
	}
}

func (s *TrafficStats) AddBytesIn(n int64) {
	atomic.AddInt64(&s.totalBytesIn, n)
}

func (s *TrafficStats) AddBytesOut(n int64) {
	atomic.AddInt64(&s.totalBytesOut, n)
}

func (s *TrafficStats) AddRequest() {
	atomic.AddInt64(&s.totalRequests, 1)
}

func (s *TrafficStats) IncActiveConnections() {
	atomic.AddInt64(&s.activeConnections, 1)
}

func (s *TrafficStats) DecActiveConnections() {
	for {
		old := atomic.LoadInt64(&s.activeConnections)
		if old <= 0 {
			return
		}
		if atomic.CompareAndSwapInt64(&s.activeConnections, old, old-1) {
			return
		}
	}
}

func (s *TrafficStats) GetTotalBytesIn() int64 {
	return atomic.LoadInt64(&s.totalBytesIn)
}

func (s *TrafficStats) GetTotalBytesOut() int64 {
	return atomic.LoadInt64(&s.totalBytesOut)
}

func (s *TrafficStats) GetTotalRequests() int64 {
	return atomic.LoadInt64(&s.totalRequests)
}

func (s *TrafficStats) GetActiveConnections() int64 {
	return atomic.LoadInt64(&s.activeConnections)
}

func (s *TrafficStats) GetTotalBytes() int64 {
	return s.GetTotalBytesIn() + s.GetTotalBytesOut()
}

func (s *TrafficStats) UpdateSpeed() {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()

	now := time.Now()
	elapsed := now.Sub(s.lastTime).Seconds()
	if elapsed < 0.1 {
		return
	}

	currentIn := atomic.LoadInt64(&s.totalBytesIn)
	currentOut := atomic.LoadInt64(&s.totalBytesOut)

	deltaIn := currentIn - s.lastBytesIn
	deltaOut := currentOut - s.lastBytesOut

	if deltaIn > 0 {
		s.speedIn = int64(float64(deltaIn) / elapsed)
	} else {
		s.speedIn = 0
	}

	if deltaOut > 0 {
		s.speedOut = int64(float64(deltaOut) / elapsed)
	} else {
		s.speedOut = 0
	}

	s.lastBytesIn = currentIn
	s.lastBytesOut = currentOut
	s.lastTime = now
}

func (s *TrafficStats) GetSpeedIn() int64 {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()
	return s.speedIn
}

func (s *TrafficStats) GetSpeedOut() int64 {
	s.speedMu.Lock()
	defer s.speedMu.Unlock()
	return s.speedOut
}

func (s *TrafficStats) GetUptime() time.Duration {
	return time.Since(s.startTime)
}

type Snapshot struct {
	TotalBytesIn      int64
	TotalBytesOut     int64
	TotalBytes        int64
	TotalRequests     int64
	ActiveConnections int64
	SpeedIn           int64
	SpeedOut          int64
	Uptime            time.Duration
}

func (s *TrafficStats) GetSnapshot() Snapshot {
	s.speedMu.Lock()
	speedIn := s.speedIn
	speedOut := s.speedOut
	s.speedMu.Unlock()

	totalIn := atomic.LoadInt64(&s.totalBytesIn)
	totalOut := atomic.LoadInt64(&s.totalBytesOut)
	active := atomic.LoadInt64(&s.activeConnections)

	return Snapshot{
		TotalBytesIn:      totalIn,
		TotalBytesOut:     totalOut,
		TotalBytes:        totalIn + totalOut,
		TotalRequests:     atomic.LoadInt64(&s.totalRequests),
		ActiveConnections: active,
		SpeedIn:           speedIn,
		SpeedOut:          speedOut,
		Uptime:            time.Since(s.startTime),
	}
}
