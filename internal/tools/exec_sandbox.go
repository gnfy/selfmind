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
	Mode          SandboxMode
	Reason        string
	NetworkShared bool
}

type ExecSandboxDiagnostic struct {
	Enabled     bool
	Required    bool
	Available   bool
	Backend     string
	Network     string
	Platform    string
	Environment string
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

func ExecSandboxAllowsNetwork() bool {
	_, _, network := execSandboxPolicy()
	return network
}

// ExecSandboxDiagnostics exposes only policy/capability metadata. It never
// includes environment variable names, values, credential refs, or commands.
func ExecSandboxDiagnostics() ExecSandboxDiagnostic {
	enabled, required, network := execSandboxPolicy()
	available := ExecSandboxAvailable()
	backend := "host"
	switch {
	case runtime.GOOS == "linux" && available && enabled:
		backend = "bubblewrap"
	case runtime.GOOS == "linux":
		backend = "host (bubblewrap unavailable or disabled)"
	case runtime.GOOS == "darwin":
		backend = "approval-controlled host"
	default:
		backend = "unsupported host"
	}
	networkMode := "isolated"
	if network {
		networkMode = "shared"
	}
	return ExecSandboxDiagnostic{
		Enabled:     enabled,
		Required:    required,
		Available:   available,
		Backend:     backend,
		Network:     networkMode,
		Platform:    runtime.GOOS + "/" + runtime.GOARCH,
		Environment: "daemon environment filtered by BuildProcessEnv",
	}
}

// ExecSandboxAvailable reports host capability without depending on whether
// the dispatcher has installed its process-wide policy yet. CLI diagnostics
// use this before a gateway is started.
func ExecSandboxAvailable() bool {
	return runtime.GOOS == "linux" && sandbox.Available()
}

// ExecSandboxPromptNote renders the model-facing one-liner about the effective
// exec sandbox, injected into the system prompt (via TaskStrategy) so the
// model KNOWS the execution environment instead of discovering it through
// opaque failures. codex ships the same awareness as its permissions
// instructions; without it, a network-less sandbox burned whole turns on
// blind retries that surfaced as timeouts (observed live against an
// IP-allowlisted ArgoCD). Empty when commands run directly on the host.
func ExecSandboxPromptNote() string {
	enabled, _, network := execSandboxPolicy()
	if !enabled || !ExecSandboxAvailable() {
		return ""
	}
	if network {
		return "Shell/exec tools run inside an OS sandbox: the filesystem outside the workspace is read-only; network shares the daemon host namespace and inherits the daemon's proxy and DNS settings. A command that must write outside the workspace can request sandbox=host (approval required) once.\n"
	}
	return "Shell/exec tools run inside an OS sandbox: the filesystem outside the workspace is read-only and network is disabled by default. Commands that clearly need egress request the workspace-scoped network:shared capability before execution. A timeout alone is not proof of a network problem, and missing credentials are not a reason to switch to host execution.\n"
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

// effectiveSandboxModeForRequest predicts the execution boundary that the
// dispatcher will actually use. Approval decisions must use this value rather
// than trusting the model-visible request: auto falls back to host when the
// configured OS sandbox is disabled or unavailable.
func effectiveSandboxModeForRequest(requested SandboxMode) SandboxMode {
	if requested == SandboxHost {
		return SandboxHost
	}
	enabled, required, _ := execSandboxPolicy()
	if requested == SandboxIsolated || required {
		return SandboxIsolated
	}
	if enabled && runtime.GOOS == "linux" && ExecSandboxAvailable() {
		return SandboxIsolated
	}
	return SandboxHost
}

func annotateEffectiveSandboxMode(args map[string]interface{}) {
	if args == nil || !isExecTool(stringArg(args, "_tool_name")) {
		return
	}
	if mode := SandboxMode(strings.ToLower(strings.TrimSpace(stringArg(args, "_effective_sandbox_mode")))); mode == SandboxHost || mode == SandboxIsolated {
		return
	}
	requested, err := requestedSandboxMode(args)
	if err != nil {
		return
	}
	args["_effective_sandbox_mode"] = string(effectiveSandboxModeForRequest(requested))
}

func effectiveSandboxModeArg(args map[string]interface{}) SandboxMode {
	if mode := SandboxMode(strings.ToLower(strings.TrimSpace(stringArg(args, "_effective_sandbox_mode")))); mode == SandboxHost || mode == SandboxIsolated {
		return mode
	}
	requested, err := requestedSandboxMode(args)
	if err != nil {
		return SandboxAuto
	}
	return requested
}

func networkSharedArg(args map[string]interface{}) bool {
	if value, ok := args["_network_shared"].(bool); ok {
		return value
	}
	return ExecSandboxAllowsNetwork()
}

// sandboxedCommand applies the execution policy to an argv without involving
// a shell. Auto is deliberately observable when it must fall back: callers
// emit the returned decision as a tool.sandbox event and include it in errors.
func sandboxedCommand(ctx context.Context, inner []string, writableRoot string, requested SandboxMode, networkOverride ...bool) (*exec.Cmd, SandboxDecision, error) {
	return sandboxedCommandForPlatform(ctx, inner, writableRoot, requested, runtime.GOOS, ExecSandboxAvailable(), networkOverride...)
}

func sandboxedCommandForPlatform(
	ctx context.Context,
	inner []string,
	writableRoot string,
	requested SandboxMode,
	goos string,
	sandboxAvailable bool,
	networkOverride ...bool,
) (*exec.Cmd, SandboxDecision, error) {
	if len(inner) == 0 {
		return nil, SandboxDecision{}, fmt.Errorf("sandbox command is empty")
	}
	plain := func(decision SandboxDecision) (*exec.Cmd, SandboxDecision, error) {
		cmd := exec.CommandContext(ctx, inner[0], inner[1:]...)
		cmd.Env = currentToolProcessEnv()
		return cmd, decision, nil
	}
	enabled, required, network := execSandboxPolicy()
	if len(networkOverride) > 0 {
		network = networkOverride[0]
	}

	if requested == SandboxHost {
		if required {
			return nil, SandboxDecision{}, fmt.Errorf("host execution is disabled because exec_sandbox.required is true")
		}
		return plain(SandboxDecision{Mode: SandboxHost, Reason: "explicit host execution", NetworkShared: true})
	}
	if !enabled {
		if requested == SandboxIsolated || required {
			return nil, SandboxDecision{}, fmt.Errorf("isolated execution was requested but exec_sandbox.enabled is false")
		}
		return plain(SandboxDecision{Mode: SandboxHost, Reason: "exec sandbox disabled by configuration", NetworkShared: true})
	}
	if goos != "linux" {
		if requested == SandboxIsolated || required {
			return nil, SandboxDecision{}, fmt.Errorf("isolated execution is unavailable on %s", goos)
		}
		return plain(SandboxDecision{
			Mode:          SandboxHost,
			Reason:        fmt.Sprintf("isolated sandbox unsupported on %s; using approval-controlled host execution", goos),
			NetworkShared: true,
		})
	}
	if !sandboxAvailable {
		if requested == SandboxIsolated || required {
			return nil, SandboxDecision{}, fmt.Errorf("isolated execution is unavailable (install bubblewrap and enable unprivileged user namespaces)")
		}
		return plain(SandboxDecision{Mode: SandboxHost, Reason: "bubblewrap or unprivileged user namespaces unavailable", NetworkShared: true})
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
		return plain(SandboxDecision{Mode: SandboxHost, Reason: "bubblewrap or unprivileged user namespaces unavailable", NetworkShared: true})
	}
	cmd := exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
	// Secrets are passed only through the child environment. Never encode them
	// into bwrap argv with --setenv: argv is visible through process listings.
	cmd.Env = currentToolProcessEnv()
	return cmd, SandboxDecision{Mode: SandboxIsolated, NetworkShared: network}, nil
}

func sandboxedShellCommand(ctx context.Context, command, writableRoot string, requested SandboxMode, networkOverride ...bool) (*exec.Cmd, SandboxDecision, error) {
	return sandboxedCommand(ctx, shellArgv(command), writableRoot, requested, networkOverride...)
}

// RunSandboxedShell is the daemon-owned execution path for durable background
// checks such as external watchers. It applies the same filtered child
// environment and sandbox policy as foreground exec tools, rather than
// inheriting gateway control-plane credentials through os/exec.
func RunSandboxedShell(ctx context.Context, command, cwd string, networkShared bool) (string, SandboxDecision, error) {
	cmd, decision, err := sandboxedShellCommand(ctx, command, cwd, SandboxAuto, networkShared)
	if err != nil {
		return "", decision, err
	}
	cmd.Dir = cwd
	output, runErr := cmd.CombinedOutput()
	return string(output), decision, runErr
}
