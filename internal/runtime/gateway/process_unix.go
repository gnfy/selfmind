//go:build !windows

package gateway

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return unix.Kill(pid, 0) == nil
}

func terminateProcess(pid int, force bool) error {
	if pid <= 0 {
		return nil
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return unix.Kill(pid, sig)
}

func configureDetachedCommand(attr **syscall.SysProcAttr) {
	*attr = &syscall.SysProcAttr{Setpgid: true}
}

func currentExecutable() (string, error) {
	return os.Executable()
}
