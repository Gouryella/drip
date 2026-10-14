package cli

import (
	"fmt"
	"os"
	"time"

	"drip/internal/shared/netutil"
	"drip/pkg/config"
)

func buildDaemonArgs(tunnelType string, args []string, subdomain string, localAddress string) ([]string, error) {
	daemonArgs := append([]string{tunnelType}, args...)
	daemonArgs = append(daemonArgs, "--daemon-child")

	if subdomain != "" {
		daemonArgs = append(daemonArgs, "--subdomain", subdomain)
	}
	if localAddress != "127.0.0.1" {
		daemonArgs = append(daemonArgs, "--address", localAddress)
	}
	if serverURL != "" {
		daemonArgs = append(daemonArgs, "--server", serverURL)
	}
	if authToken != "" || authPass != "" || authBearer != "" {
		secretPath, err := writeDaemonSecretFile(daemonSecretPayload{
			Token:      authToken,
			AuthPass:   authPass,
			AuthBearer: authBearer,
		})
		if err != nil {
			return nil, err
		}
		daemonArgs = append(daemonArgs, daemonSecretFileFlag, secretPath)
	}
	for _, ip := range allowIPs {
		daemonArgs = append(daemonArgs, "--allow-ip", ip)
	}
	for _, ip := range denyIPs {
		daemonArgs = append(daemonArgs, "--deny-ip", ip)
	}
	if transport != "" && transport != "auto" {
		daemonArgs = append(daemonArgs, "--transport", transport)
	}
	if bandwidth != "" {
		daemonArgs = append(daemonArgs, "--bandwidth", bandwidth)
	}
	if skipLocalTLSVerify {
		daemonArgs = append(daemonArgs, "--skip-local-tls-verify")
	}
	if insecure {
		daemonArgs = append(daemonArgs, "--insecure")
	}
	if verbose {
		daemonArgs = append(daemonArgs, "--verbose")
	}

	return daemonArgs, nil
}

func validateIPAccessFlags() error {
	if err := netutil.ValidateIPAccessRules(allowIPs, denyIPs); err != nil {
		return fmt.Errorf("invalid IP access flags: %w", err)
	}
	return nil
}

func resolveServerAddrAndToken(tunnelType string, port int) (string, string, error) {
	token := authToken
	if token == "" {
		token = os.Getenv("DRIP_TOKEN")
	}

	if serverURL != "" {
		return serverURL, token, nil
	}

	cfg, err := config.LoadClientConfig("")
	if err != nil {
		return "", "", fmt.Errorf(`configuration not found.

Please run 'drip config init' first, or use flags:
  drip %s %d --server SERVER:PORT --token TOKEN`, tunnelType, port)
	}

	if cfg.Server == "" {
		return "", "", fmt.Errorf("server address is required")
	}

	if token == "" {
		token = cfg.Token
	}

	return cfg.Server, token, nil
}

func newDaemonInfo(tunnelType string, port int, subdomain string, serverAddr string) *DaemonInfo {
	return &DaemonInfo{
		PID:        os.Getpid(),
		Type:       tunnelType,
		Port:       port,
		Subdomain:  subdomain,
		Server:     serverAddr,
		StartTime:  time.Now(),
		Executable: os.Args[0],
	}
}
