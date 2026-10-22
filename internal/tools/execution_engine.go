package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
	"selfmind/internal/tools/envprofiles"
)

// The execution engine.
//
// Every command SelfMind runs — foreground shell, verification, code execution,
// durable watch — passes through one construction site here. Before this, each
// tool assembled its own sandbox arguments and read the daemon's environment
// directly, which is how three defects coexisted: the writable view came from
// the command's cwd, the child environment was re-read per command, and the
// durable watch path silently had neither.
//
// The split between the two value types below is the seam that matters for the
// future: SandboxPlan is serializable, versioned, and could be produced by a
// control plane and shipped to a separate execution node, while ProcessMaterial
// holds actual environment values and must never leave the machine that will
// spawn the process.

// SandboxPlanVersion is the wire version of SandboxPlan. Bump it when the
// meaning of a field changes so an older execution node can refuse a plan it
// would misinterpret.
//
// 2 adds SynthesizedDirs: a node that ignores them mounts overlay targets that
// do not exist, which aborts the sandbox instead of degrading.
// 3 adds ProxyMode so a plan records when snapshot proxy values were omitted
// because the execution network could not reach them.
// 4 adds HostStateWrites: on a backend that cannot mount, mount-backed tool
// state is the real host subpath written in place. A node that ignored them
// would deny the tool's own state write instead of granting it.
// 5 adds FlatHostStateWrites: a node that ignored them would let a directory
// appear in a cache whose tool keeps only files there.
const SandboxPlanVersion = 5

// SandboxPlan is the serializable description of an execution boundary.
//
// It deliberately contains NO environment values and NO command text. A plan can
// be logged, stored, or sent over a wire; the values needed to actually spawn
// the process live in ProcessMaterial and are resolved locally.
type SandboxPlan struct {
	Version int `json:"version"`
	// SnapshotID and Generation identify the environment binding this plan was
	// built from, without carrying its contents.
	SnapshotID string `json:"snapshot_id,omitempty"`
	Generation int64  `json:"generation,omitempty"`
	// ScratchHandle is the lease (or durable watch) id whose scratch space this
	// plan uses. It is a HANDLE, not a path: an absolute node-local path in
	// durable state would not survive moving execution to another machine.
	ScratchHandle string `json:"scratch_handle,omitempty"`
	// WritableRoots and ReadOnlyPaths describe the filesystem view.
	WritableRoots []string `json:"writable_roots,omitempty"`
	ReadOnlyPaths []string `json:"read_only_paths,omitempty"`
	// OverlayMounts shadow a host path with a writable state directory, for
	// state whose location the tool does not let us configure.
	OverlayMounts []SandboxOverlayMount `json:"overlay_mounts,omitempty"`
	// SynthesizedDirs replace a state root with a writable shell that keeps only
	// the named children readable. An execution node that ignored this field
	// would mount the overlays above into a path it cannot create, so a plan
	// carrying them is not interpretable by an older node — hence the version.
	SynthesizedDirs []SandboxSynthesizedDir `json:"synthesized_dirs,omitempty"`
	// HostStateWrites are declared tool-state subpaths written in place on the
	// host, by a backend that cannot mount the private overlay above. They are
	// the only part of the host such a plan makes writable beyond its roots.
	HostStateWrites []string `json:"host_state_writes,omitempty"`
	// FlatHostStateWrites are the HostStateWrites whose tool keeps only files
	// directly in them, so no directory may be created or moved in there.
	FlatHostStateWrites []string `json:"flat_host_state_writes,omitempty"`
	// NetworkMode is "isolated" or "shared".
	NetworkMode string `json:"network_mode"`
	// ProxyMode is a non-secret route decision. It never carries an endpoint or
	// environment value.
	ProxyMode string `json:"proxy_mode,omitempty"`
	// Profiles are the tool environment profiles applied.
	Profiles []string `json:"profiles,omitempty"`
	// Backend names the enforcement mechanism ("bubblewrap", "host").
	Backend string `json:"backend"`
	// Mode is the effective sandbox mode after policy resolution.
	Mode SandboxMode `json:"mode"`
	// Notes are non-secret explanations (e.g. credentials withheld).
	Notes []string `json:"notes,omitempty"`
}

// ProcessMaterial carries the values needed to spawn a child process. It has no
// exported fields, no String method and no JSON marshaller, so it cannot be
// logged or serialized by accident — the one thing that must never happen to a
// child environment.
type ProcessMaterial struct {
	env        []string
	scratchTmp string
	canary     string
}

// SandboxCanary is the per-command mark a backend that supports one puts on
// the command's sandbox, or "".
func (m ProcessMaterial) SandboxCanary() string { return m.canary }

// Env returns a copy for the spawn call.
func (m ProcessMaterial) Env() []string {
	out := make([]string, len(m.env))
	copy(out, m.env)
	return out
}

// ScratchTmp is the run's temp directory, needed by the backend to build its
// mount view.
func (m ProcessMaterial) ScratchTmp() string { return m.scratchTmp }

// SandboxBackend enforces a plan. Linux uses bubblewrap and macOS seatbelt,
// from the SAME plan, snapshot, scratch and profiles; host execution is the
// approval-controlled boundary on every platform.
type SandboxBackend interface {
	// Name identifies the backend in plans and diagnostics.
	Name() string
	// Available reports whether this backend can enforce a plan on this host.
	Available() bool
	// MountsToolState reports whether the backend realizes mount-backed tool
	// state (private overlays, synthesized state roots) inside its own view.
	// Preparation asks it: a backend that cannot mount receives that state as
	// in-place writes on the declared host subpaths instead.
	MountsToolState() bool
	// Command builds the process for argv under the plan.
	Command(ctx context.Context, argv []string, plan SandboxPlan, material ProcessMaterial) (*exec.Cmd, error)
}

// ExecutionRequest is one command to run. It is the engine's only input.
type ExecutionRequest struct {
	ToolName   string
	ToolCallID string
	RunID      string
	LeaseID    string
	// Payload is a shell command line when Shell is true; otherwise Command is
	// an argv executed without a shell.
	Payload string
	Command []string
	Shell   bool
	CWD     string
	// WorkspaceRoots comes from the request's ExecutionScope, never from CWD:
	// deriving the writable view from the working directory left a command
	// unable to write to its own workspace root.
	WorkspaceRoots []string
	Sandbox        SandboxMode
	// Boundary is the execution boundary the approval funnel resolved for this
	// call, and BoundaryReason why it is the host when the call did not ask for
	// it. Execution uses exactly that boundary: an approved isolated boundary
	// the backend cannot enforce at dispatch is not dispatched, never run on
	// the host. Empty for a call no approval resolved (a durable poll).
	Boundary       SandboxMode
	BoundaryReason string
	NetworkShared  bool
	// NetworkDecision and NetworkPermission are the runtime's record of why the
	// network is or is not shared with this call (executionenv.NetworkDecision*
	// and NetworkPermission*). They are evidence, never an input to the sandbox:
	// NetworkShared alone decides the plan. Empty for a call that no capability
	// decision preceded.
	NetworkDecision   string
	NetworkPermission string
	Timeout           time.Duration
	// ToolProfile carries the duration class and its clamped timeout, which the
	// streaming heartbeat needs.
	ToolProfile ToolProfile
	// Durable identifies a daemon-owned execution with no live agent run (an
	// external watch). It supplies the identity an ExecutionScope would
	// otherwise provide.
	Durable *DurableExecutionScope
}

// ExecutionResult reports what happened, including the evidence the metrics and
// diagnostics surfaces need.
type ExecutionResult struct {
	Started                bool
	ExitCodeKnown          bool
	ExitCode               int
	Output                 string
	Plan                   SandboxPlan
	ProfilesMatched        []string
	FailureClass           string
	NetworkDecision        string
	NetworkPermission      string
	RecoveryAttempted      bool
	RecoveryOutcome        string
	HostEscapeReason       string
	ScratchBytes           int64
	RequestedProxyRemovals []string
	// HostStateRepairs are the entries the post-command scan moved out of the
	// credential caches this command wrote in place.
	HostStateRepairs []envprofiles.QuarantinedStateEntry
}

// Recovery outcomes.
const (
	RecoveryNone        = "none"
	RecoveryPrepared    = "prepared_and_retried"
	RecoveryNotEligible = "not_eligible"
)

// Host escape reasons, so "avoidable host escapes" can be measured separately
// from the ones that are inherent (an interactive login, a GUI, a deliberate
// write to the host).
const (
	HostEscapeLogin      = "login"
	HostEscapeGUI        = "gui"
	HostEscapeHostWrite  = "host_write"
	HostEscapeSandboxGap = "sandbox_gap"
)

// planFromMaterial renders the serializable plan for a resolved execution.
func planFromMaterial(material execMaterial, decision SandboxDecision) SandboxPlan {
	network := "isolated"
	if decision.NetworkShared {
		network = "shared"
	}
	backend := SandboxBackendName(decision.Mode)
	return SandboxPlan{
		Version:       SandboxPlanVersion,
		SnapshotID:    material.SnapshotID,
		Generation:    material.Generation,
		ScratchHandle: material.ScratchHandle,
		WritableRoots: dedupePaths(material.WritableRoots),
		ReadOnlyPaths: dedupePaths(material.ReadOnlyPaths),
		OverlayMounts: append([]SandboxOverlayMount{}, material.OverlayMounts...),
		SynthesizedDirs: append([]SandboxSynthesizedDir{},
			material.SynthesizedDirs...),
		HostStateWrites:     append([]string{}, material.HostStateWrites...),
		FlatHostStateWrites: flatHostStateWrites(material.HostStatePlan),
		NetworkMode:         network,
		ProxyMode:           material.ProxyMode,
		Profiles:            append([]string{}, material.Profiles...),
		Backend:             backend,
		Mode:                decision.Mode,
		Notes:               append([]string{}, material.ProfileNotes...),
	}
}

// recordedNetworkDecision reconciles the capability decision the request carries
// with the network the built boundary actually gives the process. The built
// boundary wins: host execution carries the daemon's network by construction, so
// it is recorded as shared even when no capability decision preceded it, and a
// decision that disagrees with the plan is not evidence of it. A call that no
// capability decision preceded and that is not shared records nothing: the
// record never claims a reason the runtime did not decide.
func recordedNetworkDecision(req ExecutionRequest, decision SandboxDecision) (string, string) {
	switch {
	case decision.NetworkShared:
		if req.NetworkDecision == executionenv.NetworkDecisionShared {
			return req.NetworkDecision, req.NetworkPermission
		}
		return executionenv.NetworkDecisionShared, ""
	case strings.HasPrefix(req.NetworkDecision, "withheld"):
		return req.NetworkDecision, req.NetworkPermission
	}
	return "", ""
}

// flatHostStateWrites lists the in-place writes whose tool keeps only files.
func flatHostStateWrites(plan envprofiles.HostStatePlan) []string {
	var flat []string
	for _, write := range plan.Writes {
		if write.Flat {
			flat = append(flat, write.Path)
		}
	}
	return flat
}

// SandboxOverlayMount binds Source over Target inside the sandbox.
type SandboxOverlayMount struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// SandboxSynthesizedDir replaces Target with a writable directory that keeps
// only ReadOnlyChildren readable. It is the mount point provider for overlay
// targets the host does not have.
type SandboxSynthesizedDir struct {
	Target           string   `json:"target"`
	ReadOnlyChildren []string `json:"read_only_children,omitempty"`
}

const (
	bubblewrapBackendName = "bubblewrap"
	seatbeltBackendName   = "seatbelt"
	hostBackendName       = "host"
)

// dedupePaths removes repeats while preserving order. The writable set is
// assembled from several contributors (workspace roots, the request, scratch,
// profiles), so the same root legitimately arrives more than once; emitting it
// twice would put duplicate mounts in the plan and read like a defect.
func dedupePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out
}

// classifyHostEscape names WHY a command left the sandbox, so the "avoidable"
// share can be measured. Without it the only available number is the raw host
// share, which mixes a deliberate `gcloud auth login` with a sandbox defect.
func classifyHostEscape(decision SandboxDecision, programs []string, payload string) string {
	if decision.Mode != SandboxHost {
		return ""
	}
	lower := strings.ToLower(payload)
	for _, marker := range []string{" login", " auth login", "device-code", "sso login"} {
		if strings.Contains(lower, marker) {
			return HostEscapeLogin
		}
	}
	for _, program := range programs {
		switch program {
		case "open", "xdg-open", "osascript":
			return HostEscapeGUI
		}
	}
	if strings.Contains(lower, "sudo ") || strings.Contains(lower, "/etc/") || strings.Contains(lower, "systemctl") {
		return HostEscapeHostWrite
	}
	if !ExecSandboxAvailable() {
		// The host cannot isolate at all: not an avoidable escape.
		return HostEscapeHostWrite
	}
	return HostEscapeSandboxGap
}

// ScratchQuotaSoftLimitBytes is when a run's accumulated scratch stops being
// ignorable. It is a SOFT limit by design: the running command is never killed
// (that would abort real work for a disk-hygiene reason), but the next command
// is refused so the run cannot quietly fill the disk. The refusal names the
// directory to clean, because an error a caller cannot act on is not a fix.
const ScratchQuotaSoftLimitBytes int64 = 2 << 30

// Execute runs one ExecutionRequest: it prepares the environment, builds the
// plan, enforces it through a backend, streams the output, classifies a failure,
// and applies at most one bounded recovery.
//
// This is the single production entry point for command execution. args is the
// dispatcher-side envelope (scope lookup, approval decisions written back by the
// middleware, event ids); nothing the sandbox itself needs is read from it, so
// the body below is what a separate execution node would run unchanged.
func Execute(ctx context.Context, req ExecutionRequest, args map[string]interface{}) (ExecutionResult, error) {
	if runtime := executionRuntimeFromArgs(args); runtime != nil {
		ctx = WithExecutionRuntime(ctx, runtime)
	}
	result := ExecutionResult{RecoveryOutcome: RecoveryNone, NetworkDecision: req.NetworkDecision, NetworkPermission: req.NetworkPermission}
	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	argv := req.Command
	if req.Shell {
		result.RequestedProxyRemovals = proxyRemovalVariables(req.Payload)
		argv = shellArgv(req.Payload)
		if req.ToolName == "verify" && runtime.GOOS != "windows" {
			// Verification must preserve a failed check even when the payload
			// pipes its output or prints a footer afterwards. Explicit shell
			// conditionals remain available for intentionally handled failures.
			argv = []string{"/bin/bash", "-e", "-o", "pipefail", "-c", req.Payload}
		}
	}
	if len(argv) == 0 {
		return result, fmt.Errorf("command is required")
	}
	payloadForDiagnostics := req.Payload
	if payloadForDiagnostics == "" {
		payloadForDiagnostics = strings.Join(argv, " ")
	}

	var (
		out      string
		err      error
		decision SandboxDecision
		material execMaterial
	)
	var policy *ExecSandboxPolicy
	if scope, ok := currentExecutionScopeAny(args); ok {
		policy = scope.SandboxPolicy
	}
	if policy == nil && req.Durable != nil {
		// A durable action has no live Run scope; its binding carries the policy
		// its registering Run froze, which the configuration in force now can
		// only narrow. The boundary below, and so whether its state is written
		// in place, is resolved under that policy and never a looser one.
		policy = DurableSandboxPolicy(req.Durable.Binding.SandboxPolicy)
	}
	// The boundary is resolved before anything is prepared. Preparation
	// follows the backend that will enforce it, and an approved boundary that
	// cannot be had starts nothing — it never becomes a host run.
	boundary, boundaryErr := resolveSandboxBoundaryForPolicy(req.Sandbox, req.Boundary, req.BoundaryReason,
		runtime.GOOS, ExecSandboxAvailable(), policy)
	if boundaryErr != nil {
		result.FailureClass = "environment"
		return result, enrichToolFailure(req.ToolName, boundaryErr, "")
	}
	managed, release, laneErr := acquireManagedExecution(runCtx, args, &req, boundary.Mode == SandboxHost)
	if laneErr != nil {
		if classified, ok := laneErr.(interface{ ToolErrorCategory() string }); ok && classified.ToolErrorCategory() == "environment" {
			result.FailureClass = "environment"
		}
		return result, laneErr
	}
	defer release()
	if token := managed.Value(executionLaneContextKey{}); token != nil {
		runCtx = context.WithValue(runCtx, executionLaneContextKey{}, token)
	}
	inPlaceState := boundary.Mode == SandboxIsolated && !SandboxBackendForMode(SandboxIsolated).MountsToolState()
	// At most TWO attempts, and the second only when the engine's own preparation
	// stage is the plausible cause and the command produced no output at all.
	// "Did it produce output" is the decidable proxy for "did it have side
	// effects" — inferring side-effect freedom from command semantics is not
	// something a tool can do correctly, and replaying a command that already ran
	// is worse than reporting it.
	for attempt := 1; attempt <= 2; attempt++ {
		material = execMaterialForRequest(req, args, inPlaceState)
		if material.ProfileError != nil {
			var unexpressible *envprofiles.UnexpressibleStateError
			var refusal *stableToolError
			switch {
			case errors.As(material.ProfileError, &unexpressible):
				// The backend cannot enforce this plan; preparing it again
				// cannot change that, and the host is not a substitute.
				result.FailureClass = "environment"
				result.RecoveryOutcome = RecoveryNotEligible
				return result, enrichToolFailure(req.ToolName,
					sandboxPreparationError(SandboxBackendName(SandboxIsolated), material.ProfileError), "")
			case errors.As(material.ProfileError, &refusal):
				// A typed refusal is a decision about this plan, not a
				// preparation hiccup a second attempt could clear.
				result.FailureClass = "environment"
				result.RecoveryOutcome = RecoveryNotEligible
				return result, enrichToolFailure(req.ToolName, material.ProfileError, "")
			}
			// Preparing a tool's state overlay failed. Running anyway would
			// produce a misleading downstream error (a partially populated
			// credential store looks like a broken login). The command has not
			// started, so a retry here is unambiguously safe — but only once.
			if attempt == 1 {
				result.RecoveryAttempted = true
				result.RecoveryOutcome = RecoveryPrepared
				continue
			}
			result.RecoveryOutcome = RecoveryNotEligible
			return result, enrichToolFailure(req.ToolName,
				fmt.Errorf("prepare execution environment: %w", material.ProfileError), "")
		}
		if attempt == 1 {
			emitProfilePreparation(runCtx, req.ToolName, req.ToolCallID, material)
		}

		if len(material.HostStatePlan.Writes) > 0 {
			if executionRuntimeFromContext(runCtx) == nil {
				result.FailureClass = "environment"
				result.RecoveryOutcome = RecoveryNotEligible
				return result, enrichToolFailure(req.ToolName,
					sandboxPreparationError(SandboxBackendName(SandboxIsolated), errors.New("sandbox recovery runtime unavailable; command was not started")), "")
			}
			// Marks this command's sandbox so whatever it leaves running can
			// be found and stopped (runGuardedCommand).
			material.SandboxCanary = newSandboxCanary()
		}
		cmd, attemptDecision, attemptPlan, sandboxErr := commandForBoundary(runCtx, argv, material, boundary, req.NetworkShared)
		if sandboxErr != nil {
			result.FailureClass = "environment"
			return result, enrichToolFailure(req.ToolName, sandboxErr, "")
		}
		decision = attemptDecision
		result.NetworkDecision, result.NetworkPermission = recordedNetworkDecision(req, decision)
		// The plan comes back from the construction site rather than being
		// rebuilt here: only that site knows the projection it applied for the
		// decision it resolved.
		result.Plan = attemptPlan
		result.ProfilesMatched = material.Profiles
		result.ScratchBytes = material.ScratchBytes
		if args != nil {
			args["_sandbox_reason"] = decision.Reason
		}
		if attempt == 1 {
			result.HostEscapeReason = classifyHostEscape(decision,
				execCommandProgramsForPayload(req.ToolName, payloadForDiagnostics), payloadForDiagnostics)
			payload := map[string]interface{}{
				"tool_name":          req.ToolName,
				"tool_call_id":       req.ToolCallID,
				"mode":               string(decision.Mode),
				"reason":             decision.Reason,
				"network":            decision.NetworkShared,
				"proxy_mode":         result.Plan.ProxyMode,
				"profiles":           material.Profiles,
				"snapshot_id":        result.Plan.SnapshotID,
				"generation":         result.Plan.Generation,
				"host_escape_reason": result.HostEscapeReason,
			}
			if result.NetworkDecision != "" {
				// Why the network is or is not shared with this call, as evidence.
				payload["network_decision"] = result.NetworkDecision
			}
			if result.NetworkPermission != "" {
				payload["network_permission"] = result.NetworkPermission
			}
			if len(result.Plan.HostStateWrites) > 0 {
				// Evidence of the one part of the host an isolated run may write
				// beyond its roots.
				payload["host_state_writes"] = result.Plan.HostStateWrites
			}
			emitToolProgress(kernel.EventChannelFromContext(runCtx), "tool.sandbox", payload, string(decision.Mode))
		}
		cmd.Dir = req.CWD

		// A command that writes credential caches in place is repaired just
		// before it starts and, from a deferred step that runs however the
		// attempt ends, after it: its leftover processes are stopped and the
		// caches scanned again.
		out, err = runGuardedCommand(runCtx, req, material, cmd, &result, func() (string, error) {
			return runCommandStreaming(runCtx, cmd, payloadForDiagnostics, req.ToolName, req.ToolCallID, req.ToolProfile)
		})
		result.Output = out
		result.Started = result.Started || cmd.Process != nil
		result.ExitCodeKnown = cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0
		result.ExitCode = 0
		if exitErr, ok := err.(interface{ ExitCode() int }); ok {
			result.ExitCode = exitErr.ExitCode()
		} else if err != nil {
			result.ExitCode = -1
		}
		if result.ExitCodeKnown {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}
		if attempt == 1 && shouldRecoverExecution(decision, material, result.ExitCode, err, out, runCtx) {
			result.RecoveryAttempted = true
			result.RecoveryOutcome = RecoveryPrepared
			emitToolProgress(kernel.EventChannelFromContext(runCtx), "tool.recovery", map[string]interface{}{
				"tool_name":    req.ToolName,
				"tool_call_id": req.ToolCallID,
				"outcome":      RecoveryPrepared,
			}, "re-preparing the execution environment and retrying once")
			continue
		}
		break
	}

	if runCtx.Err() == context.DeadlineExceeded {
		result.FailureClass = "timeout"
		failure := fmt.Errorf("command timed out after %s", timeoutSummary(req.ToolProfile))
		return result, enrichSandboxTimeout(req.ToolName, failure, out, decision)
	}
	if runCtx.Err() == context.Canceled {
		// Cancellation is a lifecycle decision, not a diagnosable failure.
		return result, fmt.Errorf("command cancelled")
	}
	if err != nil {
		failure := fmt.Errorf("command failed: %w", err)
		if decision.Mode == SandboxIsolated {
			if class, denied := classifySandboxDenial(result.ExitCode, failure, out); denied {
				result.FailureClass = class
			} else {
				result.FailureClass = ClassifyToolError(req.ToolName, failure, out)
			}
			return result, enrichIsolatedSandboxFailure(req.ToolName, result.ExitCode, failure, out)
		}
		result.FailureClass = ClassifyToolError(req.ToolName, failure, out)
		return result, enrichToolFailure(req.ToolName, failure, out)
	}
	return result, nil
}
