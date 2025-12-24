//go:build linux

package tuning

import "syscall"

func getSystemTotalMemory() uint64 {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err == nil {
		return info.Totalram * uint64(info.Unit)
	}
	return 1024 * 1024 * 1024
}
