package cli

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"drip/internal/server/proxy"
	"drip/internal/server/tcp"
	"drip/internal/server/tunnel"
	"drip/internal/shared/constants"
	"drip/internal/shared/tuning"
	"drip/internal/shared/utils"
	"drip/pkg/config"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var (
	serverPort         int
	serverPublicPort   int
	serverDomain       string
	serverTunnelDomain string
	serverAuthToken    string
	serverMetricsToken string
	serverDebug        bool
	serverTCPPortMin   int
	serverTCPPortMax   int
	serverTLSCert      string
	serverTLSKey       string
	serverPprofPort    int
	serverTransports   string
	serverTunnelTypes  string
	serverConfigFile   string
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start Drip server",
	Long:  `Start the Drip tunnel server to accept client connections`,
	RunE:  runServer,
}

func init() {
	rootCmd.AddCommand(serverCmd)

	// Config file flag
	serverCmd.Flags().StringVarP(&serverConfigFile, "config", "c", "", "Path to config file (default: /etc/drip/config.yaml or ~/.drip/server.yaml)")

	// Command line flags with environment variable defaults
	serverCmd.Flags().IntVarP(&serverPort, "port", "p", getEnvInt("DRIP_PORT", 8443), "Server port (env: DRIP_PORT)")
	serverCmd.Flags().IntVar(&serverPublicPort, "public-port", getEnvInt("DRIP_PUBLIC_PORT", 0), "Public port to display in URLs (env: DRIP_PUBLIC_PORT)")
	serverCmd.Flags().StringVarP(&serverDomain, "domain", "d", getEnvString("DRIP_DOMAIN", constants.DefaultDomain), "Server domain for client connections (env: DRIP_DOMAIN)")
	serverCmd.Flags().StringVar(&serverTunnelDomain, "tunnel-domain", getEnvString("DRIP_TUNNEL_DOMAIN", ""), "Domain for tunnel URLs, defaults to --domain (env: DRIP_TUNNEL_DOMAIN)")
	serverCmd.Flags().StringVarP(&serverAuthToken, "token", "t", getEnvString("DRIP_TOKEN", ""), "Authentication token (env: DRIP_TOKEN)")
	serverCmd.Flags().StringVar(&serverMetricsToken, "metrics-token", getEnvString("DRIP_METRICS_TOKEN", ""), "Metrics and stats token (env: DRIP_METRICS_TOKEN)")
	serverCmd.Flags().BoolVar(&serverDebug, "debug", false, "Enable debug logging")
	serverCmd.Flags().IntVar(&serverTCPPortMin, "tcp-port-min", getEnvInt("DRIP_TCP_PORT_MIN", constants.DefaultTCPPortMin), "Minimum TCP tunnel port (env: DRIP_TCP_PORT_MIN)")
	serverCmd.Flags().IntVar(&serverTCPPortMax, "tcp-port-max", getEnvInt("DRIP_TCP_PORT_MAX", constants.DefaultTCPPortMax), "Maximum TCP tunnel port (env: DRIP_TCP_PORT_MAX)")

	// TLS options
	serverCmd.Flags().StringVar(&serverTLSCert, "tls-cert", getEnvString("DRIP_TLS_CERT", ""), "Path to TLS certificate file (env: DRIP_TLS_CERT)")
	serverCmd.Flags().StringVar(&serverTLSKey, "tls-key", getEnvString("DRIP_TLS_KEY", ""), "Path to TLS private key file (env: DRIP_TLS_KEY)")

	// Performance profiling
	serverCmd.Flags().IntVar(&serverPprofPort, "pprof", getEnvInt("DRIP_PPROF_PORT", 0), "Enable pprof on specified port (env: DRIP_PPROF_PORT)")

	// Transport and tunnel type restrictions
	serverCmd.Flags().StringVar(&serverTransports, "transports", getEnvString("DRIP_TRANSPORTS", "tcp,wss"), "Allowed transports: tcp,wss (env: DRIP_TRANSPORTS)")
	serverCmd.Flags().StringVar(&serverTunnelTypes, "tunnel-types", getEnvString("DRIP_TUNNEL_TYPES", "http,https,tcp"), "Allowed tunnel types: http,https,tcp (env: DRIP_TUNNEL_TYPES)")
}

func runServer(cmd *cobra.Command, _ []string) error {
	// Apply server-mode GC tuning (high throughput, more memory)
	tuning.ApplyMode(tuning.ModeServer)

	// Load config file if specified or if default exists
	var cfg *config.ServerConfig
	configPath := serverConfigFile
	if configPath == "" && config.ServerConfigExists("") {
		configPath = config.DefaultServerConfigPath()
	}
	if configPath != "" {
		var err error
		cfg, err = config.LoadServerConfig(configPath)
		if err != nil {
			return fmt.Errorf("failed to load config file: %w", err)
		}
	}
	if cfg == nil {
		cfg = &config.ServerConfig{}
	}

	// Port
	if cmd.Flags().Changed("port") {
		cfg.Port = serverPort
	} else if os.Getenv("DRIP_PORT") != "" {
		cfg.Port = serverPort
	} else if cfg.Port == 0 {
		cfg.Port = serverPort
	}

	// PublicPort
	if cmd.Flags().Changed("public-port") {
		cfg.PublicPort = serverPublicPort
	} else if os.Getenv("DRIP_PUBLIC_PORT") != "" {
		cfg.PublicPort = serverPublicPort
	}

	// Domain
	if cmd.Flags().Changed("domain") {
		cfg.Domain = serverDomain
	} else if os.Getenv("DRIP_DOMAIN") != "" {
		cfg.Domain = serverDomain
	} else if cfg.Domain == "" {
		cfg.Domain = serverDomain
	}

	// TunnelDomain
	if cmd.Flags().Changed("tunnel-domain") {
		cfg.TunnelDomain = serverTunnelDomain
	} else if os.Getenv("DRIP_TUNNEL_DOMAIN") != "" {
		cfg.TunnelDomain = serverTunnelDomain
	}

	// AuthToken
	if cmd.Flags().Changed("token") {
		cfg.AuthToken = serverAuthToken
	} else if os.Getenv("DRIP_TOKEN") != "" {
		cfg.AuthToken = serverAuthToken
	}

	// MetricsToken
	if cmd.Flags().Changed("metrics-token") {
		cfg.MetricsToken = serverMetricsToken
	} else if os.Getenv("DRIP_METRICS_TOKEN") != "" {
		cfg.MetricsToken = serverMetricsToken
	}

	// Debug
	if cmd.Flags().Changed("debug") {
		cfg.Debug = serverDebug
	}

	// TCPPortMin
	if cmd.Flags().Changed("tcp-port-min") {
		cfg.TCPPortMin = serverTCPPortMin
	} else if os.Getenv("DRIP_TCP_PORT_MIN") != "" {
		cfg.TCPPortMin = serverTCPPortMin
	} else if cfg.TCPPortMin == 0 {
		cfg.TCPPortMin = serverTCPPortMin
	}

	// TCPPortMax
	if cmd.Flags().Changed("tcp-port-max") {
		cfg.TCPPortMax = serverTCPPortMax
	} else if os.Getenv("DRIP_TCP_PORT_MAX") != "" {
		cfg.TCPPortMax = serverTCPPortMax
	} else if cfg.TCPPortMax == 0 {
		cfg.TCPPortMax = serverTCPPortMax
	}

	// TLSCertFile
	if cmd.Flags().Changed("tls-cert") {
		cfg.TLSCertFile = serverTLSCert
	} else if os.Getenv("DRIP_TLS_CERT") != "" {
		cfg.TLSCertFile = serverTLSCert
	}

	// TLSKeyFile
	if cmd.Flags().Changed("tls-key") {
		cfg.TLSKeyFile = serverTLSKey
	} else if os.Getenv("DRIP_TLS_KEY") != "" {
		cfg.TLSKeyFile = serverTLSKey
	}

	// PprofPort
	if cmd.Flags().Changed("pprof") {
		cfg.PprofPort = serverPprofPort
	} else if os.Getenv("DRIP_PPROF_PORT") != "" {
		cfg.PprofPort = serverPprofPort
	}

	// AllowedTransports
	if cmd.Flags().Changed("transports") {
		cfg.AllowedTransports = parseCommaSeparated(serverTransports)
	} else if os.Getenv("DRIP_TRANSPORTS") != "" {
		cfg.AllowedTransports = parseCommaSeparated(serverTransports)
	} else if len(cfg.AllowedTransports) == 0 {
		cfg.AllowedTransports = parseCommaSeparated(serverTransports)
	}

	// AllowedTunnelTypes
	if cmd.Flags().Changed("tunnel-types") {
		cfg.AllowedTunnelTypes = parseCommaSeparated(serverTunnelTypes)
	} else if os.Getenv("DRIP_TUNNEL_TYPES") != "" {
		cfg.AllowedTunnelTypes = parseCommaSeparated(serverTunnelTypes)
	} else if len(cfg.AllowedTunnelTypes) == 0 {
		cfg.AllowedTunnelTypes = parseCommaSeparated(serverTunnelTypes)
	}

	// TLSEnabled
	if os.Getenv("DRIP_TLS_ENABLED") != "" {
		cfg.TLSEnabled = os.Getenv("DRIP_TLS_ENABLED") == "true" || os.Getenv("DRIP_TLS_ENABLED") == "1"
	} else if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		if !cfg.TLSEnabled {
			cfg.TLSEnabled = true
		}
	}

	if cfg.TLSEnabled {
		if cfg.TLSCertFile == "" {
			return fmt.Errorf("TLS certificate path is required when TLS is enabled (use --tls-cert flag, DRIP_TLS_CERT environment variable, or config file)")
		}
		if cfg.TLSKeyFile == "" {
			return fmt.Errorf("TLS private key path is required when TLS is enabled (use --tls-key flag, DRIP_TLS_KEY environment variable, or config file)")
		}
	}

	if err := utils.InitServerLogger(cfg.Debug); err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	defer utils.Sync()

	logger := utils.GetLogger()

	if configPath != "" {
		logger.Info("Loaded configuration from file", zap.String("path", configPath))
	}

	logger.Info("Starting Drip Server",
		zap.String("version", Version),
		zap.String("commit", GitCommit),
	)

	if cfg.PprofPort > 0 {
		go func() {
			pprofAddr := fmt.Sprintf("localhost:%d", cfg.PprofPort)
			logger.Info("Starting pprof server", zap.String("address", pprofAddr))
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				logger.Error("pprof server failed", zap.Error(err))
			}
		}()
	}

	// Set public port for display if not specified
	if cfg.PublicPort == 0 {
		cfg.PublicPort = cfg.Port
	}

	// Use tunnel domain if not set, fall back to domain
	if cfg.TunnelDomain == "" {
		cfg.TunnelDomain = cfg.Domain
	}

	if err := cfg.Validate(); err != nil {
		logger.Fatal("Invalid server configuration", zap.Error(err))
	}

	tlsConfig, err := cfg.LoadTLSConfig()
	if err != nil {
		logger.Fatal("Failed to load TLS configuration", zap.Error(err))
	}

	if cfg.TLSEnabled {
		logger.Info("TLS 1.3 configuration loaded",
			zap.String("cert", cfg.TLSCertFile),
			zap.String("key", cfg.TLSKeyFile),
		)
	} else {
		logger.Info("TLS disabled - running in plain TCP mode (for reverse proxy)")
	}

	tunnelManager := tunnel.NewManager(logger)

	portAllocator, err := tcp.NewPortAllocator(cfg.TCPPortMin, cfg.TCPPortMax)
	if err != nil {
		logger.Fatal("Invalid TCP port range", zap.Error(err))
	}

	listenAddr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)

	httpHandler := proxy.NewHandler(tunnelManager, logger, cfg.Domain, cfg.TunnelDomain, cfg.AuthToken, cfg.MetricsToken)
	httpHandler.SetAllowedTransports(cfg.AllowedTransports)
	httpHandler.SetAllowedTunnelTypes(cfg.AllowedTunnelTypes)

	listener := tcp.NewListener(listenAddr, tlsConfig, cfg.AuthToken, tunnelManager, logger, portAllocator, cfg.Domain, cfg.TunnelDomain, cfg.PublicPort, httpHandler)
	listener.SetAllowedTransports(cfg.AllowedTransports)
	listener.SetAllowedTunnelTypes(cfg.AllowedTunnelTypes)

	bandwidth, err := parseBandwidth(cfg.Bandwidth)
	if err != nil {
		logger.Fatal("Invalid bandwidth configuration", zap.Error(err))
	}
	burstMultiplier := cfg.BurstMultiplier
	if burstMultiplier <= 0 {
		burstMultiplier = 2.0
	}
	listener.SetBandwidth(bandwidth)
	listener.SetBurstMultiplier(burstMultiplier)
	if bandwidth > 0 {
		logger.Info("Bandwidth limit configured",
			zap.String("bandwidth", cfg.Bandwidth),
			zap.Int64("bandwidth_bytes_sec", bandwidth),
			zap.Float64("burst_multiplier", burstMultiplier),
		)
	}

	if err := listener.Start(); err != nil {
		logger.Fatal("Failed to start TCP listener", zap.Error(err))
	}

	protocol := "TCP (plain)"
	if cfg.TLSEnabled {
		protocol = "TCP over TLS 1.3"
	}

	logger.Info("Drip Server started",
		zap.String("address", listenAddr),
		zap.String("domain", cfg.Domain),
		zap.String("tunnel_domain", cfg.TunnelDomain),
		zap.String("protocol", protocol),
		zap.Strings("transports", cfg.AllowedTransports),
		zap.Strings("tunnel_types", cfg.AllowedTunnelTypes),
	)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	<-quit

	logger.Info("Shutting down server...")

	if err := listener.Stop(); err != nil {
		logger.Error("Error stopping listener", zap.Error(err))
	}

	logger.Info("Server stopped")
	return nil
}

func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

func getEnvString(key string, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func parseCommaSeparated(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, strings.ToLower(p))
		}
	}
	return result
}
