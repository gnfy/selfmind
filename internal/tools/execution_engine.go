package tools

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"selfmind/internal/kernel"
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
const SandboxPlanVersion = 1

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
	// NetworkMode is "isolated" or "shared".
	NetworkMode string `json:"network_mode"`
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
}

// Env returns a copy for the spawn call.
func (m ProcessMaterial) Env() []string {
	out := make([]string, len(m.env))
	copy(out, m.env)
	return out
}

// ScratchTmp is the run's temp directory, needed by the backend to build its
// mount view.
func (m ProcessMaterial) ScratchTmp() string { return m.scratchTmp }

// SandboxBackend enforces a plan. Linux uses bubblewrap; macOS currently uses
// approval-controlled host execution with the SAME plan, snapshot, scratch and
// profiles, so adding a native macOS sandbox later replaces this one interface
// implementation and nothing above it.
type SandboxBackend interface {
	// Name identifies the backend in plans and diagnostics.
	Name() string
	// Available reports whether this backend can enforce a plan on this host.
	Available() bool
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
	NetworkShared  bool
	Timeout        time.Duration
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
	ExitCode          int
	Output            string
	Plan              SandboxPlan
	ProfilesMatched   []string
	FailureClass      string
	RecoveryAttempted bool
	RecoveryOutcome   string
	HostEscapeReason  string
	ScratchBytes      int64
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
	backend := hostBackendName
	if decision.Mode == SandboxIsolated {
		backend = bubblewrapBackendName
	}
	return SandboxPlan{
		Version:       SandboxPlanVersion,
		SnapshotID:    material.SnapshotID,
		Generation:    material.Generation,
		ScratchHandle: material.ScratchHandle,
		WritableRoots: dedupePaths(material.WritableRoots),
		ReadOnlyPaths: dedupePaths(material.ReadOnlyPaths),
		OverlayMounts: append([]SandboxOverlayMount{}, material.OverlayMounts...),
		NetworkMode:   network,
		Profiles:      append([]string{}, material.Profiles...),
		Backend:       backend,
		Mode:          decision.Mode,
		Notes:         append([]string{}, material.ProfileNotes...),
	}
}

// SandboxOverlayMount binds Source over Target inside the sandbox.
type SandboxOverlayMount struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

const (
	bubblewrapBackendName = "bubblewrap"
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
	if runtime.GOOS != "linux" || !ExecSandboxAvailable() {
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
	result := ExecutionResult{RecoveryOutcome: RecoveryNone}
	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	argv := req.Command
	if req.Shell {
		argv = shellArgv(req.Payload)
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
	// At most TWO attempts, and the second only when the engine's own preparation
	// stage is the plausible cause and the command produced no output at all.
	// "Did it produce output" is the decidable proxy for "did it have side
	// effects" — inferring side-effect freedom from command semantics is not
	// something a tool can do correctly, and replaying a command that already ran
	// is worse than reporting it.
	for attempt := 1; attempt <= 2; attempt++ {
		material = execMaterialForRequest(req, args)
		if material.ProfileError != nil {
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

		cmd, attemptDecision, sandboxErr := sandboxedCommandWithMaterial(runCtx, argv, material,
			req.Sandbox, runtime.GOOS, ExecSandboxAvailable(), req.NetworkShared)
		if sandboxErr != nil {
			return result, enrichToolFailure(req.ToolName, sandboxErr, "")
		}
		decision = attemptDecision
		result.Plan = planFromMaterial(material, decision)
		result.ProfilesMatched = material.Profiles
		result.ScratchBytes = material.ScratchBytes
		if args != nil {
			args["_sandbox_reason"] = decision.Reason
		}
		if attempt == 1 {
			result.HostEscapeReason = classifyHostEscape(decision,
				execCommandProgramsForPayload(req.ToolName, payloadForDiagnostics), payloadForDiagnostics)
			emitToolProgress(kernel.EventChannelFromContext(runCtx), "tool.sandbox", map[string]interface{}{
				"tool_name":          req.ToolName,
				"tool_call_id":       req.ToolCallID,
				"mode":               string(decision.Mode),
				"reason":             decision.Reason,
				"network":            decision.NetworkShared,
				"profiles":           material.Profiles,
				"snapshot_id":        result.Plan.SnapshotID,
				"generation":         result.Plan.Generation,
				"host_escape_reason": result.HostEscapeReason,
			}, string(decision.Mode))
		}
		cmd.Dir = req.CWD

		out, err = runCommandStreaming(runCtx, cmd, payloadForDiagnostics, req.ToolName, req.ToolCallID, req.ToolProfile)
		result.Output = out
		result.ExitCode = 0
		if exitErr, ok := err.(interface{ ExitCode() int }); ok {
			result.ExitCode = exitErr.ExitCode()
		} else if err != nil {
			result.ExitCode = -1
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
