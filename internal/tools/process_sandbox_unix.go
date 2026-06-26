//go:build !windows

package tools

import (
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
