package cli

import (
	"fmt"
	"strconv"

	"drip/internal/client/tcp"
	"drip/internal/shared/protocol"

	"github.com/spf13/cobra"
)

var (
	subdomain    string
	daemonMode   bool
	daemonMarker bool
	localAddress string
	allowIPs     []string
	denyIPs      []string
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

Configuration:
  First time: Run 'drip config init' to save server and token
  Subsequent: Just run 'drip http <port>'

Note: Uses TCP over TLS 1.3 for secure communication`,
	Args: cobra.ExactArgs(1),
	RunE: runHTTP,
}

func init() {
	httpCmd.Flags().StringVarP(&subdomain, "subdomain", "n", "", "Custom subdomain (optional)")
	httpCmd.Flags().BoolVarP(&daemonMode, "daemon", "d", false, "Run in background (daemon mode)")
	httpCmd.Flags().StringVarP(&localAddress, "address", "a", "127.0.0.1", "Local address to forward to (default: 127.0.0.1)")
	httpCmd.Flags().StringSliceVar(&allowIPs, "allow-ip", nil, "Allow only these IPs or CIDR ranges (e.g., 192.168.1.1,10.0.0.0/8)")
	httpCmd.Flags().StringSliceVar(&denyIPs, "deny-ip", nil, "Deny these IPs or CIDR ranges (e.g., 1.2.3.4,192.168.1.0/24)")
	httpCmd.Flags().BoolVar(&daemonMarker, "daemon-child", false, "Internal flag for daemon child process")
	httpCmd.Flags().MarkHidden("daemon-child")
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

	serverAddr, token, err := resolveServerAddrAndToken("http", port)
	if err != nil {
		return err
	}

	connConfig := &tcp.ConnectorConfig{
		ServerAddr: serverAddr,
		Token:      token,
		TunnelType: protocol.TunnelTypeHTTP,
		LocalHost:  localAddress,
		LocalPort:  port,
		Subdomain:  subdomain,
		Insecure:   insecure,
		AllowIPs:   allowIPs,
		DenyIPs:    denyIPs,
	}

	var daemon *DaemonInfo
	if daemonMarker {
		daemon = newDaemonInfo("http", port, subdomain, serverAddr)
	}

	return runTunnelWithUI(connConfig, daemon)
}
