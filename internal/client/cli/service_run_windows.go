//go:build windows

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"drip/internal/shared/utils"
	"drip/pkg/config"

	"go.uber.org/zap"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// Event log identifiers. Windows only groups messages by these, it does not
// interpret them.
const (
	eventIDStarted = 1
	eventIDStopped = 2
	eventIDFailed  = 3
)

// serviceStopTimeout bounds how long the supervisor gets to close its tunnels
// after a stop request. The service control manager kills the process well before
// its own timeout, so this stays short.
const serviceStopTimeout = 20 * time.Second

// runService is the entry point the service manager launches. Run from a console
// it supervises the same tunnels in the foreground, which is how a service that
// will not stay up gets debugged.
func runService(opts serviceRunOptions) error {
	if opts.configPath == "" {
		opts.configPath = defaultServiceConfigPath()
	}
	if opts.logPath == "" {
		opts.logPath = defaultServiceLogPath()
	}

	// Everything downstream that falls back to the default config path — error
	// messages included — must agree with the file the service actually reads.
	if err := os.Setenv("DRIP_CONFIG", opts.configPath); err != nil {
		return fmt.Errorf("failed to set DRIP_CONFIG: %w", err)
	}

	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("failed to detect the service environment: %w", err)
	}

	if !isService {
		return runServiceForeground(opts)
	}

	if err := utils.InitFileLogger(opts.logPath, opts.verbose); err != nil {
		return err
	}
	defer utils.Sync()

	handler := &dripService{opts: opts, logger: utils.GetLogger()}

	// The event log is where an administrator looks first, but it is not worth
	// refusing to start over: the file log carries the same information.
	if elog, err := eventlog.Open(opts.name); err == nil {
		handler.elog = elog
		defer func() { _ = elog.Close() }()
	} else {
		handler.logger.Warn("Failed to open the event log source", zap.Error(err))
	}

	if err := svc.Run(opts.name, handler); err != nil {
		handler.logger.Error("Service failed", zap.Error(err))
		return fmt.Errorf("service failed: %w", err)
	}

	return nil
}

// runServiceForeground supervises the tunnels without the service manager, logging
// to the console.
func runServiceForeground(opts serviceRunOptions) error {
	if err := utils.InitLogger(opts.verbose); err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	defer utils.Sync()

	cfg, tunnels, err := loadServiceTunnels(opts)
	if err != nil {
		return err
	}

	fmt.Printf("Supervising %d tunnel(s) from %s. Press Ctrl+C to stop.\n", len(tunnels), opts.configPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return superviseTunnels(ctx, cfg, tunnels, utils.GetLogger())
}

func loadServiceTunnels(opts serviceRunOptions) (*config.ClientConfig, []*config.TunnelConfig, error) {
	cfg, err := config.LoadClientConfig(opts.configPath)
	if err != nil {
		return nil, nil, err
	}

	tunnels, err := selectTunnels(cfg, opts.all, opts.tunnels)
	if err != nil {
		return nil, nil, err
	}

	return cfg, tunnels, nil
}

// dripService adapts the tunnel supervisor to the service control manager.
type dripService struct {
	opts   serviceRunOptions
	logger *zap.Logger
	elog   *eventlog.Log
}

// Execute never reports svc.Stopped itself: svc.Run does that once Execute
// returns, and it carries the exit code with it. Reporting Stopped early would
// tell the service manager the process stopped cleanly, and a service that
// stopped cleanly does not get its recovery actions applied.
func (s *dripService) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending, WaitHint: 15000}

	// The config is read here rather than before svc.Run so a bad config reports a
	// real exit code to the service manager instead of a start timeout.
	cfg, tunnels, err := loadServiceTunnels(s.opts)
	if err != nil {
		s.report(eventIDFailed, fmt.Sprintf("Drip service cannot start: %v", err), true)
		return false, 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- superviseTunnels(ctx, cfg, tunnels, s.logger)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}
	s.report(eventIDStarted, fmt.Sprintf("Drip service started with %d tunnel(s) from %s", len(tunnels), s.opts.configPath), false)

	for {
		select {
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus

			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending, WaitHint: uint32(serviceStopTimeout / time.Millisecond)}
				cancel()

				select {
				case <-done:
				case <-time.After(serviceStopTimeout):
					s.logger.Warn("Timed out closing tunnels, stopping anyway")
				}

				s.report(eventIDStopped, "Drip service stopped", false)
				return false, 0

			default:
				s.logger.Warn("Unexpected service control request", zap.Uint32("cmd", uint32(request.Cmd)))
			}

		case err := <-done:
			// The supervisor gave up on every tunnel. Exiting non-zero is what makes
			// the service manager apply the configured recovery actions.
			if err != nil {
				s.report(eventIDFailed, fmt.Sprintf("Drip service stopped: %v", err), true)
				return false, 1
			}

			s.report(eventIDStopped, "Drip service stopped", false)
			return false, 0
		}
	}
}

// report writes one line to both the file log and the Windows event log.
func (s *dripService) report(eventID uint32, message string, isError bool) {
	if isError {
		s.logger.Error(message)
	} else {
		s.logger.Info(message)
	}

	if s.elog == nil {
		return
	}

	var err error
	if isError {
		err = s.elog.Error(eventID, message)
	} else {
		err = s.elog.Info(eventID, message)
	}
	if err != nil {
		s.logger.Warn("Failed to write to the event log", zap.Error(err))
	}
}
