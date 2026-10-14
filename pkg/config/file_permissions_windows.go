//go:build windows

package config

import "os"

func fileGID(info os.FileInfo) (int, bool) {
	return 0, false
}

func processInGroup(gid int) bool {
	return false
}
