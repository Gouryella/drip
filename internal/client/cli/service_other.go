//go:build !windows

package cli

import "errors"

// errServiceUnsupported is returned by every service command off Windows. Unix
// already has service managers; wrapping them badly would help nobody.
var errServiceUnsupported = errors.New(
	"the 'drip service' commands are only available on Windows; " +
		"use a systemd unit (Linux) or a launchd agent (macOS) running 'drip start --all' instead")

func installService(serviceOptions) error { return errServiceUnsupported }

func uninstallService(string) error { return errServiceUnsupported }

func startService(string) error { return errServiceUnsupported }

func stopService(string) error { return errServiceUnsupported }

func statusService(string) error { return errServiceUnsupported }

func runService(serviceRunOptions) error { return errServiceUnsupported }
