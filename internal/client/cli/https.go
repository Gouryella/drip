package cli

import (
	"fmt"
	"strconv"

	"drip/internal/client/tcp"
	"drip/internal/shared/protocol"

	"github.com/spf13/cobra"
)

var httpsCmd = &cobra.Command{
	Use:   "https <port>",
	Short: "Start HTTPS tunnel",
	Long: `Start an HTTPS tunnel to expose a local HTTPS server.

Example:
  drip https 443                    Tunnel localhost:443
  drip https 8443 --subdomain myapp Use custom subdomain
  drip https 443 --allow-ip 192.168.0.0/16  Only allow IPs from 192.168.x.x
  drip https 443 --allow-ip 10.0.0.1        Allow single IP
  drip https 443 --deny-ip 1.2.3.4          Block specific IP

Configuration:
  First time: Run 'drip config init' to save server and token
  Subsequent: Just run 'drip https <port>'

Note: Uses TCP over TLS 1.3 for secure communication`,
	Args: cobra.ExactArgs(1),
	RunE: runHTTPS,
}

func init() {
	httpsCmd.Flags().StringVarP(&subdomain, "subdomain", "n", "", "Custom subdomain (optional)")
	httpsCmd.Flags().BoolVarP(&daemonMode, "daemon", "d", false, "Run in background (daemon mode)")
	httpsCmd.Flags().StringVarP(&localAddress, "address", "a", "127.0.0.1", "Local address to forward to (default: 127.0.0.1)")
	httpsCmd.Flags().StringSliceVar(&allowIPs, "allow-ip", nil, "Allow only these IPs or CIDR ranges (e.g., 192.168.1.1,10.0.0.0/8)")
	httpsCmd.Flags().StringSliceVar(&denyIPs, "deny-ip", nil, "Deny these IPs or CIDR ranges (e.g., 1.2.3.4,192.168.1.0/24)")
	httpsCmd.Flags().BoolVar(&daemonMarker, "daemon-child", false, "Internal flag for daemon child process")
	httpsCmd.Flags().MarkHidden("daemon-child")
	rootCmd.AddCommand(httpsCmd)
}

func runHTTPS(_ *cobra.Command, args []string) error {
	port, err := strconv.Atoi(args[0])
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port number: %s", args[0])
	}

	if daemonMode && !daemonMarker {
		return StartDaemon("https", port, buildDaemonArgs("https", args, subdomain, localAddress))
	}

	serverAddr, token, err := resolveServerAddrAndToken("https", port)
	if err != nil {
		return err
	}

	connConfig := &tcp.ConnectorConfig{
		ServerAddr: serverAddr,
		Token:      token,
		TunnelType: protocol.TunnelTypeHTTPS,
		LocalHost:  localAddress,
		LocalPort:  port,
		Subdomain:  subdomain,
		Insecure:   insecure,
		AllowIPs:   allowIPs,
		DenyIPs:    denyIPs,
	}

	var daemon *DaemonInfo
	if daemonMarker {
		daemon = newDaemonInfo("https", port, subdomain, serverAddr)
	}

	return runTunnelWithUI(connConfig, daemon)
}
