//go:build linux || darwin

package promptassets

import (
	"os"
	"syscall"
)

func promptPathOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}
