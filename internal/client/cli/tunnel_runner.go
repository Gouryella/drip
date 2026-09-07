package cli

import (
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"drip/internal/client/tcp"
	"drip/internal/shared/tuning"
	"drip/internal/shared/ui"
	"drip/internal/shared/utils"

	"go.uber.org/zap"
)

const (
	maxReconnectAttempts     = 5
	reconnectBaseInterval    = 1 * time.Second
	reconnectMaxInterval     = 30 * time.Second
	reconnectStableResetTime = 30 * time.Second
)

var (
	newTunnelClient   = tcp.NewTunnelClient
	nextReconnectWait = defaultReconnectWait
	reconnectTimer    = time.After
	tunnelRunnerNow   = time.Now
)

var errTunnelDisconnected = errors.New("connection lost")

func runTunnelWithUI(connConfig *tcp.ConnectorConfig, daemonInfo *DaemonInfo) error {
	tuning.ApplyMode(tuning.ModeClient)

	if err := utils.InitLogger(verbose); err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	defer utils.Sync()

	logger := utils.GetLogger()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	reconnectAttempts := 0
	reconnectingAfterDisconnect := false
	var lastErr error
	for {
		connector := newTunnelClient(connConfig, logger)

		fmt.Println(ui.RenderConnecting(connConfig.ServerAddr, reconnectAttempts, maxReconnectAttempts))

		connectDone := make(chan error, 1)
		go func() { connectDone <- connector.Connect() }()
		var connectErr error
		select {
		case connectErr = <-connectDone:
		case <-quit:
			_ = connector.Close()
			fmt.Println(ui.RenderShuttingDown())
			return nil
		}
		if err := connectErr; err != nil {
			_ = connector.Close()
			lastErr = err
			if isConfigurationError(err) {
				fmt.Println(ui.Warning(fmt.Sprintf("Configuration error: %v", err)))
				return fmt.Errorf("configuration error: %w", err)
			}
			if isNonRetryableError(err) {
				fmt.Println(ui.RenderConnectionFailed(err))
				return err
			}

			reconnectAttempts++
			if reconnectAttempts >= maxReconnectAttempts {
				return fmt.Errorf("failed to connect after %d attempts: %w", maxReconnectAttempts, lastErr)
			}
			wait := nextReconnectWait(reconnectAttempts)
			fmt.Println(ui.RenderConnectionFailed(err))
			fmt.Println(ui.RenderRetrying(wait))

			select {
			case <-quit:
				fmt.Println(ui.RenderShuttingDown())
				return nil
			case <-reconnectTimer(wait):
				continue
			}
		}

		if !reconnectingAfterDisconnect {
			reconnectAttempts = 0
		}
		connectedAt := tunnelRunnerNow()
		if assignedSubdomain := connector.GetSubdomain(); assignedSubdomain != "" {
			connConfig.Subdomain = assignedSubdomain
			if daemonInfo != nil {
				daemonInfo.Subdomain = assignedSubdomain
			}
		}

		if daemonInfo != nil {
			daemonInfo.URL = connector.GetURL()
			if err := SaveDaemonInfo(daemonInfo); err != nil {
				logger.Warn("Failed to save daemon info", zap.Error(err))
			}
		}

		displayAddr := connConfig.LocalHost
		if displayAddr == "127.0.0.1" {
			displayAddr = "localhost"
		}

		status := &ui.TunnelStatus{
			Type:      string(connConfig.TunnelType),
			URL:       connector.GetURL(),
			LocalAddr: fmt.Sprintf("%s:%d", displayAddr, connConfig.LocalPort),
		}

		fmt.Print(ui.RenderTunnelConnected(status))

		latencyCh := make(chan time.Duration, 1)
		connector.SetLatencyCallback(func(latency time.Duration) {
			select {
			case latencyCh <- latency:
			default:
			}
		})

		stopDisplay := make(chan struct{})
		disconnected := make(chan struct{})

		go func() {
			renderTicker := time.NewTicker(1 * time.Second)
			defer renderTicker.Stop()

			var lastLatency time.Duration
			lastRenderedLines := 0

			for {
				select {
				case latency := <-latencyCh:
					lastLatency = latency
				case <-renderTicker.C:
					stats := connector.GetStats()
					if stats == nil {
						continue
					}

					stats.UpdateSpeed()
					snapshot := stats.GetSnapshot()

					status.Latency = lastLatency
					status.BytesIn = snapshot.TotalBytesIn
					status.BytesOut = snapshot.TotalBytesOut
					status.SpeedIn = float64(snapshot.SpeedIn)
					status.SpeedOut = float64(snapshot.SpeedOut)

					if status.Type == "tcp" {
						if snapshot.SpeedIn == 0 && snapshot.SpeedOut == 0 {
							status.TotalRequest = 0
						} else {
							status.TotalRequest = snapshot.ActiveConnections
						}
					} else {
						status.TotalRequest = snapshot.TotalRequests
					}

					statsView := ui.RenderTunnelStats(status)
					if lastRenderedLines > 0 {
						fmt.Print(clearLines(lastRenderedLines))
					}

					fmt.Print(statsView)
					lastRenderedLines = countRenderedLines(statsView)
				case <-stopDisplay:
					return
				}
			}
		}()

		go func() {
			connector.Wait()
			close(disconnected)
		}()

		select {
		case <-quit:
			close(stopDisplay)
			fmt.Println()
			fmt.Println(ui.RenderShuttingDown())

			// Close with timeout (wait for ongoing requests to complete)
			done := make(chan struct{})
			go func() {
				_ = connector.Close()
				close(done)
			}()

			select {
			case <-done:
				// Closed successfully
			case <-time.After(2 * time.Second):
				fmt.Println(ui.Warning("Force closing (timeout)..."))
				select {
				case <-done:
				case <-time.After(500 * time.Millisecond):
				}
			}

			if daemonInfo != nil {
				_ = RemoveDaemonInfo(daemonInfo.Type, daemonInfo.Port)
			}
			fmt.Println(ui.Success("Tunnel closed"))
			return nil
		case <-disconnected:
			close(stopDisplay)
			fmt.Println()
			fmt.Println(ui.RenderConnectionLost())
			if tunnelRunnerNow().Sub(connectedAt) >= reconnectStableResetTime {
				reconnectAttempts = 0
			}
			lastErr = errTunnelDisconnected
			reconnectAttempts++
			reconnectingAfterDisconnect = true
			if reconnectAttempts >= maxReconnectAttempts {
				return fmt.Errorf("connection lost after %d reconnect attempts: %w", maxReconnectAttempts, lastErr)
			}
			wait := nextReconnectWait(reconnectAttempts)
			fmt.Println(ui.RenderRetrying(wait))

			select {
			case <-quit:
				fmt.Println(ui.RenderShuttingDown())
				return nil
			case <-reconnectTimer(wait):
				continue
			}
		}
	}
}

func defaultReconnectWait(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	wait := reconnectBaseInterval
	for i := 1; i < attempt; i++ {
		if wait >= reconnectMaxInterval {
			wait = reconnectMaxInterval
			break
		}
		wait *= 2
		if wait > reconnectMaxInterval {
			wait = reconnectMaxInterval
			break
		}
	}

	jitterRange := wait / 2
	if jitterRange <= 0 {
		return wait
	}
	return wait/2 + randomDuration(jitterRange)
}

func randomDuration(maxInclusive time.Duration) time.Duration {
	if maxInclusive <= 0 {
		return 0
	}

	n, err := crand.Int(crand.Reader, big.NewInt(int64(maxInclusive)+1))
	if err != nil {
		return maxInclusive / 2
	}
	return time.Duration(n.Int64())
}

func clearLines(lines int) string {
	if lines <= 0 {
		return ""
	}
	return fmt.Sprintf("\033[%dA\033[J", lines)
}

func countRenderedLines(block string) int {
	if block == "" {
		return 0
	}

	lines := strings.Count(block, "\n")
	if !strings.HasSuffix(block, "\n") {
		lines++
	}

	return lines
}

func isNonRetryableError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "subdomain is already taken") ||
		strings.Contains(errStr, "subdomain is reserved") ||
		strings.Contains(errStr, "invalid subdomain") ||
		strings.Contains(errStr, "authentication") ||
		strings.Contains(errStr, "Invalid authentication token")
}

// isConfigurationError returns true for errors caused by user configuration
// that won't be fixed by retrying (e.g., wrong transport type)
func isConfigurationError(err error) bool {
	errStr := err.Error()
	return strings.Contains(errStr, "server only supports")
}
