//go:build unix

package modelruntime

import (
	"os"
	"syscall"
)

// lockAuthFile takes a cross-process exclusive lock on a sidecar lockfile so two
// SelfMind processes sharing one OAuth auth file (e.g. two CLI sessions using
// ~/.codex/auth.json) cannot refresh — and rotate the refresh token —
// concurrently. The in-process singleflight only coordinates within one process;
// this is the cross-process complement. Best-effort: if the lock can't be taken
// the caller proceeds (correctness degrades to the old behavior, never worse).
func lockAuthFile(path string) func() {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
}
