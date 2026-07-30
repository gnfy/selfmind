package tools

import (
	"runtime"
	"strings"
)

// Sandbox containment as an approval judgement (batch C1).
//
// The old question was "does this command look dangerous?", which asks about the
// COMMAND. The better question — the one codex's safety layer asks — is "can the
// sandbox contain this?", which asks about the BLAST RADIUS. They differ sharply
// for the common case: `python3 build.py` inside an isolated sandbox can only
// touch workspace files and cannot reach the network, so it is no more dangerous
// than the file writes smart mode already performs unprompted, yet the
// command-shaped question asked about it every single time.
//
// Containment is claimed only when ALL of these hold:
//
//   - the call is an exec tool whose EFFECTIVE mode is isolated, explicitly
//     annotated for THIS call (never inferred here — see execSandboxContained);
//   - the sandbox is enabled and actually available on this platform, so the
//     policy is enforced rather than aspirational (Linux + bubblewrap; macOS has
//     no native sandbox path, so it never claims containment);
//   - the policy does not allow network egress, because the sandbox's own
//     guarantee is "workspace-writable, no network" (internal/tools/sandbox:
//     --unshare-net) and egress is what turns a contained write into
//     exfiltration;
//   - the dangerous heuristic did NOT fire, so out-of-workspace targets,
//     destructive programs, and egress commands keep their ask.
//
// It is also strictly narrower than the hard floor: hardlineToolCall runs and
// returns before containment is consulted, so a hardline op can never be
// contained into silence. Containment applies to smart mode only; on-request and
// read-only keep their literal contracts (docs/tool-safety.md).

// execSandboxContained reports whether this exec call will run under an enforced
// isolated sandbox with no network.
//
// It reads the EXPLICIT effective-mode annotation only. Inferring the mode from
// the operator policy would be wrong in the one place it matters most: the /mode
// retro-resolution path re-evaluates a PENDING approval from redacted display
// args, where a host-mode call's marker may be missing entirely — inferring
// "isolated" there would auto-approve a host escape. A missing annotation means
// "not proven contained", which asks.
func execSandboxContained(toolName string, args map[string]interface{}) bool {
	if !isExecTool(toolName) {
		return false
	}
	if strings.ToLower(strings.TrimSpace(stringArg(args, "_effective_sandbox_mode"))) != string(SandboxIsolated) {
		return false
	}
	enabled, _, network := execSandboxPolicyForArgs(args)
	if !enabled || network {
		return false
	}
	// Enforceability, not configuration: a policy that cannot be applied on this
	// host contains nothing.
	return runtime.GOOS == "linux" && ExecSandboxAvailable()
}

// containedExecReason is the diagnostic line recorded when containment replaces
// an ask. It names the guarantee being relied on so a later reader can tell this
// apart from a judge approval or a human decision.
const containedExecReason = "sandboxed execution: workspace-writable, network disabled"
