package cli

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"drip/internal/client/tcp"
	"drip/internal/shared/tuning"
	"drip/pkg/config"

	"go.uber.org/zap"
)

const (
	superviseMinBackoff   = 3 * time.Second
	superviseMaxBackoff   = 2 * time.Minute
	superviseCloseTimeout = 5 * time.Second
)

// superviseTunnels keeps every tunnel in tunnels connected until ctx is cancelled.
//
// It is the runner behind the Windows service, and it deliberately differs from
// runTunnelWithUI: there is no terminal to draw on, and it never stops retrying a
// transport failure. A service that exits because the network was not up yet at
// boot, or because the server was restarting, is worse than one that waits.
// Errors that retrying cannot fix (a reserved subdomain owned by somebody else,
// a rejected token) end that one tunnel and are logged.
func superviseTunnels(ctx context.Context, cfg *config.ClientConfig, tunnels []*config.TunnelConfig, logger *zap.Logger) error {
	if len(tunnels) == 0 {
		return errors.New("no tunnels to start")
	}

	tuning.ApplyMode(tuning.ModeClient)

	connConfigs := make([]*tcp.ConnectorConfig, 0, len(tunnels))
	for _, tunnel := range tunnels {
		connConfig, err := buildConnectorConfig(cfg, tunnel)
		if err != nil {
			return err
		}
		connConfigs = append(connConfigs, connConfig)
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed []error
	)

	for i, tunnel := range tunnels {
		wg.Add(1)
		go func(name string, connConfig *tcp.ConnectorConfig) {
			defer wg.Done()

			if err := superviseTunnel(ctx, name, connConfig, logger); err != nil {
				mu.Lock()
				failed = append(failed, err)
				mu.Unlock()
			}
		}(tunnel.Name, connConfigs[i])
	}

	wg.Wait()

	// A partial failure keeps the service up: the tunnels that still work are
	// worth more than a clean exit. Only a total loss is reported as a failure,
	// so the service manager restarts a process that is doing nothing at all.
	if ctx.Err() == nil && len(failed) == len(tunnels) {
		return fmt.Errorf("all %d tunnel(s) stopped: %w", len(tunnels), errors.Join(failed...))
	}

	return nil
}

// superviseTunnel runs a single tunnel, reconnecting with exponential backoff
// until ctx is cancelled or the tunnel hits an error retrying cannot fix.
func superviseTunnel(ctx context.Context, name string, connConfig *tcp.ConnectorConfig, logger *zap.Logger) error {
	backoff := superviseMinBackoff
	connectedOnce := false

	for {
		if ctx.Err() != nil {
			return nil
		}

		client := tcp.NewTunnelClient(connConfig, logger)

		if err := client.Connect(); err != nil {
			if isFatalTunnelError(err, connectedOnce) {
				logger.Error("Tunnel cannot start",
					zap.String("tunnel", name),
					zap.Error(err))
				return fmt.Errorf("tunnel %s: %w", name, err)
			}

			wait := jitterBackoff(backoff)
			logger.Warn("Tunnel connection failed, retrying",
				zap.String("tunnel", name),
				zap.Duration("retry_in", wait),
				zap.Error(err))

			if !sleepContext(ctx, wait) {
				return nil
			}
			backoff = nextBackoff(backoff)
			continue
		}

		connectedOnce = true
		backoff = superviseMinBackoff

		// Reconnect onto the name the server assigned, so the URL survives a drop.
		if assigned := client.GetSubdomain(); assigned != "" {
			connConfig.Subdomain = assigned
		}

		logger.Info("Tunnel connected",
			zap.String("tunnel", name),
			zap.String("url", client.GetURL()),
			zap.String("local", fmt.Sprintf("%s:%d", connConfig.LocalHost, connConfig.LocalPort)))

		disconnected := make(chan struct{})
		go func() {
			client.Wait()
			close(disconnected)
		}()

		select {
		case <-ctx.Done():
			closeTunnelClient(client)
			logger.Info("Tunnel stopped", zap.String("tunnel", name))
			return nil

		case <-disconnected:
			closeTunnelClient(client)

			wait := jitterBackoff(backoff)
			logger.Warn("Tunnel connection lost, reconnecting",
				zap.String("tunnel", name),
				zap.Duration("retry_in", wait))

			if !sleepContext(ctx, wait) {
				return nil
			}
			backoff = nextBackoff(backoff)
		}
	}
}

// closeTunnelClient closes the client, giving in-flight requests a moment to
// finish before abandoning the wait.
func closeTunnelClient(client tcp.TunnelClient) {
	done := make(chan struct{})
	go func() {
		_ = client.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(superviseCloseTimeout):
	}
}

// isFatalTunnelError reports whether err ends the tunnel for good.
func isFatalTunnelError(err error, connectedOnce bool) bool {
	if isConfigurationError(err) {
		return true
	}
	if !isNonRetryableError(err) {
		return false
	}

	// After a dropped connection the server can still hold the previous session
	// for a few seconds, so a tunnel that already owned its name treats "already
	// taken" as transient. Giving up there would leave a service permanently down
	// because of its own stale session.
	if connectedOnce && strings.Contains(err.Error(), "already taken") {
		return false
	}

	return true
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > superviseMaxBackoff {
		return superviseMaxBackoff
	}
	return next
}

// jitterBackoff spreads reconnects by ±20%, so a fleet that lost the same server
// does not come back in lockstep.
func jitterBackoff(backoff time.Duration) time.Duration {
	spread := int64(backoff) / 5
	if spread <= 0 {
		return backoff
	}
	return time.Duration(int64(backoff) - spread + rand.Int63n(2*spread+1)) // #nosec G404 -- spreading reconnects, not a security decision
}

// sleepContext waits for d, reporting false if ctx was cancelled first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
