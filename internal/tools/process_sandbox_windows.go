//go:build windows

package tools

import "os/exec"

// applySandboxLimits is a no-op on Windows. Process-group containment and
// resource limits use a different API surface (job objects) and are tracked as
// follow-up work. See the unix implementation for the security caveat.
func applySandboxLimits(cmd *exec.Cmd) {}

func configureCommandCancellation(cmd *exec.Cmd) {}

func leadsOwnGroup(cmd *exec.Cmd) bool { return false }

// stopProcessGroup has nothing to stop here: in-place state writes, the only
// reason it is called, exist only on unix sandbox backends.
func stopProcessGroup(cmd *exec.Cmd) {}
