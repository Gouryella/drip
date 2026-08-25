//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"drip/internal/shared/ui"
	"drip/pkg/config"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	// serviceControlTimeout bounds how long a start or stop is waited on.
	serviceControlTimeout = 30 * time.Second
	// serviceFailureResetPeriod is how long (in seconds) the service must stay up
	// before its failure count resets.
	serviceFailureResetPeriod = 24 * 60 * 60
)

func installService(opts serviceOptions) error {
	if err := ensureElevated(); err != nil {
		return err
	}

	exePath, err := serviceExecutablePath()
	if err != nil {
		return err
	}

	configPath, seededFrom, err := prepareServiceConfig(opts.configPath)
	if err != nil {
		return err
	}
	opts.configPath = configPath

	// Resolve the tunnels now: an install whose config the service cannot use
	// would otherwise fail at boot, unattended, with nobody reading the log.
	cfg, err := config.LoadClientConfig(configPath)
	if err != nil {
		return err
	}
	if _, err := selectTunnels(cfg, opts.all, opts.tunnels); err != nil {
		return err
	}

	if opts.logPath == "" {
		opts.logPath = defaultServiceLogPath()
	}
	if err := os.MkdirAll(filepath.Dir(opts.logPath), 0o750); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	// The config holds the account token and %ProgramData% grants Users read
	// access by default, so the machine-wide copy is locked down before the
	// service exists. A config the administrator pointed somewhere else is left
	// alone: rewriting the DACL of a file inside a user profile would lock that
	// user out of their own configuration.
	if isUnderDir(configPath, serviceDataDir()) {
		if err := restrictServiceFile(configPath, opts.username); err != nil {
			return err
		}
	} else {
		fmt.Println(ui.Warning(fmt.Sprintf(
			"%s is outside %s, so its permissions were left untouched; make sure it is readable by %s and not by every user",
			configPath, serviceDataDir(), serviceAccountLabel(opts.username))))
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to the service manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()

	if existing, err := m.OpenService(opts.name); err == nil {
		_ = existing.Close()
		return fmt.Errorf("service %q is already installed; run 'drip service uninstall --name %s' first", opts.name, opts.name)
	}

	service, err := m.CreateService(opts.name, exePath, mgr.Config{
		DisplayName:      opts.displayName,
		Description:      opts.description,
		StartType:        serviceStartTypeValue(opts.startType),
		DelayedAutoStart: opts.startType == "delayed",
		ErrorControl:     mgr.ErrorNormal,
		ServiceStartName: opts.username,
		Password:         opts.password,
	}, buildServiceArgs(opts)...)
	if err != nil {
		return fmt.Errorf("failed to create service: %w", err)
	}
	defer func() { _ = service.Close() }()

	if err := configureServiceRecovery(service); err != nil {
		_ = service.Delete()
		return err
	}

	if err := eventlog.InstallAsEventCreate(opts.name, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		fmt.Println(ui.Warning(fmt.Sprintf("Service installed, but registering the event log source failed: %v", err)))
	}

	lines := []string{
		ui.KeyValue("Service", opts.name),
		ui.KeyValue("Executable", exePath),
		ui.KeyValue("Config", configPath),
		ui.KeyValue("Log", opts.logPath),
		ui.KeyValue("Start type", opts.startType),
		ui.KeyValue("Account", serviceAccountLabel(opts.username)),
	}
	if seededFrom != "" {
		lines = append(lines, ui.KeyValue("Copied from", seededFrom))
	}
	lines = append(lines, "", "Next: drip service start --name "+opts.name)

	fmt.Println(ui.SuccessBox("Service installed", lines...))

	return nil
}

func uninstallService(name string) error {
	if err := validateServiceName(name); err != nil {
		return err
	}
	if err := ensureElevated(); err != nil {
		return err
	}

	m, service, err := openService(name, windows.SC_MANAGER_CONNECT, windows.SERVICE_ALL_ACCESS)
	if err != nil {
		return err
	}
	defer closeService(m, service)

	status, err := service.Query()
	if err != nil {
		return fmt.Errorf("failed to query service: %w", err)
	}

	if status.State != svc.Stopped {
		if _, err := service.Control(svc.Stop); err != nil {
			return fmt.Errorf("failed to stop service: %w", err)
		}
		if err := waitForServiceState(service, svc.Stopped); err != nil {
			return err
		}
	}

	if err := service.Delete(); err != nil {
		return fmt.Errorf("failed to delete service: %w", err)
	}

	if err := eventlog.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Println(ui.Warning(fmt.Sprintf("Service removed, but unregistering the event log source failed: %v", err)))
	}

	// The config and logs are left in place on purpose: an uninstall is often a
	// reinstall, and the config holds credentials the user would have to re-enter.
	fmt.Println(ui.SuccessBox("Service removed",
		ui.KeyValue("Service", name),
		"",
		"Configuration and logs under "+serviceDataDir()+" were kept."))

	return nil
}

func startService(name string) error {
	if err := validateServiceName(name); err != nil {
		return err
	}

	m, service, err := openService(name, windows.SC_MANAGER_CONNECT, windows.SERVICE_START|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer closeService(m, service)

	status, err := service.Query()
	if err != nil {
		return fmt.Errorf("failed to query service: %w", err)
	}
	if status.State == svc.Running {
		fmt.Println(ui.Success(fmt.Sprintf("Service %q is already running (PID %d)", name, status.ProcessId)))
		return nil
	}

	if err := service.Start(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}
	if err := waitForServiceState(service, svc.Running); err != nil {
		return err
	}

	fmt.Println(ui.Success(fmt.Sprintf("Service %q started", name)))

	return nil
}

func stopService(name string) error {
	if err := validateServiceName(name); err != nil {
		return err
	}

	m, service, err := openService(name, windows.SC_MANAGER_CONNECT, windows.SERVICE_STOP|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer closeService(m, service)

	status, err := service.Query()
	if err != nil {
		return fmt.Errorf("failed to query service: %w", err)
	}
	if status.State == svc.Stopped {
		fmt.Println(ui.Success(fmt.Sprintf("Service %q is already stopped", name)))
		return nil
	}

	if _, err := service.Control(svc.Stop); err != nil {
		return fmt.Errorf("failed to stop service: %w", err)
	}
	if err := waitForServiceState(service, svc.Stopped); err != nil {
		return err
	}

	fmt.Println(ui.Success(fmt.Sprintf("Service %q stopped", name)))

	return nil
}

func statusService(name string) error {
	if err := validateServiceName(name); err != nil {
		return err
	}

	m, service, err := openService(name, windows.SC_MANAGER_CONNECT, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return err
	}
	defer closeService(m, service)

	status, err := service.Query()
	if err != nil {
		return fmt.Errorf("failed to query service: %w", err)
	}

	lines := []string{
		ui.KeyValue("Service", name),
		ui.KeyValue("State", serviceStateLabel(status.State)),
	}
	if status.ProcessId != 0 {
		lines = append(lines, ui.KeyValue("PID", fmt.Sprintf("%d", status.ProcessId)))
	}
	if status.State == svc.Stopped && status.Win32ExitCode != 0 {
		lines = append(lines, ui.KeyValue("Last exit code", fmt.Sprintf("%d", status.Win32ExitCode)))
	}

	if cfg, err := service.Config(); err == nil {
		lines = append(lines,
			ui.KeyValue("Display name", cfg.DisplayName),
			ui.KeyValue("Start type", serviceStartTypeLabel(cfg.StartType, cfg.DelayedAutoStart)),
			ui.KeyValue("Account", serviceAccountLabel(cfg.ServiceStartName)),
			ui.KeyValue("Command", cfg.BinaryPathName),
		)
	}

	fmt.Println(ui.Info("Service status", lines...))

	return nil
}

// openService opens the service manager and one service with the narrowest
// access that works, so 'service status' does not demand an elevated prompt.
func openService(name string, managerAccess uint32, serviceAccess uint32) (*mgr.Mgr, *mgr.Service, error) {
	managerHandle, err := windows.OpenSCManager(nil, nil, managerAccess)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to the service manager: %w", err)
	}

	namePointer, err := windows.UTF16PtrFromString(name)
	if err != nil {
		_ = windows.CloseServiceHandle(managerHandle)
		return nil, nil, fmt.Errorf("invalid service name: %w", err)
	}

	serviceHandle, err := windows.OpenService(managerHandle, namePointer, serviceAccess)
	if err != nil {
		_ = windows.CloseServiceHandle(managerHandle)
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil, nil, fmt.Errorf("service %q is not installed; run 'drip service install' first", name)
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, nil, fmt.Errorf("access denied opening service %q: run this command from an elevated prompt", name)
		}
		return nil, nil, fmt.Errorf("failed to open service %q: %w", name, err)
	}

	return &mgr.Mgr{Handle: managerHandle}, &mgr.Service{Name: name, Handle: serviceHandle}, nil
}

func closeService(m *mgr.Mgr, service *mgr.Service) {
	_ = service.Close()
	_ = m.Disconnect()
}

// waitForServiceState polls until the service reaches want or the timeout lapses.
func waitForServiceState(service *mgr.Service, want svc.State) error {
	deadline := time.Now().Add(serviceControlTimeout)

	for time.Now().Before(deadline) {
		status, err := service.Query()
		if err != nil {
			return fmt.Errorf("failed to query service: %w", err)
		}
		if status.State == want {
			return nil
		}
		if want == svc.Running && status.State == svc.Stopped && status.Win32ExitCode != 0 {
			return fmt.Errorf("service exited with code %d; check the service log and the Windows event log", status.Win32ExitCode)
		}
		time.Sleep(300 * time.Millisecond)
	}

	return fmt.Errorf("timed out after %s waiting for the service to reach %s", serviceControlTimeout, serviceStateLabel(want))
}

// configureServiceRecovery makes Windows restart the service after a failure.
func configureServiceRecovery(service *mgr.Service) error {
	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}

	if err := service.SetRecoveryActions(actions, serviceFailureResetPeriod); err != nil {
		return fmt.Errorf("failed to set recovery actions: %w", err)
	}

	// Without this, recovery only covers crashes; a supervisor that gives up and
	// exits with an error code would stay down until somebody noticed.
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("failed to enable recovery on non-crash failures: %w", err)
	}

	return nil
}

// prepareServiceConfig resolves the config path the service will read, seeding it
// from the user's own config the first time. Returns the resolved path and the
// path it was copied from, if any.
func prepareServiceConfig(requested string) (string, string, error) {
	path := requested
	if path == "" {
		path = defaultServiceConfigPath()
	}

	path, err := filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("invalid config path: %w", err)
	}

	if _, err := os.Stat(path); err == nil {
		return path, "", nil
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("failed to read %s: %w", path, err)
	}

	// The service runs as LocalSystem, whose profile is
	// C:\Windows\system32\config\systemprofile, so it can never read the config in
	// the installing user's home. Copy it into a machine-wide location instead.
	source := config.DefaultClientConfigPath()
	data, err := os.ReadFile(filepath.Clean(source)) // #nosec G304 -- the source is the caller's own config path
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf(
				"no configuration found at %s; run 'drip config init' first, or pass --config with a config file the service should read",
				source)
		}
		return "", "", fmt.Errorf("failed to read %s: %w", source, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", "", fmt.Errorf("failed to create config directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", "", fmt.Errorf("failed to write %s: %w", path, err)
	}

	return path, source, nil
}

// restrictServiceFile replaces the file's inherited permissions with SYSTEM and
// Administrators full control, plus read access for a custom service account.
func restrictServiceFile(path string, username string) error {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("failed to resolve the SYSTEM account: %w", err)
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("failed to resolve the Administrators group: %w", err)
	}

	entries := []windows.EXPLICIT_ACCESS{
		explicitAccess(system, windows.GENERIC_ALL, windows.TRUSTEE_IS_USER),
		explicitAccess(administrators, windows.GENERIC_ALL, windows.TRUSTEE_IS_GROUP),
	}

	if sid, err := serviceAccountSID(username); err != nil {
		return err
	} else if sid != nil {
		entries = append(entries, explicitAccess(sid, windows.GENERIC_READ|windows.GENERIC_EXECUTE, windows.TRUSTEE_IS_USER))
	}

	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("failed to build the access control list: %w", err)
	}

	// PROTECTED_DACL_SECURITY_INFORMATION drops the inherited entries, which is
	// the whole point: %ProgramData% grants Users read access by default and the
	// config file holds the account token.
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	); err != nil {
		return fmt.Errorf("failed to restrict permissions on %s: %w", path, err)
	}

	return nil
}

func explicitAccess(sid *windows.SID, permissions windows.ACCESS_MASK, trusteeType windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: permissions,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

// serviceAccountSID resolves the account the service runs as, or nil when the
// default LocalSystem is used and the SYSTEM entry already covers it.
func serviceAccountSID(username string) (*windows.SID, error) {
	if username == "" {
		return nil, nil
	}

	switch strings.ToLower(username) {
	case "localsystem", `nt authority\system`:
		return nil, nil
	}

	// ".\name" means the local machine, a form LookupSID does not understand.
	account := strings.TrimPrefix(username, `.\`)

	sid, _, _, err := windows.LookupSID("", account)
	if err != nil {
		return nil, fmt.Errorf("failed to look up account %q: %w", username, err)
	}

	return sid, nil
}

// serviceExecutablePath returns the absolute path the service manager should run.
func serviceExecutablePath() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to locate the drip executable: %w", err)
	}

	// A service outlives the shell that installed it, so a symlink or a relative
	// path here becomes an unstartable service later.
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}

	return filepath.Abs(exePath)
}

// serviceDataDir is the machine-wide directory holding the service config and logs.
func serviceDataDir() string {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return filepath.Join(programData, "drip")
}

func defaultServiceConfigPath() string {
	return filepath.Join(serviceDataDir(), "config.yaml")
}

func defaultServiceLogPath() string {
	return filepath.Join(serviceDataDir(), "logs", "service.log")
}

func ensureElevated() error {
	if windows.GetCurrentProcessToken().IsElevated() {
		return nil
	}
	return errors.New("this command changes the service database and must run from an elevated prompt (Run as administrator)")
}

func serviceStartTypeValue(startType string) uint32 {
	if startType == "manual" {
		return mgr.StartManual
	}
	return mgr.StartAutomatic
}

func serviceStartTypeLabel(startType uint32, delayed bool) string {
	switch startType {
	case mgr.StartManual:
		return "manual"
	case mgr.StartDisabled:
		return "disabled"
	case mgr.StartAutomatic:
		if delayed {
			return "delayed"
		}
		return "auto"
	default:
		return fmt.Sprintf("unknown (%d)", startType)
	}
}

func serviceAccountLabel(username string) string {
	if username == "" {
		return "LocalSystem"
	}
	return username
}

func serviceStateLabel(state svc.State) string {
	switch state {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continuing"
	case svc.PausePending:
		return "pausing"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("unknown (%d)", state)
	}
}
