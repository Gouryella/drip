package tcp

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// SessionScaler manages automatic scaling of yamux sessions based on load.
type SessionScaler struct {
	client *PoolClient
	logger *zap.Logger
	stopCh <-chan struct{}
	wg     *sync.WaitGroup

	// Scaling configuration
	checkInterval      time.Duration
	scaleUpCooldown    time.Duration
	scaleDownCooldown  time.Duration
	capacityPerSession int64
	scaleUpLoad        float64
	scaleDownLoad      float64
	burstThreshold     float64
	maxBurstAdd        int
}

// NewSessionScaler creates a new session scaler.
func NewSessionScaler(
	client *PoolClient,
	logger *zap.Logger,
	stopCh <-chan struct{},
	wg *sync.WaitGroup,
) *SessionScaler {
	return &SessionScaler{
		client:             client,
		logger:             logger,
		stopCh:             stopCh,
		wg:                 wg,
		checkInterval:      1 * time.Second,
		scaleUpCooldown:    1 * time.Second,
		scaleDownCooldown:  60 * time.Second,
		capacityPerSession: 256,
		scaleUpLoad:        0.6,
		scaleDownLoad:      0.2,
		burstThreshold:     0.9,
		maxBurstAdd:        4,
	}
}

// Start starts the scaler loop.
func (s *SessionScaler) Start() {
	s.wg.Add(1)
	go s.scalerLoop()
}

// scalerLoop monitors load and adjusts session count.
func (s *SessionScaler) scalerLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
		}

		s.client.mu.Lock()
		desired := s.client.desiredTotal
		if desired == 0 {
			desired = s.client.initialSessions
			s.client.desiredTotal = desired
		}
		lastScale := s.client.lastScale
		s.client.mu.Unlock()

		current := s.client.sessionCount()
		if current == 0 {
			continue
		}

		activeConns := s.client.stats.GetActiveConnections()
		capacity := int64(current) * s.capacityPerSession
		load := float64(activeConns) / float64(capacity)

		now := time.Now()
		// Replace lost sessions even when traffic is low. Load-based scaling
		// alone never restores the pool after a data connection disconnects.
		if current < desired {
			if now.Sub(lastScale) >= s.scaleUpCooldown {
				_ = s.client.addDataSession()
				s.client.mu.Lock()
				s.client.lastScale = now
				s.client.mu.Unlock()
			}
			continue
		}

		// Burst scaling: rapid scale-up under extreme load
		if load > s.burstThreshold && current < s.client.maxSessions {
			toAdd := min(s.maxBurstAdd, s.client.maxSessions-current)
			s.logger.Info("Burst scaling up sessions",
				zap.Int("current", current),
				zap.Int("adding", toAdd),
				zap.Float64("load", load),
			)
			added := 0
			for i := 0; i < toAdd; i++ {
				if err := s.client.addDataSession(); err != nil {
					break
				}
				added++
			}
			s.client.mu.Lock()
			s.client.desiredTotal = current + added
			s.client.lastScale = now
			s.client.mu.Unlock()
			continue
		}

		// Scale up: add sessions when load is high
		if load > s.scaleUpLoad && current < s.client.maxSessions {
			if now.Sub(lastScale) < s.scaleUpCooldown {
				continue
			}
			newDesired := min(desired+1, s.client.maxSessions)
			if newDesired > desired {
				s.logger.Debug("Scaling up sessions",
					zap.Int("current", current),
					zap.Int("desired", newDesired),
					zap.Float64("load", load),
				)
				err := s.client.addDataSession()
				s.client.mu.Lock()
				if err == nil {
					s.client.desiredTotal = newDesired
				}
				s.client.lastScale = now
				s.client.mu.Unlock()
			}
			continue
		}

		// Scale down: remove idle sessions when load is low
		if load < s.scaleDownLoad && current > s.client.minSessions {
			if now.Sub(lastScale) < s.scaleDownCooldown {
				continue
			}
			newDesired := max(desired-1, s.client.minSessions)
			if newDesired < desired && current > newDesired {
				s.logger.Debug("Scaling down sessions",
					zap.Int("current", current),
					zap.Int("desired", newDesired),
					zap.Float64("load", load),
				)
				removed := s.client.removeIdleSessions(1)
				s.client.mu.Lock()
				if removed > 0 {
					s.client.desiredTotal = newDesired
				}
				s.client.lastScale = now
				s.client.mu.Unlock()
			}
		}
	}
}
