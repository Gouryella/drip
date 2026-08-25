package cli

import (
	"fmt"
	"strconv"
	"strings"

	"drip/internal/client/tcp"
	"drip/internal/shared/protocol"
	"drip/internal/shared/units"

	"github.com/spf13/cobra"
)

var (
	subdomain          string
	daemonMode         bool
	daemonMarker       bool
	localAddress       string
	allowIPs           []string
	denyIPs            []string
	authPass           string
	authBearer         string
	transport          string
	bandwidth          string
	skipLocalTLSVerify bool
)

var httpCmd = &cobra.Command{
	Use:   "http <port>",
	Short: "Start HTTP tunnel",
	Long: `Start an HTTP tunnel to expose a local HTTP server.

Example:
  drip http 3000                    Tunnel localhost:3000
  drip http 8080 --subdomain myapp  Use custom subdomain
  drip http 3000 --allow-ip 192.168.0.0/16  Only allow IPs from 192.168.x.x
  drip http 3000 --allow-ip 10.0.0.1        Allow single IP
  drip http 3000 --deny-ip 1.2.3.4          Block specific IP
  drip http 3000 --auth secret              Enable proxy authentication with password
  drip http 3000 --auth-bearer sk-xxx       Enable proxy authentication with bearer token
  drip http 3000 --transport wss            Use WebSocket over TLS (CDN-friendly)
  drip http 3000 --bandwidth 1M             Limit bandwidth to 1 MB/s

Configuration:
  First time: Run 'drip config init' to save server and token
  Subsequent: Just run 'drip http <port>'

Transport options:
  auto  - Automatically select based on server address (default)
  tcp   - Direct TLS 1.3 connection
  wss   - WebSocket over TLS (works through CDN like Cloudflare)

Bandwidth format:
  1K, 1KB  - 1 kilobyte per second (1024 bytes/s)
  1M, 1MB  - 1 megabyte per second (1048576 bytes/s)
  1G, 1GB  - 1 gigabyte per second
  1024     - 1024 bytes per second (raw number)`,
	Args:          cobra.ExactArgs(1),
	RunE:          runHTTP,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	httpCmd.Flags().StringVarP(&subdomain, "subdomain", "n", "", "Custom subdomain (optional)")
	httpCmd.Flags().BoolVarP(&daemonMode, "daemon", "d", false, "Run in background (daemon mode)")
	httpCmd.Flags().StringVarP(&localAddress, "address", "a", "127.0.0.1", "Local address to forward to (default: 127.0.0.1)")
	httpCmd.Flags().StringSliceVar(&allowIPs, "allow-ip", nil, "Allow only these IPs or CIDR ranges (e.g., 192.168.1.1,10.0.0.0/8)")
	httpCmd.Flags().StringSliceVar(&denyIPs, "deny-ip", nil, "Deny these IPs or CIDR ranges (e.g., 1.2.3.4,192.168.1.0/24)")
	httpCmd.Flags().StringVar(&authPass, "auth", "", "Password for proxy authentication")
	httpCmd.Flags().StringVar(&authBearer, "auth-bearer", "", "Bearer token for proxy authentication")
	httpCmd.Flags().StringVar(&transport, "transport", "auto", "Transport protocol: auto, tcp, wss (WebSocket over TLS)")
	httpCmd.Flags().StringVar(&bandwidth, "bandwidth", "", "Bandwidth limit (e.g., 1M, 500K, 1G)")
	httpCmd.Flags().BoolVar(&skipLocalTLSVerify, "skip-local-tls-verify", false, "Skip TLS verification for local HTTPS backends")
	httpCmd.Flags().BoolVar(&daemonMarker, "daemon-child", false, "Internal flag for daemon child process")
	_ = httpCmd.Flags().MarkHidden("daemon-child")
	rootCmd.AddCommand(httpCmd)
}

func runHTTP(_ *cobra.Command, args []string) error {
	port, err := strconv.Atoi(args[0])
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port number: %s", args[0])
	}

	if daemonMode && !daemonMarker {
		return StartDaemon("http", port, buildDaemonArgs("http", args, subdomain, localAddress))
	}

	if authPass != "" && authBearer != "" {
		return fmt.Errorf("cannot use --auth and --auth-bearer together")
	}

	serverAddr, token, err := resolveServerAddrAndToken("http", port)
	if err != nil {
		return err
	}

	bw, err := parseBandwidth(bandwidth)
	if err != nil {
		return err
	}

	connConfig := &tcp.ConnectorConfig{
		ServerAddr:         serverAddr,
		Token:              token,
		TunnelType:         protocol.TunnelTypeHTTP,
		LocalHost:          localAddress,
		LocalPort:          port,
		Subdomain:          subdomain,
		Insecure:           insecure,
		AllowIPs:           allowIPs,
		DenyIPs:            denyIPs,
		AuthPass:           authPass,
		AuthBearer:         authBearer,
		Transport:          parseTransport(transport),
		Bandwidth:          bw,
		SkipLocalTLSVerify: skipLocalTLSVerify,
	}

	var daemon *DaemonInfo
	if daemonMarker {
		daemon = newDaemonInfo("http", port, subdomain, serverAddr)
	}

	return runTunnelWithUI(connConfig, daemon)
}

func parseTransport(s string) tcp.TransportType {
	switch strings.ToLower(s) {
	case "wss":
		return tcp.TransportWebSocket
	case "tcp", "tls":
		return tcp.TransportTCP
	default:
		return tcp.TransportAuto
	}
}

func parseBandwidth(s string) (int64, error) {
	return units.ParseBandwidth(s)
}
