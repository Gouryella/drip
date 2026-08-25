package cli

import (
	"context"
	cryptotls "crypto/tls"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"drip/internal/server/admin"
	"drip/internal/server/auth"
	"drip/internal/server/proxy"
	"drip/internal/server/reservations"
	"drip/internal/server/store"
	"drip/internal/server/tcp"
	servertls "drip/internal/server/tls"
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
	serverMetricsToken        string
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
	serverDBPath              string
	serverRequireAuth         bool
	serverReservationsOnly    bool
	serverAdminAddress        string
	serverTLSMode             string
	serverACMEEmail           string
	serverACMEDNSProvider     string
	serverACMEDNSToken        string
	serverACMECA              string
	serverACMECacheDir        string
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
	serverCmd.Flags().StringVar(&serverMetricsToken, "metrics-token", getEnvString("DRIP_METRICS_TOKEN", ""), "Metrics and stats token (env: DRIP_METRICS_TOKEN)")
	serverCmd.Flags().BoolVar(&serverDebug, "debug", false, "Enable debug logging")
	serverCmd.Flags().IntVar(&serverTCPPortMin, "tcp-port-min", getEnvInt("DRIP_TCP_PORT_MIN", constants.DefaultTCPPortMin), "Minimum TCP tunnel port (env: DRIP_TCP_PORT_MIN)")
	serverCmd.Flags().IntVar(&serverTCPPortMax, "tcp-port-max", getEnvInt("DRIP_TCP_PORT_MAX", constants.DefaultTCPPortMax), "Maximum TCP tunnel port (env: DRIP_TCP_PORT_MAX)")

	// TLS options
	serverCmd.Flags().StringVar(&serverTLSCert, "tls-cert", getEnvString("DRIP_TLS_CERT", ""), "Path to TLS certificate file (env: DRIP_TLS_CERT)")
	serverCmd.Flags().StringVar(&serverTLSKey, "tls-key", getEnvString("DRIP_TLS_KEY", ""), "Path to TLS private key file (env: DRIP_TLS_KEY)")

	// ACME / automatic TLS
	serverCmd.Flags().StringVar(&serverTLSMode, "tls-mode", getEnvString("DRIP_TLS_MODE", ""), "TLS mode: none, manual or acme (env: DRIP_TLS_MODE; inferred when unset)")
	serverCmd.Flags().StringVar(&serverACMEEmail, "acme-email", getEnvString("DRIP_ACME_EMAIL", ""), "ACME account email for expiry notices (env: DRIP_ACME_EMAIL)")
	serverCmd.Flags().StringVar(&serverACMEDNSProvider, "acme-dns-provider", getEnvString("DRIP_ACME_DNS_PROVIDER", ""), "DNS provider for the ACME DNS-01 challenge: "+strings.Join(servertls.SupportedDNSProviders(), ", ")+" (env: DRIP_ACME_DNS_PROVIDER)")
	serverCmd.Flags().StringVar(&serverACMEDNSToken, "acme-dns-token", getEnvString("DRIP_ACME_DNS_TOKEN", ""), "Scoped DNS provider API token (env: DRIP_ACME_DNS_TOKEN)")
	serverCmd.Flags().StringVar(&serverACMECA, "acme-ca", getEnvString("DRIP_ACME_CA", ""), "ACME CA: production, staging, or a directory URL (env: DRIP_ACME_CA)")
	serverCmd.Flags().StringVar(&serverACMECacheDir, "acme-cache-dir", getEnvString("DRIP_ACME_CACHE_DIR", ""), "Directory for certificates and the ACME account key (env: DRIP_ACME_CACHE_DIR)")

	// Performance profiling
	serverCmd.Flags().IntVar(&serverPprofPort, "pprof", getEnvInt("DRIP_PPROF_PORT", 0), "Enable pprof on specified port (env: DRIP_PPROF_PORT)")

	// Transport and tunnel type restrictions
	serverCmd.Flags().StringVar(&serverTransports, "transports", getEnvString("DRIP_TRANSPORTS", "tcp,wss"), "Allowed transports: tcp,wss (env: DRIP_TRANSPORTS)")
	serverCmd.Flags().StringVar(&serverTunnelTypes, "tunnel-types", getEnvString("DRIP_TUNNEL_TYPES", "http,https,tcp"), "Allowed tunnel types: http,https,tcp (env: DRIP_TUNNEL_TYPES)")
	// Control plane
	serverCmd.Flags().StringVar(&serverDBPath, "db", getEnvString("DRIP_DB_PATH", ""), "Path to the control plane SQLite database; enables client credentials and reservations (env: DRIP_DB_PATH)")
	serverCmd.Flags().StringVar(&serverAdminAddress, "admin", getEnvString("DRIP_ADMIN_ADDRESS", ""), "Serve the admin panel on this address, e.g. 127.0.0.1:8444 (env: DRIP_ADMIN_ADDRESS)")
	serverCmd.Flags().BoolVar(&serverReservationsOnly, "reservations-only", getEnvBool("DRIP_RESERVATIONS_ONLY", false), "Reject registrations that do not bind a reservation (env: DRIP_RESERVATIONS_ONLY)")
	serverCmd.Flags().BoolVar(&serverRequireAuth, "require-auth", getEnvBool("DRIP_REQUIRE_AUTH", false), "Reject registrations without a recognised credential (env: DRIP_REQUIRE_AUTH)")

	serverCmd.Flags().Int64Var(&serverMaxRequestBodyBytes, "max-request-body-bytes", getEnvInt64("DRIP_MAX_REQUEST_BODY_BYTES", 0), "Maximum tunneled HTTP request body size in bytes; 0 disables the limit (env: DRIP_MAX_REQUEST_BODY_BYTES)")
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

	// MaxRequestBodyBytes
	if cmd.Flags().Changed("max-request-body-bytes") {
		cfg.MaxRequestBodyBytes = serverMaxRequestBodyBytes
	} else if os.Getenv("DRIP_MAX_REQUEST_BODY_BYTES") != "" {
		cfg.MaxRequestBodyBytes = serverMaxRequestBodyBytes
	}

	// DBPath
	if cmd.Flags().Changed("db") {
		cfg.DBPath = serverDBPath
	} else if os.Getenv("DRIP_DB_PATH") != "" {
		cfg.DBPath = serverDBPath
	}

	// RequireAuth
	if cmd.Flags().Changed("require-auth") {
		cfg.RequireAuth = serverRequireAuth
	} else if os.Getenv("DRIP_REQUIRE_AUTH") != "" {
		cfg.RequireAuth = serverRequireAuth
	}

	// AdminAddress
	if cmd.Flags().Changed("admin") {
		cfg.AdminAddress = serverAdminAddress
	} else if os.Getenv("DRIP_ADMIN_ADDRESS") != "" {
		cfg.AdminAddress = serverAdminAddress
	}

	// ReservationsOnly
	if cmd.Flags().Changed("reservations-only") {
		cfg.ReservationsOnly = serverReservationsOnly
	} else if os.Getenv("DRIP_RESERVATIONS_ONLY") != "" {
		cfg.ReservationsOnly = serverReservationsOnly
	}

	// TLSEnabled is the pre-mode switch; it still selects manual mode so old
	// configuration files and environments keep working unchanged.
	if os.Getenv("DRIP_TLS_ENABLED") != "" {
		cfg.TLSEnabled = os.Getenv("DRIP_TLS_ENABLED") == "true" || os.Getenv("DRIP_TLS_ENABLED") == "1"
	} else if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
		if !cfg.TLSEnabled {
			cfg.TLSEnabled = true
		}
	}

	// TLSMode
	if cmd.Flags().Changed("tls-mode") {
		cfg.TLSMode = serverTLSMode
	} else if os.Getenv("DRIP_TLS_MODE") != "" {
		cfg.TLSMode = serverTLSMode
	}

	// ACME settings
	if cmd.Flags().Changed("acme-email") || os.Getenv("DRIP_ACME_EMAIL") != "" {
		cfg.ACME.Email = serverACMEEmail
	}
	if cmd.Flags().Changed("acme-dns-provider") || os.Getenv("DRIP_ACME_DNS_PROVIDER") != "" {
		cfg.ACME.DNSProvider = serverACMEDNSProvider
	}
	if cmd.Flags().Changed("acme-dns-token") || os.Getenv("DRIP_ACME_DNS_TOKEN") != "" {
		cfg.ACME.DNSAPIToken = serverACMEDNSToken
	}
	if cmd.Flags().Changed("acme-ca") || os.Getenv("DRIP_ACME_CA") != "" {
		cfg.ACME.CA = serverACMECA
	}
	if cmd.Flags().Changed("acme-cache-dir") || os.Getenv("DRIP_ACME_CACHE_DIR") != "" {
		cfg.ACME.CacheDir = serverACMECacheDir
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

	tlsMode, err := cfg.ResolveTLSMode()
	if err != nil {
		logger.Fatal("Invalid TLS configuration", zap.Error(err))
	}

	var (
		tlsConfig   *cryptotls.Config
		acmeManager *servertls.ACMEManager
	)

	switch servertls.Mode(tlsMode) {
	case servertls.ModeNone:
		logger.Info("TLS disabled - running in plain TCP mode (for reverse proxy)")

	case servertls.ModeManual:
		tlsConfig, err = servertls.LoadManual(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			logger.Fatal("Failed to load TLS certificate", zap.Error(err))
		}
		logger.Info("TLS 1.3 enabled with manual certificates",
			zap.String("cert", cfg.TLSCertFile),
			zap.String("key", cfg.TLSKeyFile),
		)

	case servertls.ModeACME:
		acmeManager, err = servertls.NewACME(context.Background(), servertls.ACMEConfig{
			Domain:             cfg.Domain,
			TunnelDomain:       cfg.TunnelDomain,
			Names:              cfg.ACME.Domains,
			Email:              cfg.ACME.Email,
			CA:                 cfg.ACME.CA,
			CacheDir:           cfg.ACME.CacheDir,
			PropagationTimeout: time.Duration(cfg.ACME.PropagationTimeoutSeconds) * time.Second,
			Resolvers:          cfg.ACME.Resolvers,
			DNS: servertls.DNSProviderConfig{
				Name:     cfg.ACME.DNSProvider,
				APIToken: cfg.ACME.DNSAPIToken,
			},
			Logger: logger,
		})
		if err != nil {
			logger.Fatal("Failed to obtain ACME certificates", zap.Error(err))
		}
		defer acmeManager.Close()

		tlsConfig = acmeManager.TLSConfig()
		logger.Info("TLS 1.3 enabled with ACME certificates",
			zap.Strings("names", acmeManager.Names()),
		)
	}

	// Control plane: open the database and build the authenticator before the
	// listener, so registrations can resolve credentials from the first packet.
	var controlStore *store.Store
	if cfg.DBPath != "" {
		controlStore, err = store.Open(cfg.DBPath)
		if err != nil {
			logger.Fatal("Failed to open control plane database", zap.Error(err))
		}
		defer func() {
			if cerr := controlStore.Close(); cerr != nil {
				logger.Error("Error closing control plane database", zap.Error(cerr))
			}
		}()

		version, verr := controlStore.SchemaVersion(context.Background())
		if verr != nil {
			logger.Fatal("Failed to read control plane schema version", zap.Error(verr))
		}
		logger.Info("Control plane database ready",
			zap.String("path", cfg.DBPath),
			zap.Int("schema_version", version),
		)
	}

	authenticator := auth.New(auth.Config{
		Store:          controlStore,
		LegacyToken:    cfg.AuthToken,
		AllowAnonymous: !cfg.RequireAuth && cfg.AuthToken == "" && controlStore == nil,
		Logger:         logger,
	})

	defer authenticator.Close()
	authenticator.StartPurgeTask(5 * time.Minute)

	if controlStore == nil && cfg.AuthToken == "" && !cfg.RequireAuth {
		logger.Warn("Server is accepting unauthenticated tunnel registrations; " +
			"set db_path for client credentials, or token for a shared secret")
	}

	resolver := reservations.New(controlStore, cfg.ReservationsOnly, logger)
	if cfg.ReservationsOnly {
		logger.Info("Reservations-only mode: registrations must bind a reservation")
	}

	tunnelManager := tunnel.NewManager(logger)

	portAllocator, err := tcp.NewPortAllocator(cfg.TCPPortMin, cfg.TCPPortMax)
	if err != nil {
		logger.Fatal("Invalid TCP port range", zap.Error(err))
	}

	listenAddr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)

	httpHandler := proxy.NewHandler(proxy.HandlerConfig{
		Manager:             tunnelManager,
		Logger:              logger,
		ServerDomain:        cfg.Domain,
		TunnelDomain:        cfg.TunnelDomain,
		AuthToken:           cfg.AuthToken,
		MetricsToken:        cfg.MetricsToken,
		MaxRequestBodyBytes: cfg.MaxRequestBodyBytes,
	})
	httpHandler.SetAllowedTransports(cfg.AllowedTransports)
	httpHandler.SetAllowedTunnelTypes(cfg.AllowedTunnelTypes)

	listener := tcp.NewListener(tcp.ListenerConfig{
		Address:       listenAddr,
		TLSConfig:     tlsConfig,
		AuthToken:     cfg.AuthToken,
		Authenticator: authenticator,
		Resolver:      resolver,
		Manager:       tunnelManager,
		Logger:        logger,
		PortAlloc:     portAllocator,
		Domain:        cfg.Domain,
		TunnelDomain:  cfg.TunnelDomain,
		PublicPort:    cfg.PublicPort,
		HTTPHandler:   httpHandler,
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
	}

	if err := listener.Start(); err != nil {
		logger.Fatal("Failed to start TCP listener", zap.Error(err))
	}

	protocol := "TCP (plain)"
	if tlsConfig != nil {
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

	if cfg.AdminAddress != "" {
		adminServer, aerr := admin.New(admin.Config{
			Store:         controlStore,
			Manager:       tunnelManager,
			Authenticator: authenticator,
			Address:       cfg.AdminAddress,
			TLSConfig:     tlsConfig,
			SessionTTL:    time.Duration(cfg.AdminSessionHours) * time.Hour,
			Deployment: admin.Deployment{
				Domain:       cfg.Domain,
				TunnelDomain: cfg.TunnelDomain,
				PublicPort:   cfg.PublicPort,
				TLS:          tlsConfig != nil,
			},
			Logger: logger,
		})
		if aerr != nil {
			logger.Fatal("Failed to configure admin panel", zap.Error(aerr))
		}
		if aerr := adminServer.Start(); aerr != nil {
			logger.Fatal("Failed to start admin panel", zap.Error(aerr))
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if serr := adminServer.Stop(shutdownCtx); serr != nil {
				logger.Error("Error stopping admin panel", zap.Error(serr))
			}
		}()

		count, cerr := controlStore.CountAdminUsers(context.Background())
		if cerr == nil && count == 0 {
			logger.Warn("Admin panel has no administrator yet; the first-run setup screen is open to anyone who can reach it",
				zap.String("address", cfg.AdminAddress),
			)
		}
	}

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

// getEnvBool returns the environment variable value as bool, or defaultVal if not set
func getEnvBool(key string, defaultVal bool) bool {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	return val == "true" || val == "1" || val == "yes"
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
