//go:build !linux && !darwin && !windows

package tuning

func getSystemTotalMemory() uint64 {
	return 1024 * 1024 * 1024 // 1GB fallback
}
