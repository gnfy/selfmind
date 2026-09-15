//go:build !windows

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

// applySandboxLimits applies best-effort process isolation for sandboxed code
// execution on unix. It places the child in its own process group so that a
// timeout/cancel kills the whole tree (including any subprocesses the script
// spawns), not just the python3 parent.
//
// NOTE: this is process-group containment only. It is NOT a security sandbox.
// True isolation (namespaces, seccomp, cgroup memory/CPU limits, container
// runtime) is tracked as follow-up infra work and must not be assumed here.
func applySandboxLimits(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Cancel the command's own process group, including pipeline children which
// otherwise keep inherited output pipes open after the shell is killed.
func configureCommandCancellation(cmd *exec.Cmd) {
	applySandboxLimits(cmd)
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return os.ErrProcessDone
			}
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if err == syscall.ESRCH {
				return os.ErrProcessDone
			}
			return err
		}
	}
}
