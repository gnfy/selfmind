package tools

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools/envprofiles"
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
	isolation := IsolationBackendForPlatform(runtime.GOOS)
	backend := hostBackendName
	switch {
	case isolation == nil:
		backend = "host (no isolation backend on " + runtime.GOOS + ")"
	case available && enabled:
		backend = isolation.Name()
	default:
		backend = "host (" + isolationUnavailableReason(runtime.GOOS) + " or disabled)"
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
		Environment: "daemon environment filtered by BuildProcessEnv and projected into the execution network view",
	}
}

// ExecSandboxAvailable reports host capability without depending on whether
// the dispatcher has installed its process-wide policy yet. CLI diagnostics
// use this before a gateway is started.
func ExecSandboxAvailable() bool {
	backend := IsolationBackendForPlatform(runtime.GOOS)
	return backend != nil && backend.Available()
}

// isolationUnavailableReason explains, per platform convention, why a host that
// has an isolation backend still cannot use it. It stays a derivation from the
// platform rather than a branch inside the exec path.
func isolationUnavailableReason(goos string) string {
	switch goos {
	case "linux":
		return "bubblewrap or unprivileged user namespaces unavailable"
	case "darwin":
		return "sandbox-exec unavailable"
	}
	return "no isolation backend on " + goos
}

// ExecSandboxPromptNote renders the model-facing one-liner about the effective
// exec sandbox, injected into the system prompt (via TaskStrategy) so the
// model KNOWS the execution environment instead of discovering it through
// opaque failures. codex ships the same awareness as its permissions
// instructions; without it, a network-less sandbox burned whole turns on
// blind retries that surfaced as timeouts (observed live against an
// IP-allowlisted ArgoCD). Host execution explains its managed lane.
func ExecSandboxPromptNoteForContext(ctx context.Context) string {
	if scope, ok := currentExecutionScopeAny(map[string]interface{}{"_context": ctx}); ok {
		policy := scope.SandboxPolicy
		if policy == nil {
			policy = CurrentExecSandboxPolicy()
		}
		if isolatedExecutionView(scope) || policy.Required {
			note := "Shell/exec tools require an enforced OS sandbox for this Run. Host execution is unavailable by policy, regardless of occupied worker slots; ending another Run does not change this policy. Unsupported environment requirements are preparation blocks: no process starts, and retrying unchanged requirements cannot repair them. "
			if policy.AllowNetwork {
				note += "Network uses the configured execution view and proxy route, and only for a command that reaches a remote service (package installs, git remotes, cloud CLIs, curl) or sets network: true; normal network/credential permissions and effect claims still apply.\n"
			} else {
				note += "Network is disabled by default; request the normal network:shared capability when a command reaches a remote service or needs it (network: true).\n"
			}
			return note
		}
		return execSandboxPromptNoteForPolicy(policy)
	}
	return ExecSandboxPromptNote()
}

func ExecSandboxPromptNote() string {
	return execSandboxPromptNoteForPolicy(CurrentExecSandboxPolicy())
}

func execSandboxPromptNoteForPolicy(policy *ExecSandboxPolicy) string {
	if policy.Required {
		return "Shell/exec tools require an enforced OS sandbox. Host execution is unavailable by operator policy; unsupported isolation is a preparation block and no process starts. Normal network and credential permissions still apply.\n"
	}
	if !policy.Enabled || !ExecSandboxAvailable() {
		return "Shell/exec tools run on the host under the current approval mode. Each host command exclusively holds the person's execution lane; other Runs' commands and file edits wait, while model calls continue. Smart reviews every host invocation without a reusable grant; full-auto keeps the safety floor, and on-request asks the person. Explicit host background processes hold the lane only while starting and may affect other Runs' files.\n"
	}
	if policy.AllowNetwork {
		return "Shell/exec tools run inside an OS sandbox: the filesystem outside the workspace is read-only (use $SELFMIND_RUN_TMP for temporary files instead of /tmp); network shares the daemon host namespace only with a command that reaches a remote service (package installs, git remotes, cloud CLIs, curl) or sets network: true, and proxy settings are projected into that network view. Builds, tests and execute_code run offline: set network: true on terminal, verify or execute_code when one needs the network, such as its dependencies downloaded or a remote service called. A command that cannot run inside the sandbox may request sandbox=host; smart reviews every invocation without reuse, full-auto preserves the safety floor, and on-request asks the person. Host execution exclusively holds the person's execution lane. Other Runs' commands and file edits wait, while model calls continue. A host background process holds the lane only while starting and can still affect other Runs' files.\n"
	}
	return "Shell/exec tools run inside an OS sandbox: the filesystem outside the workspace is read-only (use $SELFMIND_RUN_TMP for temporary files instead of /tmp) and network is disabled by default. Commands that clearly reach a remote service (curl, ssh) request the workspace-scoped network:shared capability before execution; set network: true on terminal, verify or execute_code when any other command or script needs the network. A timeout alone is not proof of a network problem, and missing credentials are not a reason to switch to host execution. A command that cannot run inside the sandbox may request sandbox=host under the current approval mode: smart reviews each invocation without reuse, full-auto preserves the safety floor, and on-request asks the person. Host execution exclusively holds the person's execution lane; other Runs' commands and file edits wait, while model calls continue. Explicit host background processes release it after startup and may still affect other Runs' files.\n"
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

// Runtime-owned annotations of the boundary decided before approval.
const (
	// effectiveSandboxReasonArg says why a call that did not ask for the host
	// will run there.
	effectiveSandboxReasonArg = "_effective_sandbox_reason"
	// sandboxPlanRefusalArg says why a call that demands isolation cannot be
	// enforced; such a call is refused before any approval.
	sandboxPlanRefusalArg = "_sandbox_plan_refusal"
)

// annotateEffectiveSandboxMode decides, once and before any capability ask,
// judge or person, the boundary this call will actually run under. Every
// approval decision reads it, and execution is pinned to it.
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
	boundary, refusal := previewSandboxBoundary(stringArg(args, "_tool_name"), args, requested, enabled, required)
	args["_effective_sandbox_mode"] = string(boundary.Mode)
	if boundary.Mode == SandboxHost && requested != SandboxHost && boundary.Reason != "" {
		args[effectiveSandboxReasonArg] = boundary.Reason
	}
	if refusal != "" {
		args[sandboxPlanRefusalArg] = refusal
	}
}

// previewSandboxBoundary resolves the boundary a call would run under on this
// host, including whether the backend can enforce THIS call's plan. Isolation
// the backend cannot enforce is never presented as isolation: optional
// isolation becomes host execution with its reason, which is what the judge
// and the person are then asked about, and demanded isolation is refused with
// its reason so no approval is requested for it at all.
func previewSandboxBoundary(toolName string, args map[string]interface{}, requested SandboxMode, enabled, required bool) (sandboxBoundary, string) {
	if requested == SandboxHost {
		// An explicit host request is asked about as host execution; the
		// required policy still refuses it at dispatch.
		return sandboxBoundary{Mode: SandboxHost, Reason: "explicit host execution"}, ""
	}
	boundary, err := resolveSandboxBoundary(requested, "", "", runtime.GOOS, ExecSandboxAvailable(), enabled, required)
	if err != nil {
		return sandboxBoundary{Mode: SandboxIsolated}, err.Error()
	}
	if boundary.Mode != SandboxIsolated {
		return boundary, ""
	}
	reason := isolatedPlanRefusal(toolName, args)
	if reason == "" {
		return boundary, ""
	}
	if requested == SandboxIsolated || required {
		return boundary, reason
	}
	return sandboxBoundary{
		Mode:   SandboxHost,
		Reason: SandboxBackendName(SandboxIsolated) + " cannot enforce the isolated plan: " + reason,
	}, ""
}

// isolatedPlanRefusal reports, without preparing anything, why this
// platform's isolation backend could not enforce the call's plan. A backend
// that mounts tool state can always realize it; one that writes it in place
// must express every requirement as a declared subpath. Feasibility is
// decided before a credential:read ask, so it assumes that ask could grant
// the credentials: the in-place writes withheld credentials need are a subset.
func isolatedPlanRefusal(toolName string, args map[string]interface{}) string {
	if SandboxBackendForMode(SandboxIsolated).MountsToolState() {
		return ""
	}
	if _, err := previewHostStateWrites(toolName, args, true); err != nil {
		return err.Error()
	}
	return ""
}

// approvedSandboxBoundary is the boundary the approval funnel resolved for
// this call, and why it is the host when the call did not ask for it.
// Execution must use exactly it.
func approvedSandboxBoundary(args map[string]interface{}) (SandboxMode, string) {
	switch mode := SandboxMode(strings.ToLower(strings.TrimSpace(stringArg(args, "_effective_sandbox_mode")))); mode {
	case SandboxHost, SandboxIsolated:
		return mode, strings.TrimSpace(stringArg(args, effectiveSandboxReasonArg))
	}
	return "", ""
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
	cmd, decision, _, err := sandboxedCommandWithMaterial(ctx, inner, execMaterial{
		WritableRoots: []string{writableRoot},
		Env:           currentToolProcessEnv(),
	}, requested, goos, sandboxAvailable, networkOverride...)
	return cmd, decision, err
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
	// HostStateWrites are the same kind of state for a backend that cannot
	// mount: declared host subpaths written in place.
	HostStateWrites []string
	// HostStatePlan is the in-place plan those paths came from; the engine
	// scans it before and after the command (envprofiles.RepairHostStateWrites).
	HostStatePlan envprofiles.HostStatePlan
	// SandboxCanary marks the processes running under this one command's
	// sandbox, so the ones it leaves behind can be found and stopped.
	SandboxCanary string
	ScratchTmp    string
	Env           []string
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
	// ProxyMode records the runtime-owned projection of snapshot proxy values
	// into this invocation's network view. It contains no endpoint or secret.
	ProxyMode        string
	CopiedStateFiles int
	CopiedStateBytes int64
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
) (*exec.Cmd, SandboxDecision, SandboxPlan, error) {
	return sandboxedCommandWithMaterialPolicy(ctx, inner, material, requested, goos, sandboxAvailable, nil, networkOverride...)
}

func sandboxedCommandWithMaterialPolicy(
	ctx context.Context,
	inner []string,
	material execMaterial,
	requested SandboxMode,
	goos string,
	sandboxAvailable bool,
	policy *ExecSandboxPolicy,
	networkOverride ...bool,
) (*exec.Cmd, SandboxDecision, SandboxPlan, error) {
	if len(inner) == 0 {
		return nil, SandboxDecision{}, SandboxPlan{}, fmt.Errorf("sandbox command is empty")
	}
	enabled, required, network := execSandboxPolicy()
	if policy != nil {
		enabled, required, network = policy.Enabled, policy.Required, policy.AllowNetwork
	}
	if len(networkOverride) > 0 {
		network = networkOverride[0]
	}
	boundary, err := resolveSandboxBoundary(requested, "", "", goos, sandboxAvailable, enabled, required)
	if err != nil {
		return nil, SandboxDecision{}, SandboxPlan{}, err
	}
	return commandForBoundary(ctx, inner, material, boundary, network)
}

// sandboxBoundary is the execution boundary of one call. It is resolved before
// the call's material is prepared, because preparation follows the backend
// that will enforce it.
type sandboxBoundary struct {
	Mode SandboxMode
	// Reason says why the boundary is the host when the call did not ask for
	// it.
	Reason string
}

func resolveSandboxBoundaryForPolicy(requested, approved SandboxMode, approvedReason, goos string, available bool, policy *ExecSandboxPolicy) (sandboxBoundary, error) {
	enabled, required, _ := execSandboxPolicy()
	if policy != nil {
		enabled, required = policy.Enabled, policy.Required
	}
	return resolveSandboxBoundary(requested, approved, approvedReason, goos, available, enabled, required)
}

// resolveSandboxBoundary decides a call's boundary from what it requested,
// the boundary its approval already resolved (empty when none did), and the
// policy and host capability.
//
// Isolation that was demanded — by the request, the required policy, or an
// approval that assumed it — is never degraded: a host that cannot provide it
// refuses the call. Only optional isolation the host cannot provide at all
// resolves to the host, with its reason. A backend that cannot enforce one
// particular plan is not a reason to run on the host either; command
// construction refuses that plan.
func resolveSandboxBoundary(requested, approved SandboxMode, approvedReason, goos string, available, enabled, required bool) (sandboxBoundary, error) {
	if requested == SandboxHost || approved == SandboxHost {
		if required {
			return sandboxBoundary{}, isolationRequiredError(fmt.Errorf("host execution is disabled because exec_sandbox.required is true"))
		}
		if requested == SandboxHost {
			return sandboxBoundary{Mode: SandboxHost, Reason: "explicit host execution"}, nil
		}
		reason := strings.TrimSpace(approvedReason)
		if reason == "" {
			reason = "approved host execution"
		}
		return sandboxBoundary{Mode: SandboxHost, Reason: reason}, nil
	}
	unavailable := func(cause error, hostReason string) (sandboxBoundary, error) {
		switch {
		case approved == SandboxIsolated:
			return sandboxBoundary{}, approvedBoundaryUnavailableError(cause)
		case requested == SandboxIsolated || required:
			return sandboxBoundary{}, isolationRequiredError(cause)
		}
		return sandboxBoundary{Mode: SandboxHost, Reason: hostReason}, nil
	}
	if !enabled {
		return unavailable(fmt.Errorf("isolated execution was requested but exec_sandbox.enabled is false"),
			"exec sandbox disabled by configuration")
	}
	if IsolationBackendForPlatform(goos) == nil {
		return unavailable(fmt.Errorf("isolated execution is unavailable on %s", goos),
			fmt.Sprintf("isolated sandbox unsupported on %s; using approval-controlled host execution", goos))
	}
	if !available {
		return unavailable(fmt.Errorf("isolated execution is unavailable (%s)", isolationUnavailableReason(goos)),
			isolationUnavailableReason(goos))
	}
	return sandboxBoundary{Mode: SandboxIsolated}, nil
}

// commandForBoundary builds the process for a resolved boundary and never
// widens it: when the boundary is isolation and the backend cannot enforce
// this plan, the call is refused and nothing starts — on every platform and at
// every capacity. There is no host fallback here.
func commandForBoundary(ctx context.Context, inner []string, material execMaterial, boundary sandboxBoundary, network bool) (*exec.Cmd, SandboxDecision, SandboxPlan, error) {
	if len(inner) == 0 {
		return nil, SandboxDecision{}, SandboxPlan{}, fmt.Errorf("sandbox command is empty")
	}
	if len(material.Env) == 0 {
		material.Env = currentToolProcessEnv()
	}
	decision := SandboxDecision{Mode: SandboxIsolated, NetworkShared: network}
	if boundary.Mode != SandboxIsolated {
		decision = SandboxDecision{Mode: SandboxHost, Reason: boundary.Reason, NetworkShared: true}
	}
	// The proxy projection belongs to the RESOLVED decision, not the request.
	// Host execution carries the daemon's own network, so projecting from an
	// `auto` request stripped a proxy the command could actually reach and
	// produced a self-contradictory record: network_mode "shared" beside
	// proxy_mode "omitted_for_isolated_network".
	projected := adaptExecMaterialForNetwork(material, decision.NetworkShared)
	plan := planFromMaterial(projected, decision)
	processMaterial := ProcessMaterial{env: projected.Env, scratchTmp: strings.TrimSpace(projected.ScratchTmp), canary: projected.SandboxCanary}
	cmd, err := SandboxBackendForMode(decision.Mode).Command(ctx, inner, plan, processMaterial)
	if err != nil {
		if decision.Mode == SandboxIsolated {
			return nil, SandboxDecision{}, SandboxPlan{}, sandboxPreparationError(SandboxBackendName(SandboxIsolated), err)
		}
		return nil, SandboxDecision{}, SandboxPlan{}, err
	}
	return cmd, decision, plan, nil
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
//
// The Binding is the whole authority a poll runs under: its frozen sandbox
// policy (DurableSandboxPolicy) and the in-place credential writes its
// registration's approval covered (HostStateWrites). Nothing a caller passes
// beside it, and no later configuration, widens either.
type DurableExecutionScope struct {
	ScratchKey   string
	TenantID     string
	PersonID     string
	WorkspaceID  string
	TrustLevel   string
	Capabilities []string
	Binding      executionenv.Binding
}

// DurableSandboxPolicy is the policy a durable action runs under: the one its
// registering Run froze, narrowed by the current operator policy and never
// widened by it. A binding recorded before the policy was frozen has none and
// keeps the operator policy, as it always did.
func DurableSandboxPolicy(frozen *executionenv.SandboxPolicy) *ExecSandboxPolicy {
	current := CurrentExecSandboxPolicy()
	if frozen == nil {
		return current
	}
	// Network is not frozen: a poll's network is its binding's network:shared
	// capability, checked against the policy in force before every poll.
	return &ExecSandboxPolicy{
		Enabled:      frozen.Enabled || current.Enabled,
		Required:     frozen.Required || current.Required,
		AllowNetwork: current.AllowNetwork,
	}
}

// frozenSandboxPolicy records the isolation policy a call runs under, for a
// durable action it registers.
func frozenSandboxPolicy(args map[string]interface{}) *executionenv.SandboxPolicy {
	enabled, required, _ := execSandboxPolicyForArgs(args)
	return &executionenv.SandboxPolicy{Enabled: enabled, Required: required}
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
	//
	// The poll has no approval of its own: the boundary its registration was
	// approved for rides on the binding and is pinned like any approved call's.
	// A binding that recorded none (a row from before it existed) resolves
	// under the policy alone; one recording a boundary this binary cannot
	// interpret is refused rather than guessed at.
	boundary := SandboxMode(durable.Binding.ApprovedBoundary)
	if boundary != "" && boundary != SandboxHost && boundary != SandboxIsolated {
		return ExecutionResult{FailureClass: "environment"},
			approvedBoundaryUnavailableError(fmt.Errorf("the registration recorded the unrecognised boundary %q", durable.Binding.ApprovedBoundary))
	}
	return Execute(ctx, ExecutionRequest{
		ToolName:       "watch_external",
		Payload:        command,
		Shell:          true,
		CWD:            cwd,
		WorkspaceRoots: []string{cwd},
		Sandbox:        SandboxAuto,
		Boundary:       boundary,
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
