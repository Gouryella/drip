package cli

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"drip/internal/client/tcp"
	"drip/internal/shared/protocol"
	"drip/internal/shared/ui"
	"drip/internal/shared/utils"
	"drip/pkg/config"

	"github.com/spf13/cobra"
)

var (
	startAll bool
)

var startCmd = &cobra.Command{
	Use:   "start [tunnel-names...]",
	Short: "Start predefined tunnels from config",
	Long: `Start one or more predefined tunnels from your configuration file.

Examples:
  drip start web              Start the tunnel named "web"
  drip start web api          Start multiple tunnels
  drip start --all            Start all configured tunnels

Configuration file example (~/.drip/config.yaml):
  server: tunnel.example.com:443
  token: your-token
  tls: true
  tunnels:
    - name: web
      type: http
      port: 3000
      subdomain: myapp

    - name: api
      type: http
      port: 8080
      subdomain: api
      transport: wss

    - name: db
      type: tcp
      port: 5432
      subdomain: postgres
      allow_ips:
        - 192.168.0.0/16
        - 10.0.0.0/8`,
	RunE:          runStart,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	startCmd.Flags().BoolVar(&startAll, "all", false, "Start all configured tunnels")
	rootCmd.AddCommand(startCmd)
}

func runStart(_ *cobra.Command, args []string) error {
	cfg, err := config.LoadClientConfig("")
	if err != nil {
		return err
	}

	if len(cfg.Tunnels) == 0 {
		return fmt.Errorf("no tunnels configured in %s", config.DefaultClientConfigPath())
	}

	var tunnelsToStart []*config.TunnelConfig

	if startAll {
		tunnelsToStart = cfg.Tunnels
	} else if len(args) == 0 {
		// No args and no --all flag, show available tunnels
		fmt.Println(ui.Title("Available Tunnels"))
		fmt.Println()
		for _, t := range cfg.Tunnels {
			fmt.Printf("  %s\n", formatTunnelInfo(t))
		}
		fmt.Println()
		fmt.Println("Usage:")
		fmt.Println("  drip start <tunnel-name>    Start a specific tunnel")
		fmt.Println("  drip start --all            Start all tunnels")
		return nil
	} else {
		// Start specific tunnels by name
		for _, name := range args {
			t := cfg.GetTunnel(name)
			if t == nil {
				availableNames := cfg.GetTunnelNames()
				return fmt.Errorf("tunnel '%s' not found. Available tunnels: %s", name, strings.Join(availableNames, ", "))
			}
			tunnelsToStart = append(tunnelsToStart, t)
		}
	}

	if len(tunnelsToStart) == 0 {
		return fmt.Errorf("no tunnels to start")
	}

	for _, t := range tunnelsToStart {
		if err := validateTunnelBandwidth(t); err != nil {
			return err
		}
	}

	// Start tunnels
	if len(tunnelsToStart) == 1 {
		return startSingleTunnel(cfg, tunnelsToStart[0])
	}

	return startMultipleTunnels(cfg, tunnelsToStart)
}

func formatTunnelInfo(t *config.TunnelConfig) string {
	addr := t.Address
	if addr == "" {
		addr = "127.0.0.1"
	}
	info := fmt.Sprintf("%-12s %s  %s:%d", t.Name, t.Type, addr, t.Port)
	if t.Subdomain != "" {
		info += fmt.Sprintf("  (subdomain: %s)", t.Subdomain)
	}
	return info
}

func startSingleTunnel(cfg *config.ClientConfig, t *config.TunnelConfig) error {
	connConfig, err := buildConnectorConfig(cfg, t)
	if err != nil {
		return err
	}

	fmt.Printf("Starting tunnel '%s' (%s %s:%d)\n", t.Name, t.Type, getAddress(t), t.Port)

	return runTunnelWithUI(connConfig, nil)
}

func startMultipleTunnels(cfg *config.ClientConfig, tunnels []*config.TunnelConfig) error {
	if err := utils.InitLogger(verbose); err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	defer utils.Sync()

	logger := utils.GetLogger()

	fmt.Println(ui.Title("Starting Tunnels"))
	fmt.Println()

	var wg sync.WaitGroup
	errChan := make(chan error, len(tunnels))
	stopChan := make(chan struct{})
	var stopOnce sync.Once
	stopAll := func() {
		stopOnce.Do(func() {
			close(stopChan)
		})
	}
	defer stopAll()

	// Handle interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	go func() {
		select {
		case <-sigChan:
			fmt.Println("\nShutting down tunnels...")
			stopAll()
		case <-stopChan:
		}
	}()

	for _, t := range tunnels {
		wg.Add(1)
		go func(tunnel *config.TunnelConfig) {
			defer wg.Done()

			select {
			case <-stopChan:
				return
			default:
			}

			connConfig, err := buildConnectorConfig(cfg, tunnel)
			if err != nil {
				errChan <- err
				stopAll()
				return
			}
			fmt.Printf("  Starting %s (%s %s:%d)...\n", tunnel.Name, tunnel.Type, getAddress(tunnel), tunnel.Port)

			select {
			case <-stopChan:
				return
			default:
			}

			client := newTunnelClient(connConfig, logger)
			connectFinished := make(chan struct{})
			go func() {
				select {
				case <-stopChan:
					_ = client.Close()
				case <-connectFinished:
				}
			}()
			defer close(connectFinished)
			defer client.Close()

			// Connect
			if err := client.Connect(); err != nil {
				errChan <- fmt.Errorf("%s: %w", tunnel.Name, err)
				stopAll()
				return
			}
			fmt.Printf("  ✓ %s: %s\n", tunnel.Name, client.GetURL())

			// Run until stopped
			<-stopChan
		}(t)
	}

	// Wait for interrupt or error
	go func() {
		wg.Wait()
		close(errChan)
	}()

	// Collect errors
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
		fmt.Printf("  ✗ %v\n", err)
	}

	wg.Wait()

	if len(errors) > 0 {
		return fmt.Errorf("%d tunnel(s) failed to start", len(errors))
	}

	return nil
}

func buildConnectorConfig(cfg *config.ClientConfig, t *config.TunnelConfig) (*tcp.ConnectorConfig, error) {
	bw, err := parseBandwidth(t.Bandwidth)
	if err != nil {
		return nil, fmt.Errorf("invalid bandwidth for tunnel '%s': %w", t.Name, err)
	}

	tunnelType := protocol.TunnelTypeHTTP
	switch t.Type {
	case "https":
		tunnelType = protocol.TunnelTypeHTTPS
	case "tcp":
		tunnelType = protocol.TunnelTypeTCP
	}

	transport := tcp.TransportAuto
	switch strings.ToLower(t.Transport) {
	case "tcp", "tls":
		transport = tcp.TransportTCP
	case "wss":
		transport = tcp.TransportWebSocket
	}

	return &tcp.ConnectorConfig{
		ServerAddr:         cfg.Server,
		Token:              cfg.Token,
		TunnelType:         tunnelType,
		LocalHost:          getAddress(t),
		LocalPort:          t.Port,
		Subdomain:          t.Subdomain,
		Insecure:           insecure,
		AllowIPs:           t.AllowIPs,
		DenyIPs:            t.DenyIPs,
		AuthPass:           t.Auth,
		AuthBearer:         t.AuthBearer,
		Transport:          transport,
		Bandwidth:          bw,
		SkipLocalTLSVerify: t.SkipLocalTLSVerify,
	}, nil
}

func getAddress(t *config.TunnelConfig) string {
	if t.Address != "" {
		return t.Address
	}
	return "127.0.0.1"
}

func validateTunnelBandwidth(t *config.TunnelConfig) error {
	_, err := parseBandwidth(t.Bandwidth)
	if err != nil {
		return fmt.Errorf("invalid bandwidth for tunnel '%s': %w", t.Name, err)
	}
	return nil
}
