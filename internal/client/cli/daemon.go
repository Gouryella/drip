package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"drip/internal/shared/ui"
	"drip/pkg/config"
	json "github.com/goccy/go-json"
)

func isSupportedTunnelType(tunnelType string) bool {
	switch tunnelType {
	case "http", "https", "tcp":
		return true
	default:
		return false
	}
}

func validateDaemonTarget(tunnelType string, port int) error {
	if !isSupportedTunnelType(tunnelType) {
		return fmt.Errorf("invalid tunnel type: %s", tunnelType)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid port: %d", port)
	}
	return nil
}

func getDaemonFilePath(tunnelType string, port int) (string, error) {
	if err := validateDaemonTarget(tunnelType, port); err != nil {
		return "", err
	}
	return filepath.Join(getDaemonDir(), fmt.Sprintf("%s_%d.json", tunnelType, port)), nil
}

func getDaemonLogPath(tunnelType string, port int) (string, error) {
	if err := validateDaemonTarget(tunnelType, port); err != nil {
		return "", err
	}
	return filepath.Join(getDaemonDir(), fmt.Sprintf("%s_%d.log", tunnelType, port)), nil
}

func isDaemonFlag(arg string) bool {
	return arg == "-d" || arg == "-D" || arg == "--daemon" ||
		strings.HasPrefix(arg, "--daemon=") || strings.HasPrefix(arg, "-d=")
}

func sanitizeDaemonArgs(args []string) []string {
	cleanArgs := make([]string, 0, len(args))
	for _, arg := range args {
		// Remove daemon flags to avoid recursive respawn, but preserve --daemon-child.
		if isDaemonFlag(arg) {
			continue
		}
		cleanArgs = append(cleanArgs, arg)
	}
	return cleanArgs
}

// DaemonInfo stores information about a running daemon process
type DaemonInfo struct {
	PID        int       `json:"pid"`
	Type       string    `json:"type"`       // "http" or "tcp"
	Port       int       `json:"port"`       // Local port being tunneled
	Subdomain  string    `json:"subdomain"`  // Subdomain if specified
	Server     string    `json:"server"`     // Server address
	URL        string    `json:"url"`        // Tunnel URL
	StartTime  time.Time `json:"start_time"` // When the daemon started
	Executable string    `json:"executable"` // Path to the executable
}

// getDaemonDir returns the directory for storing daemon info
func getDaemonDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".drip"
	}
	return filepath.Join(home, ".drip", "daemons")
}

// SaveDaemonInfo saves daemon information to a file
func SaveDaemonInfo(info *DaemonInfo) error {
	dir := getDaemonDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create daemon directory: %w", err)
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal daemon info: %w", err)
	}

	path, err := getDaemonFilePath(info.Type, info.Port)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write daemon info: %w", err)
	}

	return nil
}

// LoadDaemonInfo loads daemon information from a file
func LoadDaemonInfo(tunnelType string, port int) (*DaemonInfo, error) {
	path, err := getDaemonFilePath(tunnelType, port)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path is generated within controlled directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read daemon info: %w", err)
	}

	var info DaemonInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("failed to parse daemon info: %w", err)
	}

	return &info, nil
}

// RemoveDaemonInfo removes a daemon info file
func RemoveDaemonInfo(tunnelType string, port int) error {
	path, err := getDaemonFilePath(tunnelType, port)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove daemon info: %w", err)
	}
	return nil
}

// ListAllDaemons returns all daemon info files
func ListAllDaemons() ([]*DaemonInfo, error) {
	dir := getDaemonDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read daemon directory: %w", err)
	}

	var daemons []*DaemonInfo
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		// Prevent path traversal: ensure the resolved path is within the daemon directory
		path := filepath.Join(dir, entry.Name())
		cleanPath := filepath.Clean(path)
		cleanDir := filepath.Clean(dir)
		if !strings.HasPrefix(cleanPath, cleanDir+string(filepath.Separator)) && cleanPath != cleanDir {
			continue
		}

		data, err := os.ReadFile(path) // #nosec G304 -- path traversal is checked above
		if err != nil {
			continue
		}

		var info DaemonInfo
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		if err := validateDaemonTarget(info.Type, info.Port); err != nil {
			continue
		}

		daemons = append(daemons, &info)
	}

	return daemons, nil
}

// IsProcessRunning checks if a process with the given PID is running
func IsProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	return isProcessRunningOS(process)
}

// KillProcess kills a process by PID
func KillProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("process not found: %w", err)
	}

	if err := killProcessOS(process); err != nil {
		return fmt.Errorf("failed to kill process: %w", err)
	}

	return nil
}

// StartDaemon starts the current process as a daemon
func StartDaemon(tunnelType string, port int, args []string) error {
	cleanArgs := sanitizeDaemonArgs(args)
	secretPath := parseFlagValue(cleanArgs, daemonSecretFileFlag, "", "")
	secretTransferred := false
	defer func() {
		if secretPath != "" && !secretTransferred {
			_ = os.Remove(secretPath)
		}
	}()

	if err := validateDaemonTarget(tunnelType, port); err != nil {
		return err
	}

	// Get the executable path
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	cmd := exec.Command(executable, cleanArgs...) // #nosec G204 -- exec.Command does not invoke a shell; executable and daemon target are validated separately

	setupDaemonCmd(cmd)

	logDir := getDaemonDir()
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return fmt.Errorf("failed to create daemon directory: %w", err)
	}
	logPath, err := getDaemonLogPath(tunnelType, port)
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600) // #nosec G304 -- tunnelType is validated, path is within controlled directory
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		_ = logFile.Close()
		return fmt.Errorf("failed to open /dev/null: %w", err)
	}
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		_ = devNull.Close()
		return fmt.Errorf("failed to start daemon: %w", err)
	}
	secretTransferred = true

	_ = logFile.Close()
	_ = devNull.Close()

	localHost := parseFlagValue(cleanArgs, "--address", "-a", "127.0.0.1")
	displayHost := localHost
	if displayHost == "127.0.0.1" {
		displayHost = "localhost"
	}
	forwardAddr := fmt.Sprintf("%s:%d", displayHost, port)

	serverAddr := parseFlagValue(cleanArgs, "--server", "-s", "")
	if serverAddr == "" {
		if cfg, err := config.LoadClientConfig(""); err == nil {
			serverAddr = cfg.Server
		}
	}

	var url string

	info, err := waitForDaemonInfo(tunnelType, port, cmd.Process.Pid, 30*time.Second)
	if err == nil && info != nil && info.PID == cmd.Process.Pid && info.URL != "" {
		url = info.URL
		if info.Server != "" {
			serverAddr = info.Server
		}
	}

	fmt.Println(ui.RenderDaemonStarted(tunnelType, port, cmd.Process.Pid, logPath, url, forwardAddr, serverAddr))

	return nil
}

func parseFlagValue(args []string, longName string, shortName string, defaultValue string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == longName || args[i] == shortName {
			if i+1 < len(args) && args[i+1] != "" {
				return args[i+1]
			}
		}
	}
	return defaultValue
}

func waitForDaemonInfo(tunnelType string, port int, pid int, timeout time.Duration) (*DaemonInfo, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !IsProcessRunning(pid) {
			return nil, nil
		}

		info, err := LoadDaemonInfo(tunnelType, port)
		if err == nil && info != nil && info.PID == pid {
			if info.URL != "" {
				return info, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, nil
}

// CleanupStaleDaemons removes daemon info for processes that are no longer running
func CleanupStaleDaemons() error {
	daemons, err := ListAllDaemons()
	if err != nil {
		return err
	}

	for _, info := range daemons {
		if !IsProcessRunning(info.PID) {
			_ = RemoveDaemonInfo(info.Type, info.Port)
		}
	}

	return nil
}

// FormatDuration formats a duration in a human-readable way
func FormatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}

	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd %dh", days, hours)
}
