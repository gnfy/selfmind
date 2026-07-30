package tools

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

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
	enabled, required, _ := execSandboxPolicy()
	return effectiveSandboxModeForPolicy(requested, enabled, required)
}

func effectiveSandboxModeForPolicy(requested SandboxMode, enabled, required bool) SandboxMode {
	if requested == SandboxHost {
		return SandboxHost
	}
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
	enabled, required, _ := execSandboxPolicyForArgs(args)
	args["_effective_sandbox_mode"] = string(effectiveSandboxModeForPolicy(requested, enabled, required))
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
	_, _, network := execSandboxPolicyForArgs(args)
	return network
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
	return sandboxedCommandWithMaterial(ctx, inner, execMaterial{
		WritableRoots: []string{writableRoot},
		Env:           currentToolProcessEnv(),
	}, requested, goos, sandboxAvailable, networkOverride...)
}

// execMaterial is everything the sandbox needs that is derived from the request
// rather than from process-wide policy: the writable view, the run's scratch
// space, and the child environment. Keeping it in one value is what lets every
// exec path (terminal, verify, execute_code, durable watch) share one
// construction site.
type execMaterial struct {
	WritableRoots []string
	// ReadOnlyPaths are approved host paths a profile mapped read-only (a
	// kubeconfig, an aws credentials file). They are already readable under the
	// read-only host root today; keeping them explicit is what lets a future
	// restricted-read backend narrow the view without changing callers.
	ReadOnlyPaths []string
	// OverlayMounts bind a writable state directory over a host path whose
	// location a tool does not let us change (the AWS SSO token cache).
	OverlayMounts []SandboxOverlayMount
	// SynthesizedDirs replace a tool's state root with a writable shell so the
	// overlay targets above are mountable even when the host lacks them.
	SynthesizedDirs []SandboxSynthesizedDir
	ScratchTmp      string
	Env             []string
	// Profiles, ProfileNotes and the copy counters are reported as execution
	// evidence so a withheld credential or a bounded state copy is visible
	// instead of surfacing later as a mysterious "not logged in".
	Profiles []string
	// ProfilesFromInventory are the profiles prepared because the HOST has their
	// state, not because the command named their tool. Reported as evidence: a
	// command that received credential state without naming the tool must be
	// explainable from the event stream alone.
	ProfilesFromInventory []string
	ProfileNotes          []string
	CopiedStateFiles      int
	CopiedStateBytes      int64
	// ScratchBytes is the run's accumulated scratch size, reported as evidence
	// so an unbounded run is visible before it fills the disk.
	ScratchBytes int64
	// Identity is the environment binding this material was built from. It is
	// what makes an emitted SandboxPlan self-describing: a plan with empty
	// snapshot/generation/scratch fields cannot be audited or replayed, and it
	// hides exactly the binding a remote execution node would need to verify.
	SnapshotID    string
	Generation    int64
	ScratchHandle string
	// ProfileError records a failure while preparing state. The command must not
	// run: a half-prepared credential overlay fails later with a misleading
	// error.
	ProfileError error
}

func sandboxedCommandWithMaterial(
	ctx context.Context,
	inner []string,
	material execMaterial,
	requested SandboxMode,
	goos string,
	sandboxAvailable bool,
	networkOverride ...bool,
) (*exec.Cmd, SandboxDecision, error) {
	if len(inner) == 0 {
		return nil, SandboxDecision{}, fmt.Errorf("sandbox command is empty")
	}
	env := material.Env
	if len(env) == 0 {
		env = currentToolProcessEnv()
	}
	plain := func(decision SandboxDecision) (*exec.Cmd, SandboxDecision, error) {
		plan := planFromMaterial(material, decision)
		processMaterial := ProcessMaterial{env: env, scratchTmp: strings.TrimSpace(material.ScratchTmp)}
		cmd, err := SandboxBackendForMode(SandboxHost).Command(ctx, inner, plan, processMaterial)
		if err != nil {
			return nil, SandboxDecision{}, err
		}
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

	decision := SandboxDecision{Mode: SandboxIsolated, NetworkShared: network}
	plan := planFromMaterial(material, decision)
	processMaterial := ProcessMaterial{env: env, scratchTmp: strings.TrimSpace(material.ScratchTmp)}
	cmd, err := SandboxBackendForMode(SandboxIsolated).Command(ctx, inner, plan, processMaterial)
	if err != nil {
		if requested == SandboxIsolated || required {
			return nil, SandboxDecision{}, fmt.Errorf("isolated execution is unavailable (install bubblewrap and enable unprivileged user namespaces)")
		}
		return plain(SandboxDecision{Mode: SandboxHost, Reason: "bubblewrap or unprivileged user namespaces unavailable", NetworkShared: true})
	}
	return cmd, decision, nil
}

func sandboxedShellCommand(ctx context.Context, command, writableRoot string, requested SandboxMode, networkOverride ...bool) (*exec.Cmd, SandboxDecision, error) {
	return sandboxedCommand(ctx, shellArgv(command), writableRoot, requested, networkOverride...)
}

// DurableExecutionScope describes a daemon-owned execution that has no live
// agent run — an external watch, a scheduled check — but still needs the same
// environment preparation as a foreground command.
//
// This path has NO host escape hatch by design, which is exactly why it must
// get state overlays: a durable gcloud check failed on every attempt with
// CHECK_ERROR because it could not write its own credential store, and no
// amount of model reasoning could recover it. ScratchKey is stable per watch, so
// the overlay is materialized once and reused by every poll.
type DurableExecutionScope struct {
	ScratchKey   string
	TenantID     string
	PersonID     string
	WorkspaceID  string
	TrustLevel   string
	Capabilities []string
}

// RunDurableCheck runs a daemon-owned check with the same execution material a
// foreground command receives: request-derived writable roots, run scratch, and
// the tool environment profiles the command's programs need.
//
// It returns the FULL typed result on purpose. The earlier signature returned
// only (output, error), so the caller had to re-derive a diagnosis from text —
// and a watch whose sandbox could not even be constructed
// (FailureClass=credential_state_readonly) was retried every 30 seconds for two
// hours, because the string it inspected did not happen to contain a marker from
// its own private list. The typed class already exists at this boundary; the one
// thing a durable caller must not do is throw it away.
func RunDurableCheck(ctx context.Context, command, cwd string, networkShared bool, durable DurableExecutionScope) (ExecutionResult, error) {
	// The durable path uses the SAME engine entry as a foreground command. It
	// previously had its own construction, which is how it ended up with neither
	// state overlays nor an environment binding — and it is the one path with no
	// host escape hatch, so an unprepared environment there is unrecoverable.
	return Execute(ctx, ExecutionRequest{
		ToolName:       "watch_external",
		Payload:        command,
		Shell:          true,
		CWD:            cwd,
		WorkspaceRoots: []string{cwd},
		Sandbox:        SandboxAuto,
		NetworkShared:  networkShared,
		Durable:        &durable,
		ToolProfile:    ToolProfile{Class: ToolExecutionStandard, HeartbeatInterval: time.Second},
	}, nil)
}

// ExecSandboxPolicy is the execution policy for one request.
type ExecSandboxPolicy struct {
	Enabled      bool
	Required     bool
	AllowNetwork bool
}

// CurrentExecSandboxPolicy snapshots the operator policy so a request can carry
// it explicitly instead of reading a process global at execution time.
func CurrentExecSandboxPolicy() *ExecSandboxPolicy {
	enabled, required, network := execSandboxPolicy()
	return &ExecSandboxPolicy{Enabled: enabled, Required: required, AllowNetwork: network}
}

// execSandboxPolicyForArgs resolves the policy for a call: the request's own
// policy when the scope carries one, else the process default. Threading policy
// through the request is what lets one process serve executions that must not
// share a single global setting.
func execSandboxPolicyForArgs(args map[string]interface{}) (enabled, required, network bool) {
	if scope, ok := currentExecutionScopeAny(args); ok && scope.SandboxPolicy != nil {
		return scope.SandboxPolicy.Enabled, scope.SandboxPolicy.Required, scope.SandboxPolicy.AllowNetwork
	}
	return execSandboxPolicy()
}
