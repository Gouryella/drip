package cli

import (
	"fmt"
	"os"
	"time"

	"drip/pkg/config"
)

func buildDaemonArgs(tunnelType string, args []string, subdomain string, localAddress string) []string {
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
	if authToken != "" {
		daemonArgs = append(daemonArgs, "--token", authToken)
	}
	if insecure {
		daemonArgs = append(daemonArgs, "--insecure")
	}
	if verbose {
		daemonArgs = append(daemonArgs, "--verbose")
	}
	if bandwidth != "" {
		daemonArgs = append(daemonArgs, "--bandwidth", bandwidth)
	}

	return daemonArgs
}

func resolveServerAddrAndToken(tunnelType string, port int) (string, string, error) {
	if serverURL != "" {
		return serverURL, authToken, nil
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

	return cfg.Server, cfg.Token, nil
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
