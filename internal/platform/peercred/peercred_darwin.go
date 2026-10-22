//go:build darwin

package peercred

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// socketPeer reads LOCAL_PEERCRED for the uid and LOCAL_PEERPID for the pid;
// both are captured by the kernel when the peer connected.
func socketPeer(fd int) (Credentials, error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return Credentials{}, fmt.Errorf("LOCAL_PEERCRED: %w", err)
	}
	pid, err := unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	if err != nil {
		return Credentials{}, fmt.Errorf("LOCAL_PEERPID: %w", err)
	}
	return Credentials{UID: int(cred.Uid), PID: pid}, nil
}

// lookupProcess reads the parent and process group from the kernel process
// table. macOS has no child subreaper: a process whose parent exits is
// reparented to launchd, so the walk alone cannot follow a detached
// descendant; the tool process-group record covers the common case.
func lookupProcess(pid int) (process, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return process{}, err
	}
	if int(info.Proc.P_pid) != pid {
		return process{}, fmt.Errorf("process %d not found", pid)
	}
	return process{PPID: int(info.Eproc.Ppid), PGID: int(info.Eproc.Pgid)}, nil
}

// EnableChildSubreaper is unavailable on macOS.
func EnableChildSubreaper() (bool, error) { return false, nil }

// ReapAdoptedOrphans has nothing to reap without a subreaper.
func ReapAdoptedOrphans(context.Context, time.Duration, *GroupTracker) {}
