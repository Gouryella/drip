package cli

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
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
	serverAuthToken    string
	serverMetricsToken string
	serverDebug        bool
	serverTCPPortMin   int
	serverTCPPortMax   int
	serverTLSCert      string
	serverTLSKey       string
	serverPprofPort    int
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start Drip server",
	Long:  `Start the Drip tunnel server to accept client connections`,
	RunE:  runServer,
}

func init() {
	rootCmd.AddCommand(serverCmd)

	// Command line flags with environment variable defaults
	serverCmd.Flags().IntVarP(&serverPort, "port", "p", getEnvInt("DRIP_PORT", 8443), "Server port (env: DRIP_PORT)")
	serverCmd.Flags().IntVar(&serverPublicPort, "public-port", getEnvInt("DRIP_PUBLIC_PORT", 0), "Public port to display in URLs (env: DRIP_PUBLIC_PORT)")
	serverCmd.Flags().StringVarP(&serverDomain, "domain", "d", getEnvString("DRIP_DOMAIN", constants.DefaultDomain), "Server domain (env: DRIP_DOMAIN)")
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
}

func runServer(_ *cobra.Command, _ []string) error {
	// Apply server-mode GC tuning (high throughput, more memory)
	tuning.ApplyMode(tuning.ModeServer)

	if serverTLSCert == "" {
		return fmt.Errorf("TLS certificate path is required (use --tls-cert flag or DRIP_TLS_CERT environment variable)")
	}
	if serverTLSKey == "" {
		return fmt.Errorf("TLS private key path is required (use --tls-key flag or DRIP_TLS_KEY environment variable)")
	}

	if err := utils.InitServerLogger(serverDebug); err != nil {
		return fmt.Errorf("failed to initialize logger: %w", err)
	}
	defer utils.Sync()

	logger := utils.GetLogger()

	logger.Info("Starting Drip Server",
		zap.String("version", Version),
		zap.String("commit", GitCommit),
	)

	if serverPprofPort > 0 {
		go func() {
			pprofAddr := fmt.Sprintf("localhost:%d", serverPprofPort)
			logger.Info("Starting pprof server", zap.String("address", pprofAddr))
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				logger.Error("pprof server failed", zap.Error(err))
			}
		}()
	}

	displayPort := serverPublicPort
	if displayPort == 0 {
		displayPort = serverPort
	}

	serverConfig := &config.ServerConfig{
		Port:        serverPort,
		PublicPort:  displayPort,
		Domain:      serverDomain,
		TCPPortMin:  serverTCPPortMin,
		TCPPortMax:  serverTCPPortMax,
		TLSEnabled:  true,
		TLSCertFile: serverTLSCert,
		TLSKeyFile:  serverTLSKey,
		AuthToken:   serverAuthToken,
		Debug:       serverDebug,
	}

	if err := serverConfig.Validate(); err != nil {
		logger.Fatal("Invalid server configuration", zap.Error(err))
	}

	tlsConfig, err := serverConfig.LoadTLSConfig()
	if err != nil {
		logger.Fatal("Failed to load TLS configuration", zap.Error(err))
	}

	logger.Info("TLS 1.3 configuration loaded",
		zap.String("cert", serverTLSCert),
		zap.String("key", serverTLSKey),
	)

	tunnelManager := tunnel.NewManager(logger)

	portAllocator, err := tcp.NewPortAllocator(serverTCPPortMin, serverTCPPortMax)
	if err != nil {
		logger.Fatal("Invalid TCP port range", zap.Error(err))
	}

	listenAddr := fmt.Sprintf("0.0.0.0:%d", serverPort)

	httpHandler := proxy.NewHandler(tunnelManager, logger, serverDomain, serverAuthToken, serverMetricsToken)

	listener := tcp.NewListener(listenAddr, tlsConfig, serverAuthToken, tunnelManager, logger, portAllocator, serverDomain, displayPort, httpHandler)

	if err := listener.Start(); err != nil {
		logger.Fatal("Failed to start TCP listener", zap.Error(err))
	}

	logger.Info("Drip Server started",
		zap.String("address", listenAddr),
		zap.String("domain", serverDomain),
		zap.String("protocol", "TCP over TLS 1.3"),
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

// getEnvString returns the environment variable value, or defaultVal if not set
func getEnvString(key string, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
