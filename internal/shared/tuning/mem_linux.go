//go:build linux

package tuning

import "syscall"

func getSystemTotalMemory() uint64 {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err == nil {
		return constrainMemory(uint64(info.Totalram)*uint64(info.Unit),
			"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes")
	}
	return 1024 * 1024 * 1024
}
