//go:build unix

package modelchange

import (
	"fmt"
	"os"
	"syscall"
)

// lockStateFile serializes model-state/config transitions across the daemon,
// detached restart helper, and local clients. The process-local mutex on
// Service is intentionally not treated as cross-process authority.
func lockStateFile(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open model state lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock model state: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
