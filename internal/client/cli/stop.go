package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop <type> <port>|all",
	Short: "Stop background tunnels",
	Long: `Stop one or all background tunnels.

Examples:
  drip stop http 3000    Stop HTTP tunnel on port 3000
  drip stop https 8443   Stop HTTPS tunnel on port 8443
  drip stop tcp 5432     Stop TCP tunnel on port 5432
  drip stop all          Stop all running tunnels

Use 'drip list' to see running tunnels.`,
	Aliases: []string{"kill"},
	Args:    cobra.MinimumNArgs(1),
	RunE:    runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)
}

func runStop(cmd *cobra.Command, args []string) error {
	if args[0] == "all" {
		return stopAllDaemons()
	}

	if len(args) < 2 {
		return fmt.Errorf("usage: drip stop <type> <port> or drip stop all")
	}

	tunnelType := args[0]
	if tunnelType != "http" && tunnelType != "https" && tunnelType != "tcp" {
		return fmt.Errorf("invalid tunnel type: %s (must be 'http', 'https', or 'tcp')", tunnelType)
	}

	port, err := strconv.Atoi(args[1])
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port number: %s", args[1])
	}

	return stopDaemon(tunnelType, port)
}

func stopDaemon(tunnelType string, port int) error {
	info, err := LoadDaemonInfo(tunnelType, port)
	if err != nil {
		return fmt.Errorf("failed to load daemon info: %w", err)
	}

	if info == nil {
		return fmt.Errorf("no %s tunnel running on port %d", tunnelType, port)
	}

	if !IsProcessRunning(info.PID) {
		RemoveDaemonInfo(tunnelType, port)
		return fmt.Errorf("tunnel was not running (cleaned up stale entry)")
	}

	if err := KillProcess(info.PID); err != nil {
		return fmt.Errorf("failed to stop tunnel: %w", err)
	}

	RemoveDaemonInfo(tunnelType, port)

	fmt.Printf("\033[32m✓\033[0m Stopped %s tunnel on port %d (PID: %d)\n", tunnelType, port, info.PID)
	return nil
}

func stopAllDaemons() error {
	CleanupStaleDaemons()

	daemons, err := ListAllDaemons()
	if err != nil {
		return fmt.Errorf("failed to list daemons: %w", err)
	}

	if len(daemons) == 0 {
		fmt.Println("\033[90mNo running tunnels to stop.\033[0m")
		return nil
	}

	stopped := 0
	failed := 0

	for _, d := range daemons {
		if !IsProcessRunning(d.PID) {
			RemoveDaemonInfo(d.Type, d.Port)
			continue
		}

		if err := KillProcess(d.PID); err != nil {
			fmt.Printf("\033[31m✗\033[0m Failed to stop %s tunnel on port %d: %v\n", d.Type, d.Port, err)
			failed++
			continue
		}

		RemoveDaemonInfo(d.Type, d.Port)
		fmt.Printf("\033[32m✓\033[0m Stopped %s tunnel on port %d (PID: %d)\n", d.Type, d.Port, d.PID)
		stopped++
	}

	fmt.Println()
	if failed > 0 {
		fmt.Printf("Stopped %d tunnel(s), %d failed\n", stopped, failed)
	} else {
		fmt.Printf("Stopped %d tunnel(s)\n", stopped)
	}

	return nil
}
