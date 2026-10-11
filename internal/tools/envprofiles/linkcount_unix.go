//go:build unix

package envprofiles

import (
	"io/fs"
	"syscall"
)

// linkCount is the number of directory entries naming the file: more than one
// means another path, possibly outside the cache, aliases it.
func linkCount(info fs.FileInfo) uint64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Nlink)
	}
	return 1
}
