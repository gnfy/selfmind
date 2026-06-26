//go:build windows

package tools

import "os/exec"

// applySandboxLimits is a no-op on Windows. Process-group containment and
// resource limits use a different API surface (job objects) and are tracked as
// follow-up work. See the unix implementation for the security caveat.
func applySandboxLimits(cmd *exec.Cmd) {}
