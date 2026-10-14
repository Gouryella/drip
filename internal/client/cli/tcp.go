package cli

import (
	"fmt"
	"strconv"

	"drip/internal/client/tcp"
	"drip/internal/shared/protocol"

	"github.com/spf13/cobra"
)

var tcpCmd = &cobra.Command{
	Use:   "tcp <port>",
	Short: "Start TCP tunnel",
	Long: `Start a TCP tunnel to expose any TCP service.

Example:
  drip tcp 5432                     Tunnel PostgreSQL
  drip tcp 3306                     Tunnel MySQL
  drip tcp 22                       Tunnel SSH
  drip tcp 6379 --subdomain myredis Tunnel Redis with custom subdomain
  drip tcp 5432 --allow-ip 192.168.0.0/16  Only allow IPs from 192.168.x.x
  drip tcp 22 --allow-ip 10.0.0.1          Allow single IP
  drip tcp 22 --deny-ip 1.2.3.4            Block specific IP
  drip tcp 22 --transport wss              Use WebSocket over TLS (CDN-friendly)
  drip tcp 22 --bandwidth 1M              Limit bandwidth to 1 MB/s

Supported Services:
  - Databases: PostgreSQL (5432), MySQL (3306), Redis (6379), MongoDB (27017)
  - SSH: Port 22
  - Any TCP service

Configuration:
  First time: Run 'drip config init' to save server and token
  Subsequent: Just run 'drip tcp <port>'

Transport options:
  auto  - Automatically select based on server address (default)
  tcp   - Direct TLS 1.3 connection
  wss   - WebSocket over TLS (works through CDN like Cloudflare)

Note: TCP tunnels require dynamic port allocation on the server.
      When using CDN (--transport wss), the server must still expose the allocated port directly.`,
	Args:          cobra.ExactArgs(1),
	RunE:          runTCP,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	tcpCmd.Flags().StringVarP(&subdomain, "subdomain", "n", "", "Custom subdomain (optional)")
	tcpCmd.Flags().BoolVarP(&daemonMode, "daemon", "d", false, "Run in background (daemon mode)")
	tcpCmd.Flags().StringVarP(&localAddress, "address", "a", "127.0.0.1", "Local address to forward to (default: 127.0.0.1)")
	tcpCmd.Flags().StringSliceVar(&allowIPs, "allow-ip", nil, "Allow only these IPs or CIDR ranges (e.g., 192.168.1.1,10.0.0.0/8)")
	tcpCmd.Flags().StringSliceVar(&denyIPs, "deny-ip", nil, "Deny these IPs or CIDR ranges (e.g., 1.2.3.4,192.168.1.0/24)")
	tcpCmd.Flags().StringVar(&transport, "transport", "auto", "Transport protocol: auto, tcp, wss (WebSocket over TLS)")
	tcpCmd.Flags().StringVar(&bandwidth, "bandwidth", "", "Bandwidth limit (e.g., 1M, 500K, 1G)")
	tcpCmd.Flags().BoolVar(&skipLocalTLSVerify, "skip-local-tls-verify", false, "Ignored for TCP tunnels; kept for daemon argument compatibility")
	tcpCmd.Flags().BoolVar(&daemonMarker, "daemon-child", false, "Internal flag for daemon child process")
	_ = tcpCmd.Flags().MarkHidden("daemon-child")
	tcpCmd.Flags().StringVar(&daemonSecretFile, daemonSecretFileFlagName, "", "Internal daemon secret file")
	_ = tcpCmd.Flags().MarkHidden(daemonSecretFileFlagName)
	rootCmd.AddCommand(tcpCmd)
}

func runTCP(_ *cobra.Command, args []string) error {
	port, err := strconv.Atoi(args[0])
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port number: %s", args[0])
	}

	if err := validateIPAccessFlags(); err != nil {
		return err
	}

	if daemonMode && !daemonMarker {
		daemonArgs, err := buildDaemonArgs("tcp", args, subdomain, localAddress)
		if err != nil {
			return err
		}
		return StartDaemon("tcp", port, daemonArgs)
	}

	if err := applyDaemonSecretFile(); err != nil {
		return err
	}

	serverAddr, token, err := resolveServerAddrAndToken("tcp", port)
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
		TunnelType:         protocol.TunnelTypeTCP,
		LocalHost:          localAddress,
		LocalPort:          port,
		Subdomain:          subdomain,
		Insecure:           insecure,
		AllowIPs:           allowIPs,
		DenyIPs:            denyIPs,
		Transport:          parseTransport(transport),
		Bandwidth:          bw,
		SkipLocalTLSVerify: skipLocalTLSVerify,
	}

	var daemon *DaemonInfo
	if daemonMarker {
		daemon = newDaemonInfo("tcp", port, subdomain, serverAddr)
	}

	return runTunnelWithUI(connConfig, daemon)
}
