//go:build unix

package cli

import (
	"os"
	"syscall"
)

// lockHistoryFile takes a cross-process advisory lock on a sidecar lockfile so
// concurrent selfmind processes cannot interleave an append with a trim
// rewrite (O_APPEND alone does not serialize against truncation). exclusive
// selects LOCK_EX (writers) vs LOCK_SH (readers). Best-effort like
// modelruntime's auth lock: if the lock can't be taken the caller proceeds.
func lockHistoryFile(path string, exclusive bool) func() {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return func() {}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}
