//go:build unix

package peercred

import (
	"errors"
	"syscall"
)

// groupAlive reports whether any process is still in the group. EPERM means a
// member exists under another user, which still keeps the id in use.
func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
