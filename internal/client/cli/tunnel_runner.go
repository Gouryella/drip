package cli

import (
	"fmt"
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
	maxReconnectAttempts = 5
	reconnectInterval    = 3 * time.Second
)

func runTunnelWithUI(connConfig *tcp.ConnectorConfig, daemonInfo *DaemonInfo) error {
	tuning.ApplyMode(tuning.ModeClient)

	if err := utils.InitLogger(verbose); err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	defer utils.Sync()

	logger := utils.GetLogger()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	reconnectAttempts := 0
	for {
		connector := tcp.NewTunnelClient(connConfig, logger)

		fmt.Println(ui.RenderConnecting(connConfig.ServerAddr, reconnectAttempts, maxReconnectAttempts))

		if err := connector.Connect(); err != nil {
			if isNonRetryableError(err) {
				return fmt.Errorf("failed to connect: %w", err)
			}

			reconnectAttempts++
			if reconnectAttempts >= maxReconnectAttempts {
				return fmt.Errorf("failed to connect after %d attempts: %w", maxReconnectAttempts, err)
			}
			fmt.Println(ui.RenderConnectionFailed(err))
			fmt.Println(ui.RenderRetrying(reconnectInterval))

			select {
			case <-quit:
				fmt.Println(ui.RenderShuttingDown())
				return nil
			case <-time.After(reconnectInterval):
				continue
			}
		}

		reconnectAttempts = 0
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
				connector.Close()
				close(done)
			}()

			select {
			case <-done:
				// Closed successfully
			case <-time.After(2 * time.Second):
				fmt.Println(ui.Warning("Force closing (timeout)..."))
			}

			if daemonInfo != nil {
				RemoveDaemonInfo(daemonInfo.Type, daemonInfo.Port)
			}
			fmt.Println(ui.Success("Tunnel closed"))
			return nil
		case <-disconnected:
			close(stopDisplay)
			fmt.Println()
			fmt.Println(ui.RenderConnectionLost())
			reconnectAttempts++
			if reconnectAttempts >= maxReconnectAttempts {
				return fmt.Errorf("connection lost after %d reconnect attempts", maxReconnectAttempts)
			}
			fmt.Println(ui.RenderRetrying(reconnectInterval))

			select {
			case <-quit:
				fmt.Println(ui.RenderShuttingDown())
				return nil
			case <-time.After(reconnectInterval):
				continue
			}
		}
	}
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
