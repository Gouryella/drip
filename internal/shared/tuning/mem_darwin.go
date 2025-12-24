//go:build darwin

package tuning

import (
	"syscall"
	"unsafe"
)

func getSystemTotalMemory() uint64 {
	mib := [2]int32{6, 24} // CTL_HW, HW_MEMSIZE
	var value uint64
	size := unsafe.Sizeof(value)

	_, _, errno := syscall.Syscall6(
		syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])),
		2,
		uintptr(unsafe.Pointer(&value)),
		uintptr(unsafe.Pointer(&size)),
		0,
		0,
	)
	if errno != 0 {
		return 1024 * 1024 * 1024
	}
	return value
}
