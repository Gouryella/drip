package cli

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	serverPort                int
	serverPublicPort          int
	serverDomain              string
	serverTunnelDomain        string
	serverAuthToken           string
	serverAllowAnonymous      bool
	serverMetricsToken        string
	serverTrustedProxies      string
	serverDebug               bool
	serverTCPPortMin          int
	serverTCPPortMax          int
	serverTLSCert             string
	serverTLSKey              string
	serverPprofPort           int
	serverTransports          string
	serverTunnelTypes         string
	serverMaxRequestBodyBytes int64
	serverConfigFile          string
)

var serverCmd = &cobra.Command{
	Use:           "server",
	Short:         "Start Drip server",
	Long:          `Start the Drip tunnel server to accept client connections`,
	RunE:          runServer,
	SilenceUsage:  true,
	SilenceErrors: true,
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
	serverCmd.Flags().BoolVar(&serverAllowAnonymous, "allow-anonymous", getEnvBool("DRIP_ALLOW_ANONYMOUS", false), "Allow unauthenticated tunnel registration (env: DRIP_ALLOW_ANONYMOUS)")
	serverCmd.Flags().StringVar(&serverMetricsToken, "metrics-token", getEnvString("DRIP_METRICS_TOKEN", ""), "Metrics and stats token (env: DRIP_METRICS_TOKEN)")
	serverCmd.Flags().StringVar(&serverTrustedProxies, "trusted-proxies", getEnvString("DRIP_TRUSTED_PROXIES", ""), "Comma-separated trusted reverse proxy IPs/CIDRs for X-Forwarded-For and X-Real-IP (env: DRIP_TRUSTED_PROXIES)")
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
	serverCmd.Flags().Int64Var(&serverMaxRequestBodyBytes, "max-request-body-bytes", getEnvInt64("DRIP_MAX_REQUEST_BODY_BYTES", config.DefaultMaxRequestBodyBytes), "Maximum tunneled HTTP request body size in bytes; 0 disables the limit and is high-risk (env: DRIP_MAX_REQUEST_BODY_BYTES)")
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
	maxRequestBodyBytesExplicit := cfg != nil && cfg.MaxRequestBodyBytesExplicit()
	if cfg == nil {
		cfg = &config.ServerConfig{
			MaxRequestBodyBytes: config.DefaultMaxRequestBodyBytes,
		}
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

	// AllowAnonymous
	if cmd.Flags().Changed("allow-anonymous") {
		cfg.AllowAnonymous = serverAllowAnonymous
	} else if os.Getenv("DRIP_ALLOW_ANONYMOUS") != "" {
		cfg.AllowAnonymous = serverAllowAnonymous
	}

	// MetricsToken
	if cmd.Flags().Changed("metrics-token") {
		cfg.MetricsToken = serverMetricsToken
	} else if os.Getenv("DRIP_METRICS_TOKEN") != "" {
		cfg.MetricsToken = serverMetricsToken
	}

	// TrustedProxies
	if cmd.Flags().Changed("trusted-proxies") {
		cfg.TrustedProxies = parseCommaSeparated(serverTrustedProxies)
	} else if os.Getenv("DRIP_TRUSTED_PROXIES") != "" {
		cfg.TrustedProxies = parseCommaSeparated(serverTrustedProxies)
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

	// MaxRequestBodyBytes
	if cmd.Flags().Changed("max-request-body-bytes") {
		cfg.MaxRequestBodyBytes = serverMaxRequestBodyBytes
		maxRequestBodyBytesExplicit = true
	} else if os.Getenv("DRIP_MAX_REQUEST_BODY_BYTES") != "" {
		cfg.MaxRequestBodyBytes = serverMaxRequestBodyBytes
		maxRequestBodyBytesExplicit = true
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

			mux := http.NewServeMux()
			mux.HandleFunc("/debug/pprof/", pprof.Index)
			mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

			srv := &http.Server{
				Addr:              pprofAddr,
				Handler:           mux,
				ReadHeaderTimeout: 5 * time.Second,
				ReadTimeout:       10 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       120 * time.Second,
			}
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

	httpHandler := proxy.NewHandler(proxy.HandlerConfig{
		Manager:                    tunnelManager,
		Logger:                     logger,
		ServerDomain:               cfg.Domain,
		TunnelDomain:               cfg.TunnelDomain,
		AuthToken:                  cfg.AuthToken,
		MetricsToken:               cfg.MetricsToken,
		TrustedProxies:             cfg.TrustedProxies,
		MaxRequestBodyBytes:        cfg.MaxRequestBodyBytes,
		UnsafeUnlimitedRequestBody: maxRequestBodyBytesExplicit && cfg.MaxRequestBodyBytes == 0,
	})
	httpHandler.SetAllowedTransports(cfg.AllowedTransports)
	httpHandler.SetAllowedTunnelTypes(cfg.AllowedTunnelTypes)

	listener := tcp.NewListener(tcp.ListenerConfig{
		Address:        listenAddr,
		TLSConfig:      tlsConfig,
		AuthToken:      cfg.AuthToken,
		AllowAnonymous: cfg.AllowAnonymous,
		Manager:        tunnelManager,
		Logger:         logger,
		PortAlloc:      portAllocator,
		Domain:         cfg.Domain,
		TunnelDomain:   cfg.TunnelDomain,
		PublicPort:     cfg.PublicPort,
		HTTPHandler:    httpHandler,
	})
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
	if cfg.MaxRequestBodyBytes > 0 {
		logger.Info("HTTP request body limit configured",
			zap.Int64("max_request_body_bytes", cfg.MaxRequestBodyBytes),
		)
	} else {
		logger.Warn("HTTP request body limit disabled; this is a high-risk public-server configuration")
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
		zap.Strings("trusted_proxies", cfg.TrustedProxies),
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

// getEnvInt returns the environment variable value as int, or defaultVal if not set
func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

func getEnvInt64(key string, defaultVal int64) int64 {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			return i
		}
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return defaultVal
	}
}

// getEnvString returns the environment variable value, or defaultVal if not set
func getEnvString(key string, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// parseCommaSeparated splits a comma-separated string into a slice
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
