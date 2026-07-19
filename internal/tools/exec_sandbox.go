package tools

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"selfmind/internal/tools/sandbox"
)

// SandboxMode is the per-call execution contract exposed by exec tools.
// Auto prefers isolation and records an explicit fallback when the host cannot
// provide it. Host is an approval-gated escape hatch for cloud CLIs and other
// integrations that intentionally need host credentials or networking.
type SandboxMode string

const (
	SandboxAuto     SandboxMode = "auto"
	SandboxIsolated SandboxMode = "isolated"
	SandboxHost     SandboxMode = "host"
)

// SandboxDecision records how a command will actually run. The requested mode
// is not sufficient for diagnostics because auto may degrade on hosts without
// bubblewrap or unprivileged user namespaces.
type SandboxDecision struct {
	Mode   SandboxMode
	Reason string
}

var (
	execSandboxMu       sync.RWMutex
	execSandboxEnabled  bool
	execSandboxRequired bool
	execSandboxNetwork  bool
)

// SetExecSandbox installs the process-wide operator policy. required disables
// all host fallbacks, including an explicit per-call sandbox=host request.
func SetExecSandbox(enabled, required, allowNetwork bool) {
	execSandboxMu.Lock()
	defer execSandboxMu.Unlock()
	execSandboxEnabled = enabled
	execSandboxRequired = required
	execSandboxNetwork = allowNetwork
}

func execSandboxPolicy() (enabled, required, network bool) {
	execSandboxMu.RLock()
	defer execSandboxMu.RUnlock()
	return execSandboxEnabled, execSandboxRequired, execSandboxNetwork
}

// ExecSandboxStatus exposes the effective host capability to diagnostic
// surfaces without leaking implementation details into the config package.
func ExecSandboxStatus() (enabled, required, available bool) {
	enabled, required, _ = execSandboxPolicy()
	return enabled, required, ExecSandboxAvailable()
}

// ExecSandboxAvailable reports host capability without depending on whether
// the dispatcher has installed its process-wide policy yet. CLI diagnostics
// use this before a gateway is started.
func ExecSandboxAvailable() bool {
	return runtime.GOOS == "linux" && sandbox.Available()
}

func requestedSandboxMode(args map[string]interface{}) (SandboxMode, error) {
	raw := strings.ToLower(strings.TrimSpace(stringArg(args, "sandbox")))
	if raw == "" {
		return SandboxAuto, nil
	}
	switch SandboxMode(raw) {
	case SandboxAuto, SandboxIsolated, SandboxHost:
		return SandboxMode(raw), nil
	default:
		return "", fmt.Errorf("invalid sandbox mode %q (want auto, isolated, or host)", raw)
	}
}

// sandboxedCommand applies the execution policy to an argv without involving
// a shell. Auto is deliberately observable when it must fall back: callers
// emit the returned decision as a tool.sandbox event and include it in errors.
func sandboxedCommand(ctx context.Context, inner []string, writableRoot string, requested SandboxMode) (*exec.Cmd, SandboxDecision, error) {
	if len(inner) == 0 {
		return nil, SandboxDecision{}, fmt.Errorf("sandbox command is empty")
	}
	plain := func(decision SandboxDecision) (*exec.Cmd, SandboxDecision, error) {
		return exec.CommandContext(ctx, inner[0], inner[1:]...), decision, nil
	}
	enabled, required, network := execSandboxPolicy()

	if requested == SandboxHost {
		if required {
			return nil, SandboxDecision{}, fmt.Errorf("host execution is disabled because exec_sandbox.required is true")
		}
		return plain(SandboxDecision{Mode: SandboxHost, Reason: "explicit host execution"})
	}
	if !enabled {
		if requested == SandboxIsolated || required {
			return nil, SandboxDecision{}, fmt.Errorf("isolated execution was requested but exec_sandbox.enabled is false")
		}
		return plain(SandboxDecision{Mode: SandboxHost, Reason: "exec sandbox disabled by configuration"})
	}
	if runtime.GOOS != "linux" {
		return nil, SandboxDecision{}, fmt.Errorf("exec sandbox is enabled but unsupported on %s", runtime.GOOS)
	}

	policy := sandbox.Policy{Network: network}
	if strings.TrimSpace(writableRoot) != "" {
		root, err := filepath.Abs(writableRoot)
		if err != nil {
			return nil, SandboxDecision{}, fmt.Errorf("resolve sandbox writable root: %w", err)
		}
		policy.WritableRoots = []string{filepath.Clean(root)}
	}
	wrapped, ok := sandbox.Wrap(policy, inner)
	if !ok {
		if requested == SandboxIsolated || required {
			return nil, SandboxDecision{}, fmt.Errorf("isolated execution is unavailable (install bubblewrap and enable unprivileged user namespaces)")
		}
		return plain(SandboxDecision{Mode: SandboxHost, Reason: "bubblewrap or unprivileged user namespaces unavailable"})
	}
	return exec.CommandContext(ctx, wrapped[0], wrapped[1:]...), SandboxDecision{Mode: SandboxIsolated}, nil
}

func sandboxedShellCommand(ctx context.Context, command, writableRoot string, requested SandboxMode) (*exec.Cmd, SandboxDecision, error) {
	return sandboxedCommand(ctx, []string{"sh", "-c", command}, writableRoot, requested)
}
