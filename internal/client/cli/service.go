package cli

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"drip/pkg/config"

	"github.com/spf13/cobra"
)

const (
	defaultServiceName        = "drip"
	defaultServiceDisplayName = "Drip Tunnel Client"
	defaultServiceDescription = "Keeps Drip tunnels connected in the background."
)

// serviceOptions describes an installation request.
type serviceOptions struct {
	name        string
	displayName string
	description string
	configPath  string
	logPath     string
	tunnels     []string
	all         bool
	verbose     bool
	startType   string
	username    string
	password    string
}

// serviceRunOptions describes a service process launched by the service manager.
type serviceRunOptions struct {
	name       string
	configPath string
	logPath    string
	tunnels    []string
	all        bool
	verbose    bool
}

var (
	serviceName        string
	serviceDisplayName string
	serviceDescription string
	serviceConfigPath  string
	serviceLogPath     string
	serviceTunnels     []string
	serviceAll         bool
	serviceStartType   string
	serviceUsername    string
	servicePassword    string
)

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage the Drip background service (Windows)",
	Long: `Run Drip as a Windows service, so configured tunnels reconnect on boot
without anybody being logged in.

Examples:
  drip service install --all              Install a service for every configured tunnel
  drip service install --tunnel web       Install a service for the tunnel named "web"
  drip service start                      Start the installed service
  drip service status                     Show state, start type and configuration
  drip service uninstall                  Stop and remove the service

The service runs as LocalSystem by default and therefore cannot read a config
file from a user profile. 'service install' copies your configuration to
%ProgramData%\drip\config.yaml and restricts it to SYSTEM and Administrators.

On Linux and macOS use systemd or launchd to supervise 'drip start' instead.`,
	Hidden:        runtime.GOOS != "windows",
	SilenceUsage:  true,
	SilenceErrors: true,
}

var serviceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install the Drip service",
	RunE: func(_ *cobra.Command, _ []string) error {
		opts := serviceOptions{
			name:        serviceName,
			displayName: serviceDisplayName,
			description: serviceDescription,
			configPath:  serviceConfigPath,
			logPath:     serviceLogPath,
			tunnels:     serviceTunnels,
			all:         serviceAll,
			verbose:     verbose,
			startType:   serviceStartType,
			username:    serviceUsername,
			password:    servicePassword,
		}

		if err := validateServiceOptions(opts); err != nil {
			return err
		}

		return installService(opts)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

var serviceUninstallCmd = &cobra.Command{
	Use:           "uninstall",
	Short:         "Stop and remove the Drip service",
	RunE:          func(_ *cobra.Command, _ []string) error { return uninstallService(serviceName) },
	SilenceUsage:  true,
	SilenceErrors: true,
}

var serviceStartCmd = &cobra.Command{
	Use:           "start",
	Short:         "Start the installed Drip service",
	RunE:          func(_ *cobra.Command, _ []string) error { return startService(serviceName) },
	SilenceUsage:  true,
	SilenceErrors: true,
}

var serviceStopCmd = &cobra.Command{
	Use:           "stop",
	Short:         "Stop the running Drip service",
	RunE:          func(_ *cobra.Command, _ []string) error { return stopService(serviceName) },
	SilenceUsage:  true,
	SilenceErrors: true,
}

var serviceRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the Drip service",
	RunE: func(_ *cobra.Command, _ []string) error {
		if err := stopService(serviceName); err != nil {
			return err
		}
		return startService(serviceName)
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

var serviceStatusCmd = &cobra.Command{
	Use:           "status",
	Short:         "Show the Drip service status",
	RunE:          func(_ *cobra.Command, _ []string) error { return statusService(serviceName) },
	SilenceUsage:  true,
	SilenceErrors: true,
}

// serviceRunCmd is what the service manager launches. Running it from a console
// supervises the same tunnels in the foreground, which is how you debug a service
// that will not stay up.
var serviceRunCmd = &cobra.Command{
	Use:    "run",
	Short:  "Run the tunnel supervisor (invoked by the service manager)",
	Hidden: true,
	RunE: func(_ *cobra.Command, _ []string) error {
		return runService(serviceRunOptions{
			name:       serviceName,
			configPath: serviceConfigPath,
			logPath:    serviceLogPath,
			tunnels:    serviceTunnels,
			all:        serviceAll,
			verbose:    verbose,
		})
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	for _, cmd := range []*cobra.Command{
		serviceInstallCmd, serviceUninstallCmd, serviceStartCmd,
		serviceStopCmd, serviceRestartCmd, serviceStatusCmd, serviceRunCmd,
	} {
		cmd.Flags().StringVar(&serviceName, "name", defaultServiceName, "Windows service name")
	}

	for _, cmd := range []*cobra.Command{serviceInstallCmd, serviceRunCmd} {
		cmd.Flags().StringVar(&serviceConfigPath, "config", "", "Path to the client config file the service reads")
		cmd.Flags().StringVar(&serviceLogPath, "log", "", "Path to the service log file")
		cmd.Flags().StringArrayVar(&serviceTunnels, "tunnel", nil, "Name of a configured tunnel to run (repeatable)")
		cmd.Flags().BoolVar(&serviceAll, "all", false, "Run every tunnel in the config file")
	}

	serviceInstallCmd.Flags().StringVar(&serviceDisplayName, "display-name", defaultServiceDisplayName, "Service display name")
	serviceInstallCmd.Flags().StringVar(&serviceDescription, "description", defaultServiceDescription, "Service description")
	serviceInstallCmd.Flags().StringVar(&serviceStartType, "start-type", "delayed", "Start type: delayed, auto or manual")
	serviceInstallCmd.Flags().StringVar(&serviceUsername, "username", "", `Account to run as (default LocalSystem, e.g. ".\\drip" or "DOMAIN\\user")`)
	serviceInstallCmd.Flags().StringVar(&servicePassword, "password", "", "Password for --username")

	serviceCmd.AddCommand(
		serviceInstallCmd, serviceUninstallCmd, serviceStartCmd,
		serviceStopCmd, serviceRestartCmd, serviceStatusCmd, serviceRunCmd,
	)
	rootCmd.AddCommand(serviceCmd)
}

// validateServiceOptions rejects an installation the service manager would accept
// and then fail to start, so the error arrives while a human is still watching.
func validateServiceOptions(opts serviceOptions) error {
	if err := validateServiceName(opts.name); err != nil {
		return err
	}

	if !opts.all && len(opts.tunnels) == 0 {
		return fmt.Errorf("specify --all or at least one --tunnel")
	}
	if opts.all && len(opts.tunnels) > 0 {
		return fmt.Errorf("--all and --tunnel are mutually exclusive")
	}

	switch opts.startType {
	case "auto", "delayed", "manual":
	default:
		return fmt.Errorf("invalid start type %q: must be delayed, auto or manual", opts.startType)
	}

	if opts.password != "" && opts.username == "" {
		return fmt.Errorf("--password requires --username")
	}

	return nil
}

func validateServiceName(name string) error {
	if name == "" {
		return fmt.Errorf("service name is required")
	}
	// The service control manager treats both slashes as illegal in a service name.
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid service name %q: slashes are not allowed", name)
	}
	if len(name) > 256 {
		return fmt.Errorf("invalid service name: must be at most 256 characters")
	}
	return nil
}

// buildServiceArgs renders the command line the service manager launches. The
// service reads no flags of its own at boot, so everything it needs is baked in
// here — a service started by the SCM has no environment or working directory
// worth relying on.
func buildServiceArgs(opts serviceOptions) []string {
	args := []string{"service", "run", "--name", opts.name, "--config", opts.configPath}

	if opts.logPath != "" {
		args = append(args, "--log", opts.logPath)
	}
	if opts.all {
		args = append(args, "--all")
	}
	for _, tunnel := range opts.tunnels {
		args = append(args, "--tunnel", tunnel)
	}
	if opts.verbose {
		args = append(args, "--verbose")
	}

	return args
}

// isUnderDir reports whether path is dir or lives inside it. Comparison folds
// case and both separators, because the paths it judges are Windows paths and a
// user may well type them with forward slashes.
func isUnderDir(path string, dir string) bool {
	cleanPath := normalizeWindowsPath(path)
	cleanDir := normalizeWindowsPath(dir)

	if cleanPath == cleanDir {
		return true
	}

	return strings.HasPrefix(cleanPath, cleanDir+"/")
}

func normalizeWindowsPath(path string) string {
	normalized := strings.ReplaceAll(filepath.Clean(path), `\`, "/")
	normalized = strings.TrimSuffix(normalized, "/")
	return strings.ToLower(normalized)
}

// selectTunnels resolves --all or a list of tunnel names against the config file.
func selectTunnels(cfg *config.ClientConfig, all bool, names []string) ([]*config.TunnelConfig, error) {
	if len(cfg.Tunnels) == 0 {
		return nil, fmt.Errorf("no tunnels configured in %s", config.DefaultClientConfigPath())
	}

	if all {
		return cfg.Tunnels, nil
	}

	selected := make([]*config.TunnelConfig, 0, len(names))
	for _, name := range names {
		tunnel := cfg.GetTunnel(name)
		if tunnel == nil {
			return nil, fmt.Errorf("tunnel '%s' not found. Available tunnels: %s",
				name, strings.Join(cfg.GetTunnelNames(), ", "))
		}
		selected = append(selected, tunnel)
	}

	if len(selected) == 0 {
		return nil, fmt.Errorf("no tunnels to start")
	}

	return selected, nil
}
