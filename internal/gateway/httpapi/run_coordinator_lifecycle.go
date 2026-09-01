package httpapi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/platform/log"
	"selfmind/internal/runpool"
	"selfmind/internal/tools"
	"strings"
	"time"
)

// finalizeErroredRun is the single terminal path for provider, transport, and
// cancellation failures after a run has started. It writes the same structured
// outcome consumed by CLI watchers, IM delivery, recovery, and resume turns.
func (c *RunCoordinator) finalizeErroredRun(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, channel string, runErr error, replay ...runMaintenanceReplay) api.RunOutcome {
	outcome := api.RunOutcome{
		Status:           "interrupted",
		CompletionReason: "provider_or_transport_error",
		Resumable:        true,
		Summary:          "The run was interrupted before the model finished responding.",
		NextSteps:        []string{"Reply \"continue\" to resume from the durable run history."},
	}
	if runErr != nil {
		outcome.Risks = []string{truncate(tools.RedactSensitive(runErr.Error()), 500)}
	}
	eventType := "run.interrupted"
	if errors.Is(context.Cause(ctx), errGatewayShutdown) {
		outcome.CompletionReason = "daemon_recovery"
		outcome.Summary = "The run was interrupted by a gateway restart."
		outcome.NextSteps = []string{"Reply \"continue\" to resume from the last durable evidence."}
		if runErr != nil {
			outcome.Risks = nil
		}
	} else if errors.Is(context.Cause(ctx), runpool.ErrStalled) || errors.Is(runErr, runpool.ErrStalled) {
		outcome.CompletionReason = "stalled"
		outcome.Summary = "The run stopped after execution made no progress within the watchdog window."
		outcome.NextSteps = []string{"Reply \"continue\" to resume from the durable run history."}
		outcome.Risks = []string{"An execution step stopped responding and was cancelled by the watchdog."}
	} else if ctx.Err() != nil || errors.Is(runErr, context.Canceled) {
		// A caller cancellation or caller deadline is terminal: request/eval turn
		// budgets deliberately bound the daemon-owned run. Provider-internal
		// timeouts, EOF, and rate-limit exhaustion leave ctx live and remain
		// resumable interruptions because durable evidence may already exist.
		outcome.Status = "cancelled"
		outcome.CompletionReason = "cancelled"
		outcome.Resumable = false
		outcome.Summary = "The run was cancelled before completion."
		outcome.NextSteps = nil
		eventType = "run.cancelled"
	}

	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil || run == nil {
		return outcome
	}
	finCtx := context.WithoutCancel(ctx)
	var evidence runMaintenanceReplay
	if len(replay) > 0 {
		evidence = replay[0]
	}
	// A successful finish_run is durable before the provider stream necessarily
	// closes. If the transport fails after that point, preserve the model's
	// structured business outcome instead of repainting completed work as an
	// interruption. Cancellation and daemon shutdown remain authoritative.
	structured := false
	if outcome.Status == "interrupted" && outcome.CompletionReason == "provider_or_transport_error" {
		if recorded, ok := c.latestStructuredRunOutcome(finCtx, task.ID, run.ID); ok {
			structured = true
			outcome = reconcileStructuredOutcome(recorded)
			verification, evidenceFiles := c.evidenceOutcome(finCtx, task.ID, run.ID)
			outcome.Verification, outcome.Files = mergeEvidenceFiles(verification, evidenceFiles, outcome.Files)
			outcome = applyVerificationOutcome(outcome)
		}
	}
	handoff := control.Handoff{
		TaskID:    task.ID,
		Summary:   outcome.Summary,
		NextSteps: outcome.NextSteps,
		Risks:     outcome.Risks,
	}
	taskStatus := outcome.Status
	if structured {
		taskStatus = taskStatusForFinalization(outcome, true)
		eventType = "run.finished"
	}
	event := control.Event{
		TaskID:     task.ID,
		RunID:      run.ID,
		Type:       eventType,
		Visibility: "task",
		Channel:    channel,
		Payload: mustJSON(map[string]interface{}{
			"outcome": outcome,
			"error":   firstString(outcome.Risks),
		}),
	}
	if err := c.materializeRunFinalization(finCtx, identity, task, run, taskStatus,
		evidence.WorkspaceID, evidence.UserInput, channel, "", outcome, evidence.Attach, handoff, event); err != nil {
		return outcome
	}
	if outcome.Status == "interrupted" && outcome.Resumable &&
		(outcome.CompletionReason == "provider_or_transport_error" || outcome.CompletionReason == "daemon_recovery") {
		_, _ = c.srv.scheduleAutomaticRunRecovery(finCtx, control.RecoveryNotification{
			TenantID: identity.TenantID, PersonID: identity.PersonID,
			TaskID: task.ID, RunID: run.ID, Channel: channel, Title: task.Title,
		}, false)
	}
	return outcome
}

func firstString(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return items[0]
}

func (c *RunCoordinator) prepareRequestWorkspace(ctx context.Context, identity *control.IdentityContext, req *api.MessageRequest) (*control.Workspace, error) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || req == nil {
		return nil, nil
	}
	store := c.srv.Control
	if strings.TrimSpace(req.WorkspaceID) != "" {
		return store.GetWorkspace(ctx, identity.TenantID, req.WorkspaceID)
	}
	if !isLocalCLIRequest(*req) {
		// IM and scheduled turns have no trustworthy client cwd. Bind the
		// person's durable workspace selection to this request explicitly so a
		// stale task pre-label cannot supply an older execution directory.
		ws, err := store.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			return nil, err
		}
		if ws != nil {
			req.WorkspaceID = ws.ID
		}
		return ws, nil
	}
	cwd := cleanClientCWD(req.ClientCWD)
	if cwd == "" {
		return nil, nil
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	ws, err := store.EnsureWorkspace(ctx, control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          filepath.Base(cwd),
		LocalPath:     cwd,
		AllowedRoots:  []string{cwd},
	})
	if err != nil {
		return nil, err
	}
	if ws != nil {
		req.WorkspaceID = ws.ID
	}
	return ws, nil
}

func (c *RunCoordinator) recordOutcomeArtifacts(ctx context.Context, task *control.Task, run *control.Run, channel string, files []string) {
	if c == nil || c.srv == nil || c.srv.Control == nil || task == nil {
		return
	}
	store := c.srv.Control
	seen := map[string]struct{}{}
	for _, uri := range files {
		uri = strings.TrimSpace(uri)
		if uri == "" {
			continue
		}
		if _, ok := seen[uri]; ok {
			continue
		}
		seen[uri] = struct{}{}
		artifact := control.Artifact{
			TaskID: task.ID,
			Kind:   artifactKind(uri),
			Name:   artifactName(uri),
			URI:    uri,
			Metadata: mustJSON(map[string]interface{}{
				"source": "run_outcome",
			}),
		}
		if run != nil {
			artifact.RunID = run.ID
		}
		saved, err := store.SaveArtifact(ctx, artifact)
		if err != nil {
			continue
		}
		runID := ""
		if run != nil {
			runID = run.ID
		}
		_, _ = store.AppendEvent(ctx, control.Event{
			TaskID:     task.ID,
			RunID:      runID,
			Type:       "artifact.created",
			Visibility: "task",
			Channel:    channel,
			Payload: mustJSON(map[string]interface{}{
				"artifact": saved,
			}),
		})
	}
}

// resolveTask decides which task this turn runs under (simplification P2,
// Deterministic evidence wins in a fixed ladder: structured return edges (approval origin run,
// platform reply metadata), a caller-supplied task id, the one-shot /resume
// pin, then an explicit continuation cue resolved through the person-wide
// unclaimed-run ladder (same channel first, then global; several candidates
// stay visible, none is guessed). Everything else owns a FRESH root task:
// grouping is display-only — context comes from the spine and the parent-run
// slice, so a new task per message can never corrupt execution.
// task_runs.task_id stays NOT NULL and the control plane
// (queue/approvals/busy/steer) is untouched.
func (c *RunCoordinator) resolveTask(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) (*control.Task, taskAttach, error) {
	store := c.srv.Control
	inputWorkKey := uniqueTaskWorkKey(req.Content)
	// Structured return edges win before every guess (simplification §5.3
	// steps 2–3): an approval continuation names its origin run via the parked
	// approval row, and a platform reply names the exact run it answers. Both
	// bind a precise parent; an invalid, foreign, or stale id fails closed so it
	// can never be applied to another pending item.
	approvalID := strings.TrimSpace(req.ApprovalID)
	if approvalID != "" {
		approval, err := store.GetApprovalRequest(ctx, identity.TenantID, approvalID)
		if err != nil {
			return nil, taskAttach{}, fmt.Errorf("load approval continuation: %w", err)
		}
		if approval == nil || approval.PersonID != identity.PersonID || approval.TaskID == "" || approval.RunID == "" ||
			(approval.Status != "approved" && approval.Status != "rejected") {
			return nil, taskAttach{}, fmt.Errorf("approval continuation is invalid or no longer available")
		}
		task, err := store.GetTask(ctx, identity.TenantID, approval.TaskID)
		if err != nil {
			return nil, taskAttach{}, fmt.Errorf("load approval task: %w", err)
		}
		if task == nil || task.PersonID != identity.PersonID {
			return nil, taskAttach{}, fmt.Errorf("approval task is invalid or no longer available")
		}
		task, err = c.bindTaskWorkspaceIfMissing(ctx, identity, task, req, nil)
		attach := newTaskAttach(taskAttachApprovalResume, inputWorkKey, false, false)
		attach.parentRunID = approval.RunID
		return task, attach, err
	}
	clarifyID := strings.TrimSpace(req.ClarifyID)
	if clarifyID != "" {
		clarify, err := store.GetClarifyRequest(ctx, identity.TenantID, clarifyID)
		if err != nil {
			return nil, taskAttach{}, fmt.Errorf("load clarification continuation: %w", err)
		}
		if clarify == nil || clarify.PersonID != identity.PersonID || clarify.TaskID == "" || clarify.RunID == "" ||
			clarify.Status != "answered" || strings.TrimSpace(clarify.Answer) == "" {
			return nil, taskAttach{}, fmt.Errorf("clarification continuation is invalid or no longer available")
		}
		task, err := store.GetTask(ctx, identity.TenantID, clarify.TaskID)
		if err != nil {
			return nil, taskAttach{}, fmt.Errorf("load clarification task: %w", err)
		}
		if task == nil || task.PersonID != identity.PersonID {
			return nil, taskAttach{}, fmt.Errorf("clarification task is invalid or no longer available")
		}
		task, err = c.bindTaskWorkspaceIfMissing(ctx, identity, task, req, nil)
		attach := newTaskAttach(taskAttachClarifyResume, inputWorkKey, false, false)
		attach.parentRunID = clarify.RunID
		return task, attach, err
	}
	if replyRunID := strings.TrimSpace(req.ReplyToRunID); replyRunID != "" {
		parent, err := store.GetRun(ctx, identity.TenantID, replyRunID)
		if err != nil {
			return nil, taskAttach{}, fmt.Errorf("load reply parent: %w", err)
		}
		if parent == nil || parent.PersonID != identity.PersonID {
			return nil, taskAttach{}, fmt.Errorf("reply target is invalid or no longer available")
		}
		task, err := store.GetTask(ctx, identity.TenantID, parent.TaskID)
		if err != nil {
			return nil, taskAttach{}, fmt.Errorf("load reply task: %w", err)
		}
		if task == nil || task.PersonID != identity.PersonID {
			return nil, taskAttach{}, fmt.Errorf("reply task is invalid or no longer available")
		}
		task, err = c.bindTaskWorkspaceIfMissing(ctx, identity, task, req, nil)
		attach := newTaskAttach(taskAttachReplyToRun, inputWorkKey, false, false)
		attach.parentRunID = parent.ID
		return task, attach, err
	}
	if req.ForceNew {
		// /new is the deterministic escape hatch when continuity resolution is
		// unavailable or the user rejects every historical candidate. Consume a
		// stale one-shot /resume pin so it cannot capture a later message, then
		// create a root regardless of current-task projection or cue text.
		_, _, _ = c.srv.consumeResumePin(ctx, identity)
		task, attach, err := c.createRootTask(ctx, identity, req)
		attach.workKey = inputWorkKey
		return task, attach, err
	}
	if req.TaskID != "" {
		task, err := store.GetTask(ctx, identity.TenantID, req.TaskID)
		if err != nil || task != nil {
			task, err = c.bindTaskWorkspaceIfMissing(ctx, identity, task, req, err)
			reason := taskAttachExplicitTaskID
			if intent.Intent == router.IntentContinue {
				reason = taskAttachContinuation
			}
			return task, newTaskAttach(reason, inputWorkKey, false, false), err
		}
	}
	// The /resume pin covers exactly the NEXT agent-bound message, so it is
	// consumed here no matter which branch wins below — a stale pin must never
	// capture unrelated work later.
	pinned, pinnedRunID, pinErr := c.srv.consumeResumePin(ctx, identity)
	if pinErr != nil {
		return nil, taskAttach{}, pinErr
	}
	// An explicit /resume is a deliberate act, so it may reopen a label the
	// person completed or archived. Cancelled/failed labels stay closed.
	if pinned != nil && (!terminalTaskStatus(pinned.Status) || resumableTaskStatus(pinned.Status)) {
		task, err := c.bindTaskWorkspaceIfMissing(ctx, identity, pinned, req, nil)
		attach := newTaskAttach(taskAttachResumePin, inputWorkKey, false, false)
		attach.parentRunID = pinnedRunID
		return task, attach, err
	}
	// §5.3 steps 5–7: an explicit continuation cue continues the unique
	// unclaimed resumable run — same channel first, then person-global. Task
	// References and the current-task pointer no longer route anything
	// (simplification P2): references are search hints, current_task is a UI
	// projection.
	if intent.Intent == router.IntentContinue {
		task, attach, err := c.resolveContinuationByRuns(ctx, identity, req, inputWorkKey)
		if err != nil || task != nil {
			return task, attach, err
		}
		// No pending run anywhere: the cue has nothing to continue, so the
		// message is ordinary new work (§5.3 step 8) — the spine tail carries
		// any conversational context it referred to.
	}
	// §5.3 step 8: every ordinary message owns a fresh root task. Wrong-looking
	// grouping is a display concern only — context comes from the spine and the
	// parent-run slice, and the default /tasks view ranks one-shot Q&A last.
	task, attach, err := c.createRootTask(ctx, identity, req)
	attach.workKey = inputWorkKey
	return task, attach, err
}

// continuationCandidatesError carries the deterministic cross-task candidate
// set for an ambiguous person-typed continuation. runMessage renders it; it is
// a control-flow signal, not a failure.
type continuationCandidatesError struct {
	runs []control.Run
}

func (e *continuationCandidatesError) Error() string {
	return fmt.Sprintf("continuation matches %d unfinished runs", len(e.runs))
}

// resolveContinuationByRuns implements the §5.3 candidate ladder for a
// deliberate continuation cue. It returns (nil, zero, nil) when the person has
// no pending run at all — the caller then treats the message as new work.
func (c *RunCoordinator) resolveContinuationByRuns(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, inputWorkKey string) (*control.Task, taskAttach, error) {
	store := c.srv.Control
	candidates, err := store.ListUnresolvedRunsForPerson(ctx, identity.TenantID, identity.PersonID, req.Channel, 5)
	if err != nil {
		return nil, taskAttach{}, err
	}
	if len(candidates) == 0 {
		candidates, err = store.ListUnresolvedRunsForPerson(ctx, identity.TenantID, identity.PersonID, "", 5)
		if err != nil {
			return nil, taskAttach{}, err
		}
	}
	switch len(candidates) {
	case 0:
		return nil, taskAttach{}, nil
	case 1:
		parent := candidates[0]
		task, err := store.GetTask(ctx, identity.TenantID, parent.TaskID)
		if err != nil || task == nil {
			return nil, taskAttach{}, err
		}
		task, err = c.bindTaskWorkspaceIfMissing(ctx, identity, task, req, nil)
		attach := newTaskAttach(taskAttachContinuation, inputWorkKey, false, false)
		attach.parentRunID = parent.ID
		return task, attach, err
	default:
		if !isUserOriginTurn(ctx, req) {
			// A daemon-originated text (cron output that happens to contain a
			// cue word) is never asked to disambiguate; it proceeds as new work.
			return nil, taskAttach{}, nil
		}
		return nil, taskAttach{}, &continuationCandidatesError{runs: candidates}
	}
}

// createRootTask creates the fresh root task every ordinary message owns
// (simplification P2). An explicit request workspace wins; otherwise the
// person's current workspace seeds the label without making it an execution
// boundary.
func (c *RunCoordinator) createRootTask(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (*control.Task, taskAttach, error) {
	store := c.srv.Control
	workspaceID := req.WorkspaceID
	if workspaceID == "" {
		if ws, _ := store.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID); ws != nil {
			workspaceID = ws.ID
		}
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID:    identity.TenantID,
		PersonID:    identity.PersonID,
		WorkspaceID: workspaceID,
		Title:       titleFromInput(req.Content),
		Channel:     req.Channel,
	})
	return task, newTaskAttach(taskAttachNewLabel, "", true, true), err
}

func (c *RunCoordinator) bindTaskWorkspaceIfMissing(ctx context.Context, identity *control.IdentityContext, task *control.Task, req api.MessageRequest, priorErr error) (*control.Task, error) {
	if priorErr != nil || task == nil || identity == nil {
		return task, priorErr
	}
	if task.WorkspaceID != "" || strings.TrimSpace(req.WorkspaceID) == "" {
		return task, nil
	}
	store := c.srv.Control
	if err := store.SetTaskWorkspace(ctx, identity.TenantID, task.ID, req.WorkspaceID); err != nil {
		return nil, err
	}
	refreshed, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || refreshed == nil {
		return task, err
	}
	return refreshed, nil
}

func (c *RunCoordinator) workspaceForTask(ctx context.Context, identity *control.IdentityContext, task *control.Task, req api.MessageRequest, attach taskAttach) (*control.Workspace, error) {
	store := c.srv.Control
	// For execution-authoritative attaches (task id, plain continuation cue,
	// /resume pin) the task's
	// own workspace binding is authoritative and must survive a resume/continue:
	// a CLI `继续` of an IM task must run in the dir the prior run built in, not
	// the terminal's cwd-derived workspace (prepareRequestWorkspace sets
	// req.WorkspaceID from ClientCWD for local CLI turns) — otherwise every file
	// op trips out-of-root approvals against the wrong tree.
	//
	// For a PRE-LABEL or governed-reference guess the label is display-only, so
	// the REQUEST wins: the
	// run executes in the explicitly requested (or cwd-derived) workspace even
	// when the guessed label is bound elsewhere. This is what makes a wrong
	// pre-label harmless — execution scope never inherits a guessed label's
	// workspace (Work Timeline P3).
	workspaceID := strings.TrimSpace(req.WorkspaceID)
	if attach.resolvedPolicy().ExecutionWorkspaceSource == attachWorkspaceTask && task != nil {
		workspaceID = strings.TrimSpace(task.WorkspaceID)
	}
	if workspaceID == "" {
		return store.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID)
	}
	return store.GetWorkspace(ctx, identity.TenantID, workspaceID)
}

func (c *RunCoordinator) installExecutionScope(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, workspace *control.Workspace, req api.MessageRequest, leases ...*executionenv.Lease) func() {
	if identity == nil {
		return func() {}
	}
	scope := tools.ExecutionScope{
		TenantID:         identity.TenantID,
		PersonID:         identity.PersonID,
		ExecutionProfile: req.ExecutionProfile,
	}
	if workspace != nil {
		scope.WorkspaceID = workspace.ID
		scope.WorkspaceRoot = workspace.LocalPath
		scope.AllowedRoots = workspace.AllowedRoots
		scope.TrustLevel = workspace.TrustLevel
	}
	bindings := req.ExecutionRoots
	if len(bindings) == 0 && run != nil {
		bindings = run.ExecutionRoots
	}
	expandsWorkspaceAuthority := false
	if len(bindings) > 0 {
		scope.RootBindings = executionenv.CloneRootBindings(bindings)
		scope.AllowedRoots = executionenv.RootPaths(bindings)
		for _, binding := range bindings {
			if binding.Role == executionenv.RootRolePrimary {
				scope.WorkspaceRoot = binding.Path
				break
			}
		}
		// A one-run path grant is not a durable workspace trust act. Until
		// trust becomes per-root, the safe aggregate is the least-trusted root.
		expandsWorkspaceAuthority = rootsExpandWorkspaceAuthority(bindings)
		if expandsWorkspaceAuthority {
			scope.TrustLevel = executionenv.TrustUntrusted
		}
	}
	if len(leases) > 0 && leases[0] != nil {
		scope.LeaseID = leases[0].ID
		scope.EnvironmentSnapshotID = leases[0].EnvironmentSnapshotID
		scope.EnvironmentGeneration = leases[0].EnvironmentGeneration
		scope.EnvironmentFingerprint = leases[0].EnvironmentFingerprint
		scope.PrincipalFingerprint = leases[0].PrincipalFingerprint
		scope.CredentialSourceHash = leases[0].CredentialSourceHash
		scope.Capabilities = append([]string{}, leases[0].ExecutionCapabilities...)
	}
	// A turn carrying attachments may read the person's imported-attachment
	// partition (importAttachments copies the files there) in addition to the
	// workspace roots. Copy-on-append: workspace.AllowedRoots must not be
	// mutated, and an empty AllowedRoots list must keep its implicit
	// workspace-root fallback (scopeAllowsPath) rather than lose it.
	if len(req.Attachments) > 0 {
		if root := c.personAttachmentsRoot(identity); root != "" {
			roots := scope.AllowedRoots
			if len(roots) == 0 && scope.WorkspaceRoot != "" {
				roots = []string{scope.WorkspaceRoot}
			}
			scope.AllowedRoots = append(append([]string{}, roots...), root)
			scope.RootBindings = append(scope.RootBindings, executionenv.RootBinding{
				Path: root, Role: executionenv.RootRoleAttachment,
				AccessCap: executionenv.RootAccessRead, Source: executionenv.RootSourceAttachment,
			})
		}
	}
	if task != nil {
		scope.TaskID = task.ID
	}
	if run != nil {
		scope.RunID = run.ID
		scope.Channel = run.Channel
	}
	// Carry the execution policy WITH the request. Reading it from a process
	// global at execution time meant one daemon could only ever have one policy;
	// snapshotting it here makes the request self-describing and is what a
	// separate execution node would receive.
	scope.SandboxPolicy = tools.CurrentExecSandboxPolicy()
	scope.Approval = c.toolApprovalHandler(identity, task, run, scope.Channel)
	scope.Clarify = c.gatewayClarify(ctx, identity, task, run, scope.Channel)
	scope.ApprovalMode = c.resolveApprovalMode(identity, req.ApprovalMode)
	// Live mode: re-resolve at EACH ask with the same precedence as run start
	// (explicit request mode wins, else the person's CURRENT persisted /mode).
	// This is what makes `/mode smart` sent from IM mid-run govern the
	// in-flight run's later approval decisions instead of a frozen snapshot.
	reqMode := req.ApprovalMode
	scope.ModeGetter = func() tools.ApprovalMode {
		return c.resolveApprovalMode(identity, reqMode)
	}
	// The person's own words for this turn, so smart-mode triage can judge
	// AUTHORIZATION and not only risk: "delete the build directory" makes a
	// destructive-looking command an instruction, while the same command with no
	// such request is the model acting alone. Bounded and redacted here because
	// the judge prompt treats it as untrusted data (docs/tool-safety.md).
	intentSnapshot := runIntentSnapshot(req, task, run, workspace)
	scope.IntentSnapshot = func() tools.RunIntentSnapshot { return intentSnapshot }
	// Grants back class-level approval memory (session/persistent allowlist);
	// the control store satisfies tools.ApprovalGrantStore structurally.
	if c.srv != nil && c.srv.Control != nil {
		scope.Grants = c.srv.Control
		// Workspace-scoped network/credential capabilities were approved for
		// the registered roots, not an arbitrary invocation-local directory.
		// An expanded run can still request a run-local approval, but must not
		// inherit or persist a capability through the workspace identity.
		if !expandsWorkspaceAuthority {
			scope.CapabilityStore = c.srv.Control
		}
		scope.ResumeAuthorizations = c.srv.Control
	}
	// Judge backs smart-mode LLM approval triage (H2). Optional: nil leaves smart
	// mode on the human-ask path (never auto-approves without a judge).
	if c.srv != nil {
		scope.Judge = c.srv.ApprovalJudge
	}
	releaseScope := tools.SetExecutionScope(identity.PersonID, scope)
	leaseID := scope.LeaseID
	return func() {
		releaseScope()
		// The lease→snapshot binding is per-run process state. Dropping it when
		// the run's scope goes away keeps the map bounded on a long-lived daemon;
		// the snapshot itself stays available for other runs on the same
		// generation, and the scratch directory survives for its TTL so a
		// finished run's intermediates remain inspectable.
		if leaseID != "" {
			executionenv.DefaultRegistry().ReleaseLease(leaseID)
		}
	}
}

// materializeExecutionLease binds one run to the operator environment that
// existed when the run started. Durable state contains credential source names
// and a non-secret principal fingerprint only; raw values stay process-local.
func (c *RunCoordinator) materializeExecutionLease(ctx context.Context, identity *control.IdentityContext, run *control.Run, workspace *control.Workspace) (*executionenv.Lease, error) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || run == nil {
		return nil, fmt.Errorf("execution lease dependencies are unavailable")
	}
	refs, principal := tools.SnapshotCredentialRefs(os.Environ())
	snapshot := executionenv.DefaultRegistry().Current()
	if snapshot == nil {
		// First run after a start that did not install one (e.g. an embedded
		// gateway in a test): install now rather than letting the execution
		// path fall back to a raw read of the daemon environment.
		snapshot = tools.InstallEnvironmentSnapshot(os.Environ(), "inherited")
	}
	capabilities := make([]string, 0, 4)
	workspaceID := ""
	if workspace != nil {
		workspaceID = workspace.ID
		if !rootsExpandWorkspaceAuthority(run.ExecutionRoots) {
			grants, err := c.srv.Control.ListActiveExecutionCapabilities(ctx, identity.TenantID, identity.PersonID, workspace.ID)
			if err != nil {
				return nil, fmt.Errorf("list execution capabilities: %w", err)
			}
			for _, grant := range grants {
				capabilities = append(capabilities, grant.Capability)
			}
			if workspace.TrustLevel == executionenv.TrustTrusted && tools.ExecSandboxAllowsNetwork() {
				capabilities = append(capabilities, executionenv.CapabilityNetworkShared)
			}
		}
	}
	lease, err := c.srv.Control.MaterializeExecutionLease(ctx, executionenv.Lease{
		RunID:                  run.ID,
		TenantID:               identity.TenantID,
		PersonID:               identity.PersonID,
		WorkspaceID:            workspaceID,
		EnvironmentProfile:     "operator",
		CredentialRefs:         refs,
		PrincipalFingerprint:   principal,
		ExecutionCapabilities:  capabilities,
		EnvironmentSnapshotID:  snapshot.ID,
		EnvironmentGeneration:  snapshot.Generation,
		EnvironmentFingerprint: snapshot.EnvironmentFingerprint,
		CredentialSourceHash:   snapshot.CredentialSourceHash,
	})
	if err != nil {
		return nil, err
	}
	// A replayed or recovered run returns its ORIGINAL lease, whose snapshot may
	// no longer exist in this process (a restart clears the registry). Rebinding
	// is only safe when the environment still describes the same account,
	// toolchain and credential sources; otherwise the run must not silently
	// continue under a different environment.
	bound, err := c.rebindLeaseEnvironment(ctx, lease, snapshot)
	if err != nil {
		return nil, err
	}
	return bound, nil
}

// rebindLeaseEnvironment resolves a lease's environment binding for this
// process. Fresh leases bind to the current snapshot. A lease that predates a
// restart is rebuilt only when all three fingerprints match; a changed PATH,
// account, or credential source is reported so the caller can park the work
// instead of continuing under an environment the run never agreed to.
func (c *RunCoordinator) rebindLeaseEnvironment(ctx context.Context, lease *executionenv.Lease, snapshot *executionenv.Snapshot) (*executionenv.Lease, error) {
	if lease == nil || snapshot == nil {
		return lease, nil
	}
	registry := executionenv.DefaultRegistry()
	if _, ok := registry.Get(lease.EnvironmentSnapshotID); ok {
		registry.BindLease(lease.ID, lease.EnvironmentSnapshotID)
		return lease, nil
	}
	if lease.EnvironmentSnapshotID == "" {
		// A lease written before environment binding existed: adopt the current
		// snapshot and record it, so later commands of this run are stable.
		registry.BindLease(lease.ID, snapshot.ID)
		return c.srv.Control.UpdateExecutionLeaseEnvironment(ctx, lease.TenantID, lease.ID, snapshot.ID,
			snapshot.Generation, snapshot.EnvironmentFingerprint, snapshot.CredentialSourceHash)
	}
	rebuilt := lease.EnvironmentFingerprint == snapshot.EnvironmentFingerprint &&
		lease.CredentialSourceHash == snapshot.CredentialSourceHash &&
		lease.PrincipalFingerprint == snapshot.PrincipalFingerprint
	if !rebuilt {
		return nil, &executionenv.EnvironmentChangedError{
			LeaseID: lease.ID,
			Changed: executionenv.DescribeEnvironmentChange(lease, snapshot),
		}
	}
	registry.BindLease(lease.ID, snapshot.ID)
	updated, err := c.srv.Control.UpdateExecutionLeaseEnvironment(ctx, lease.TenantID, lease.ID, snapshot.ID,
		snapshot.Generation, snapshot.EnvironmentFingerprint, snapshot.CredentialSourceHash)
	if err != nil {
		return nil, err
	}
	c.appendEnvironmentRebuiltEvent(ctx, lease, snapshot)
	return updated, nil
}

// appendEnvironmentRebuiltEvent records that a recovered run's environment
// snapshot was rebuilt after a restart. The generation change must be visible:
// silently rebinding is how a run would appear to continue unchanged while its
// environment binding was replaced. Non-secret metadata only.
func (c *RunCoordinator) appendEnvironmentRebuiltEvent(ctx context.Context, lease *executionenv.Lease, snapshot *executionenv.Snapshot) {
	if c == nil || c.srv == nil || c.srv.Control == nil || lease == nil || snapshot == nil {
		return
	}
	run, err := c.srv.Control.GetRun(ctx, lease.TenantID, lease.RunID)
	if err != nil || run == nil || run.TaskID == "" {
		return
	}
	_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
		TaskID:     run.TaskID,
		RunID:      lease.RunID,
		Type:       "environment.snapshot_rebuilt",
		Visibility: "internal",
		Payload: mustJSON(map[string]interface{}{
			"lease_id":       lease.ID,
			"snapshot_id":    snapshot.ID,
			"generation":     snapshot.Generation,
			"volatile_count": snapshot.VolatileCount,
			"source":         snapshot.Source,
		}),
	})
}

// resolveApprovalMode applies the approval-mode precedence: an explicit
// per-request mode wins; otherwise the person's persisted /mode preference
// (approval_mode); otherwise the smart product default. This is what makes an IM `/mode smart`
// apply to later messages that carry no mode of their own.
func (c *RunCoordinator) resolveApprovalMode(identity *control.IdentityContext, reqMode string) tools.ApprovalMode {
	modeStr := strings.TrimSpace(reqMode)
	if modeStr == "" && c != nil && c.srv != nil && c.srv.Control != nil && identity != nil {
		if pref, err := c.srv.Control.GetPersonSetting(context.Background(), identity.TenantID, identity.PersonID, personSettingApprovalMode); err == nil {
			modeStr = pref
		}
	}
	return tools.EffectiveApprovalMode(modeStr)
}

// gatewayClarify answers the clarify tool as a FIRST-CLASS pending question,
// modeled exactly on the approval waiter (toolApprovalHandler): it creates a
// durable clarify_requests row, appends the clarify.requested event, pushes a
// presence-aware notification to the person's endpoints, then blocks polling the
// DB row until an answer arrives or the wait bound / run ctx expires. An answer
// (recorded from ANY endpoint via AnswerClarifyRequest) is returned verbatim as
// the tool result; a timeout expires the row and returns the best-judgment
// sentinel so the run never hangs. This is what lets a question survive the CLI
// closing (docs/identity-continuity.md "Runtime attachment model").
func (c *RunCoordinator) gatewayClarify(runCtx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, channel string) tools.ClarifyHandler {
	return func(question string, choices []string) string {
		if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil {
			return clarifyFallbackSentinel
		}
		store := c.srv.Control
		taskID, runID := "", ""
		if task != nil {
			taskID = task.ID
		}
		if run != nil {
			runID = run.ID
		}
		reqChannel := fallback(channel, identity.Platform)
		if runCtx == nil {
			runCtx = context.Background()
		}
		waitCtx, cancel := context.WithTimeout(runCtx, clarifyWaitTimeout)
		defer cancel()

		clarify, err := store.CreateClarifyRequest(waitCtx, control.ClarifyRequest{
			TenantID: identity.TenantID,
			PersonID: identity.PersonID,
			TaskID:   taskID,
			RunID:    runID,
			Question: question,
			Options:  mustJSON(choices),
			Channel:  reqChannel,
		})
		if err != nil {
			// Cannot durably record the question: fall back rather than hang.
			return clarifyFallbackSentinel
		}
		if taskID != "" {
			_, _ = store.AppendEvent(waitCtx, control.Event{
				TaskID:     taskID,
				RunID:      runID,
				Type:       "clarify.requested",
				Visibility: "task",
				Channel:    reqChannel,
				Payload:    mustJSON(map[string]interface{}{"clarify_id": clarify.ID, "question": question, "choices": choices}),
			})
		}
		resumeWatchdog := runpool.BeginPersonWait(runCtx, runpool.PhaseWaitingClarify)
		defer resumeWatchdog()
		c.notifyClarifyRequested(context.Background(), identity, taskID, runID, reqChannel, clarify)

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				// A planned gateway shutdown parks the origin run as interrupted.
				// Preserve its question: the next endpoint answer creates an exact
				// child continuation through ClarifyID. A timeout or ordinary
				// cancellation is terminal for this waiter and must expire the row
				// so it cannot intercept an unrelated later message.
				if !errors.Is(context.Cause(waitCtx), errGatewayShutdown) {
					_ = store.ExpireClarifyRequest(context.WithoutCancel(waitCtx), identity.TenantID, clarify.ID, "waiter gone: "+waitCtx.Err().Error())
				}
				return clarifyFallbackSentinel
			case <-ticker.C:
				current, err := store.GetClarifyRequest(waitCtx, identity.TenantID, clarify.ID)
				if err != nil || current == nil {
					// Store hiccup / row vanished: don't hang, fall back.
					return clarifyFallbackSentinel
				}
				switch current.Status {
				case "answered":
					if answer := strings.TrimSpace(current.Answer); answer != "" {
						return answer
					}
					return clarifyFallbackSentinel
				case "expired":
					return clarifyFallbackSentinel
				}
			}
		}
	}
}

// approvalWaitReserve is the slice of the caller's remaining budget kept for
// everything that has to happen AFTER the wait: parking the durable row,
// appending the event, handing the typed decision back through the tool
// middleware, and — the part that made the first value too small — one more
// model round-trip for the agent to actually park the work and say so.
//
// Three seconds covered only the waiter's own cleanup. The decision came back
// in time, the agent then had no budget left to finalize, and the run still
// died on a bare transport timeout with nothing to show for it. The reserve
// has to cover the turn that reports the parked work, not just the bookkeeping
// that precedes it.
const (
	approvalWaitReserve = 30 * time.Second
	// Defaults mirror config.DefaultApprovalWait{,Unattended}. They are
	// duplicated rather than imported on purpose: httpapi takes resolved
	// policy through Server fields and does not depend on the config package.
	defaultApprovalWait           = 30 * time.Minute
	defaultApprovalWaitUnattended = 30 * time.Second
	// approvalPollTimeout bounds one status read of the pending row. It is
	// independent of the wait budget so a lapsing budget cannot turn a poll
	// into a reported transport error.
	approvalPollTimeout = 5 * time.Second
)

// approvalWaitBudget bounds one approval wait. It is the smaller of the
// configured budget for this person's reachability and whatever the caller's
// own deadline leaves. A non-positive result means "there is no time to ask".
func (c *RunCoordinator) approvalWaitBudget(ctx context.Context, identity *control.IdentityContext) time.Duration {
	budget := c.srv.ApprovalWait
	if budget <= 0 {
		budget = defaultApprovalWait
	}
	if !c.personCanAnswer(ctx, identity) {
		budget = c.srv.ApprovalWaitUnattended
		if budget <= 0 {
			budget = defaultApprovalWaitUnattended
		}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return budget
	}
	if remaining := time.Until(deadline) - approvalWaitReserve; remaining < budget {
		return remaining
	}
	return budget
}

// personCanAnswer reports whether a live endpoint exists or the delivery layer
// can route to a configured IM sender. It deliberately avoids a 24-hour
// last_seen guess: a stale timestamp cannot prove reachability, and a newly
// bound endpoint should not be penalized for lacking history. The actual send
// still records confirmed/unconfirmed/failure in the outbox; if nobody answers,
// the resource budget parks rather than invalidates the approval.
func (c *RunCoordinator) personCanAnswer(ctx context.Context, identity *control.IdentityContext) bool {
	if identity == nil {
		return false
	}
	if c.srv.presenceTracker().AnyAttached(identity.PersonID) {
		return true
	}
	account := c.preferredIMAccount(ctx, identity)
	if account == nil {
		return false
	}
	state, err := c.srv.Control.LatestDeliveryEndpointState(ctx, identity.TenantID, identity.PersonID, account.Platform, account.PlatformUserID)
	if err != nil {
		// Fail open on observation errors: parking remains the safe terminal
		// behavior, whereas prematurely abandoning a reachable decision only
		// burns the current run budget faster.
		return true
	}
	if state != nil {
		switch strings.ToLower(strings.TrimSpace(state.Status)) {
		case "pending_session", "failed", "sent_unconfirmed":
			return false
		}
	}
	return true
}

func (c *RunCoordinator) toolApprovalHandler(identity *control.IdentityContext, task *control.Task, run *control.Run, channel string) tools.ToolApprovalHandler {
	return func(ctx context.Context, req tools.ToolApprovalRequest) (tools.ToolApprovalDecision, error) {
		if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil {
			return tools.ToolApprovalDecision{}, fmt.Errorf("approval store is not available")
		}
		store := c.srv.Control
		if ctx == nil {
			ctx = context.Background()
		}
		taskID := req.TaskID
		runID := req.RunID
		if taskID == "" && task != nil {
			taskID = task.ID
		}
		if runID == "" && run != nil {
			runID = run.ID
		}

		// The wait must END BEFORE the caller's deadline. The timeout branch
		// below is the designed "park the work" path, but it only runs if this
		// waiter is still alive to return its decision: when the run context
		// expired first, the whole turn died with a bare "context deadline
		// exceeded" and produced no answer at all (observed in eval, where the
		// case budget is far below the old hardcoded 30 minutes).
		budget := c.approvalWaitBudget(ctx, identity)
		if budget <= 0 {
			// No time left to ask. Creating a durable row plus a push
			// notification for a request that would expire in the same breath
			// is pure noise, so record it as a run event instead and park.
			if taskID != "" {
				_, _ = store.AppendEvent(context.WithoutCancel(ctx), control.Event{
					TaskID:     taskID,
					RunID:      runID,
					Type:       "approval.skipped_no_budget",
					Visibility: "task",
					Channel:    fallback(channel, identity.Platform),
					Payload: mustJSON(map[string]interface{}{
						"tool":   req.ToolName,
						"reason": req.Reason,
						"target": approvalActionTarget(req.ToolName, req.Args),
					}),
				})
			}
			return tools.ToolApprovalDecision{
				Approved: false,
				Outcome:  tools.ApprovalOutcomeTimedOut,
				Reason:   "no time left in this run to ask for approval; do not request another approval or retry a variant; finish waiting_user",
			}, nil
		}
		waitCtx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		decisions := buildApprovalDecisions(req)
		persistentArgs := tools.ApprovalPersistentArgs(req.ToolName, req.Args)
		approval, err := store.CreateApprovalRequest(waitCtx, control.ApprovalRequest{
			TenantID:                 identity.TenantID,
			PersonID:                 identity.PersonID,
			TaskID:                   taskID,
			RunID:                    runID,
			ActionType:               "tool_call",
			RequestedChannel:         fallback(channel, identity.Platform),
			AuthorizationFingerprint: req.AuthorizationFingerprint,
			Payload: mustJSON(map[string]interface{}{
				"tool":            req.ToolName,
				"target":          approvalActionTarget(req.ToolName, req.Args),
				"reason":          req.Reason,
				"args":            persistentArgs,
				"grant_class":     req.GrantClass,
				"run_grant_class": req.RunGrantClass,
				"containment":     req.Containment,
				// WHERE it runs and HOW BIG the change is: a decision made without
				// them is a guess. All four are display-only context produced by the
				// execution scope, never by the model's args.
				"environment":     req.Environment,
				"cwd":             req.Cwd,
				"change_summary":  req.ChangeSummary,
				"triage_state":    req.TriageState,
				"decision_policy": req.DecisionPolicy,
				// The judge's reasoning and its two assessment axes, so the person
				// inherits the judgement instead of redoing it, and a later reader
				// can audit why the ask happened at all.
				"triage_rationale":     req.TriageRationale,
				"triage_risk":          req.TriageRisk,
				"triage_authorization": req.TriageAuthorization,
				// The authoritative answer set for this ask (batch B1). Every
				// surface renders THIS list instead of inventing one.
				"decisions": decisions,
			}),
		})
		if err != nil {
			return tools.ToolApprovalDecision{}, err
		}
		if taskID != "" {
			_, _ = store.AppendEvent(waitCtx, control.Event{
				TaskID:     taskID,
				RunID:      runID,
				Type:       "approval.requested",
				Visibility: "task",
				Channel:    fallback(channel, identity.Platform),
				Payload: mustJSON(map[string]interface{}{
					"approval_id": approval.ID,
					"action_type": approval.ActionType,
					"tool":        req.ToolName,
					"reason":      req.Reason,
					// Compact single-string object of the action (path/command)
					// so one-line UI surfaces (the TUI approval panel) can show
					// "tool → target" without decoding full args.
					"target": approvalActionTarget(req.ToolName, req.Args),
					"args":   persistentArgs,
					// Decision context for the panel: where it runs, how big the
					// change is, what a "remember this" would authorize, and
					// whether automatic triage was even able to rule. The TUI
					// builds its panel from this event, so anything absent here
					// is invisible at decision time.
					"environment":      req.Environment,
					"cwd":              req.Cwd,
					"change_summary":   req.ChangeSummary,
					"grant_class":      req.GrantClass,
					"run_grant_class":  req.RunGrantClass,
					"containment":      req.Containment,
					"triage_state":     req.TriageState,
					"decision_policy":  req.DecisionPolicy,
					"triage_rationale": req.TriageRationale,
					"triage_risk":      req.TriageRisk,
					"decisions":        decisions,
				}),
			})
		}
		resumeWatchdog := runpool.BeginPersonWait(ctx, runpool.PhaseWaitingApproval)
		defer resumeWatchdog()
		c.notifyApprovalRequested(context.Background(), identity, taskID, runID, fallback(channel, identity.Platform), approval)

		// A resource deadline parks the request instead of invalidating it. An
		// explicit run cancellation still expires it: /stop means stop, while an
		// unattended timeout means "answer later and resume".
		park := func() (tools.ToolApprovalDecision, error) {
			cause := "waiter gone"
			if err := waitCtx.Err(); err != nil {
				cause += ": " + err.Error()
			}
			cleanupCtx := context.WithoutCancel(waitCtx)
			explicitCancel := ctx.Err() != nil && errors.Is(context.Cause(ctx), context.Canceled)
			var current *control.ApprovalRequest
			var transitioned bool
			var err error
			if explicitCancel {
				current, transitioned, err = store.ExpireApprovalRequest(cleanupCtx, identity.TenantID, approval.ID, cause)
			} else {
				current, transitioned, err = store.ParkApprovalRequest(cleanupCtx, identity.TenantID, approval.ID, cause)
			}
			if err != nil {
				return tools.ToolApprovalDecision{ApprovalID: approval.ID}, err
			}
			if current == nil {
				return tools.ToolApprovalDecision{ApprovalID: approval.ID}, fmt.Errorf("approval request disappeared: %s", approval.ID)
			}
			if transitioned {
				if explicitCancel {
					if _, eventErr := store.AppendApprovalResolutionEvent(cleanupCtx, current, fallback(channel, identity.Platform), cause); eventErr != nil {
						log.Warn("failed to append approval.expired event", "approval_id", current.ID, "error", eventErr)
					}
					c.srv.notifyApprovalResolutionElsewhere(cleanupCtx, identity, current, fallback(channel, identity.Platform))
				} else if _, eventErr := store.AppendApprovalParkedEvent(cleanupCtx, current, fallback(channel, identity.Platform), cause); eventErr != nil {
					log.Warn("failed to append approval.parked event", "approval_id", current.ID, "error", eventErr)
				}
			}
			// A response can win exactly as the wait deadline fires. When the
			// pending -> expired update changed no row, honor the stored human
			// decision instead of fabricating a timeout event over it.
			if decision, terminal, claimErr := claimStoredApprovalDecision(cleanupCtx, store, identity.TenantID, runID, current); terminal || claimErr != nil {
				return decision, claimErr
			}
			if current.Status == "pending" && current.WaiterState == "parked" {
				return tools.ToolApprovalDecision{
					ApprovalID: approval.ID, Outcome: tools.ApprovalOutcomeTimedOut,
					Reason: "approval parked; the request remains answerable and a later decision will resume the task",
				}, nil
			}
			return tools.ToolApprovalDecision{ApprovalID: approval.ID}, fmt.Errorf("approval request %s remained %s/%s after waiter cleanup", approval.ID, current.Status, current.WaiterState)
		}

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				return park()
			case <-ticker.C:
				// Both branches can be ready on the same tick, and select then
				// picks at random. Polling with an already-expired waitCtx
				// returned the context error to the tool layer, which reported
				// an unanswered approval as a transport failure roughly half
				// the time it timed out on a tick boundary.
				if waitCtx.Err() != nil {
					return park()
				}
				// The poll must NOT inherit the wait deadline. Checking
				// waitCtx above still leaves a window where the budget lapses
				// between the check and the read, and the store then returns
				// the context error — reporting an unanswered approval as a
				// transport failure. Loop exit is owned by the Done branch.
				pollCtx, cancelPoll := context.WithTimeout(context.WithoutCancel(waitCtx), approvalPollTimeout)
				current, err := store.GetApprovalRequest(pollCtx, identity.TenantID, approval.ID)
				cancelPoll()
				if err != nil {
					return tools.ToolApprovalDecision{ApprovalID: approval.ID}, err
				}
				if current == nil {
					return tools.ToolApprovalDecision{ApprovalID: approval.ID}, fmt.Errorf("approval request disappeared: %s", approval.ID)
				}
				claimCtx, cancelClaim := context.WithTimeout(context.WithoutCancel(waitCtx), approvalPollTimeout)
				decision, terminal, claimErr := claimStoredApprovalDecision(claimCtx, store, identity.TenantID, runID, current)
				cancelClaim()
				if terminal || claimErr != nil {
					return decision, claimErr
				}
			}
		}
	}
}

// notifyApprovalRequested pushes an approval notification along the
// conversation-layer routing rules (docs/identity-continuity.md "Runtime
// attachment model"): IM-originated approvals notify their own channel
// (origin affinity); CLI-originated approvals are suppressed while a CLI
// endpoint is attached (the TUI already shows the inline y/N prompt — pushing
// to IM too was the double-notification bug) and otherwise go to the SINGLE
// preferred IM endpoint, never a fan-out to every bound account. Failures are
// swallowed: notification is a convenience, the approval row is the truth.
func (c *RunCoordinator) notifyApprovalRequested(ctx context.Context, identity *control.IdentityContext, taskID, runID, channel string, approval *control.ApprovalRequest) {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || identity == nil || approval == nil {
		return
	}
	taskTitle := ""
	if taskID != "" && c.srv.Control != nil {
		if task, err := c.srv.Control.GetTask(ctx, identity.TenantID, taskID); err == nil && task != nil {
			taskTitle = task.Title
		}
	}
	content := approvalNotificationText(*approval, taskTitle)
	base := delivery.Message{
		TenantID:   identity.TenantID,
		PersonID:   identity.PersonID,
		TaskID:     taskID,
		RunID:      runID,
		Content:    content,
		Kind:       delivery.KindApproval,
		ApprovalID: approval.ID,
	}
	// Track "notified" honestly: stamp notified_at only after confirmed delivery.
	// A suppressed CLI-attached, pending-session, or unconfirmed push leaves it
	// NULL; escrow can create a missing attempt, while the delivery layer alone
	// owns retry/catch-up for an existing durable row.
	delivered := false
	if identity.Platform == "cli" && c.approvalSurface(ctx, identity) == "phone-first" {
		delivered = c.deliverToPreferredIM(ctx, identity, base)
	} else {
		delivered = c.routePendingNotification(ctx, identity, channel, base)
	}
	if delivered && c.srv.Control != nil {
		_ = c.srv.Control.MarkApprovalNotified(ctx, identity.TenantID, approval.ID)
	}
}

func (c *RunCoordinator) approvalSurface(ctx context.Context, identity *control.IdentityContext) string {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil {
		return "desk-first"
	}
	value, err := c.srv.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalSurface)
	if err == nil && strings.EqualFold(strings.TrimSpace(value), "phone-first") {
		return "phone-first"
	}
	return "desk-first"
}

// notifyClarifyRequested pushes a pending-question notification along the SAME
// conversation-layer routing rules as approvals (docs/identity-continuity.md
// "Runtime attachment model"): IM-originated questions notify their own channel;
// CLI-originated questions are suppressed while a CLI endpoint is attached (the
// live TUI already shows the clarify.requested event) and otherwise go to the
// SINGLE preferred IM endpoint. Failures are swallowed: the clarify row is the
// truth; the push is a convenience. A question survives the endpoint that raised
// it closing exactly like an approval does.
func (c *RunCoordinator) notifyClarifyRequested(ctx context.Context, identity *control.IdentityContext, taskID, runID, channel string, clarify *control.ClarifyRequest) {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || identity == nil || clarify == nil {
		return
	}
	base := delivery.Message{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		TaskID:   taskID,
		RunID:    runID,
		Content:  clarifyNotificationText(*clarify),
		Kind:     delivery.KindClarify,
	}
	if c.routePendingNotification(ctx, identity, channel, base) && c.srv.Control != nil {
		_ = c.srv.Control.MarkClarifyNotified(ctx, identity.TenantID, clarify.ID)
	}
}

// routePendingNotification is the shared presence-aware, single-preferred-
// endpoint delivery used by BOTH approval and clarify pushes. Origin is CLI when
// the PLATFORM is cli — the channel is a session UUID for TUI turns and the
// literal "cli" only for `selfmind send`, so matching on channel would route
// TUI-originated pushes to a nonexistent "cli" sender. IM origin → the
// requesting channel only (no cross-channel duplication). CLI origin with a CLI
// endpoint attached → suppressed (the live TUI already renders the inline
// prompt/event; an IM push would be a duplicate). CLI origin detached → the one
// preferred IM endpoint, never a fan-out.
// The bool return reports confirmed delivery (so the caller can stamp
// notified_at); a suppressed CLI-attached, pending-session, or unconfirmed push
// returns false.
func (c *RunCoordinator) routePendingNotification(ctx context.Context, identity *control.IdentityContext, channel string, base delivery.Message) bool {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || identity == nil {
		return false
	}
	if identity.Platform != "cli" {
		msg := base
		msg.Platform = identity.Platform
		msg.PlatformUserID = identity.PlatformUserID
		msg.Channel = channel
		confirmed, _ := c.srv.Delivery.EnqueueAndTryConfirmed(ctx, msg)
		return confirmed
	}
	if c.srv.presenceTracker().IsAttached(identity.PersonID, "cli") {
		return false
	}
	return c.deliverToPreferredIM(ctx, identity, base)
}

// escrowApprovalNotification re-pushes a pending approval that was never
// notified (initial CLI push suppressed, or the send failed) once the person is
// detached OR it has remained unanswered past the escalation threshold. It
// routes to the SINGLE preferred IM
// endpoint and stamps notified_at only after confirmed delivery. A missing
// durable attempt can therefore be created by a later sweep; an existing
// pending/retry/unconfirmed row remains owned by the delivery layer's bounded
// recovery policy, and a resolved approval is never marked.
func (c *RunCoordinator) escrowApprovalNotification(ctx context.Context, approval *control.ApprovalRequest, threshold time.Duration) {
	if c == nil || c.srv == nil || c.srv.Control == nil || approval == nil {
		return
	}
	identity := &control.IdentityContext{TenantID: approval.TenantID, PersonID: approval.PersonID}
	// A live TUI suppresses only young requests. Request age is the escalation
	// clock; keyboard activity is deliberately irrelevant. phone-first already
	// chose the mobile surface, so a missing confirmed attempt must not acquire
	// desk-first's T1 delay. Delivery idempotency still prevents this sweep from
	// blindly replaying sent_unconfirmed/pending-session rows.
	if c.srv.presenceTracker().IsAttached(approval.PersonID, "cli") &&
		time.Since(approval.CreatedAt) < threshold && c.approvalSurface(ctx, identity) != "phone-first" {
		return
	}
	taskTitle := ""
	if approval.TaskID != "" {
		if task, err := c.srv.Control.GetTask(ctx, identity.TenantID, approval.TaskID); err == nil && task != nil {
			taskTitle = task.Title
		}
	}
	base := delivery.Message{
		TenantID:   identity.TenantID,
		PersonID:   identity.PersonID,
		TaskID:     approval.TaskID,
		RunID:      approval.RunID,
		Content:    approvalNotificationText(*approval, taskTitle),
		Kind:       delivery.KindApproval,
		ApprovalID: approval.ID,
	}
	if c.deliverToPreferredIM(ctx, identity, base) {
		_ = c.srv.Control.MarkApprovalNotified(ctx, identity.TenantID, approval.ID)
	}
}

// escrowClarifyNotification is the clarify twin of escrowApprovalNotification.
func (c *RunCoordinator) escrowClarifyNotification(ctx context.Context, clarify *control.ClarifyRequest, threshold time.Duration) {
	if c == nil || c.srv == nil || c.srv.Control == nil || clarify == nil {
		return
	}
	if c.srv.presenceTracker().IsAttached(clarify.PersonID, "cli") && time.Since(clarify.CreatedAt) < threshold {
		return
	}
	identity := &control.IdentityContext{TenantID: clarify.TenantID, PersonID: clarify.PersonID}
	base := delivery.Message{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		TaskID:   clarify.TaskID,
		RunID:    clarify.RunID,
		Content:  clarifyNotificationText(*clarify),
		Kind:     delivery.KindClarify,
	}
	if c.deliverToPreferredIM(ctx, identity, base) {
		_ = c.srv.Control.MarkClarifyNotified(ctx, identity.TenantID, clarify.ID)
	}
}

// preferredIMAccount picks the SINGLE IM endpoint for CLI-origin pushes when
// the CLI is detached (conversation-layer rule 4). Selection order:
//  1. The person's explicit /notify preference, when it still resolves to one
//     of their OWN bound, delivery-capable accounts (a stale preference —
//     unbound platform, sender removed — silently falls through to auto
//     rather than dropping the notification).
//  2. Auto: the most recently seen bound IM account the delivery service can
//     reach (accounts.last_seen_at, refreshed by inbound messages and
//     presence beats).
func (c *RunCoordinator) preferredIMAccount(ctx context.Context, identity *control.IdentityContext) *control.Account {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || c.srv.Control == nil || identity == nil {
		return nil
	}
	store := c.srv.Control
	supports := c.srv.Delivery.SupportsPlatform
	pref, _ := store.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingNotifyPlatform)
	if pref == "off" {
		return nil
	}
	if pref != "" && pref != "auto" {
		account, err := store.MostRecentIMAccount(ctx, identity.TenantID, identity.PersonID, func(platform string) bool {
			return platform == pref && supports(platform)
		})
		if err == nil && account != nil {
			return account
		}
	}
	account, err := store.MostRecentIMAccount(ctx, identity.TenantID, identity.PersonID, supports)
	if err != nil {
		return nil
	}
	return account
}

// deliverToPreferredIM delivers a CLI-origin message to exactly one endpoint —
// the preferred IM account — never a broadcast to every bound account
// (docs/identity-continuity.md: "One target, never a fan-out"). Shared by
// approval notifications and CLI-originated async/detached results.
// The bool return reports confirmed delivery, not merely durable enqueue, so
// escrow/initial callers never stamp notified_at for pending_session or
// sent_unconfirmed. Those states recover only through the delivery layer's
// bounded session-aware path.
func (c *RunCoordinator) deliverToPreferredIM(ctx context.Context, identity *control.IdentityContext, base delivery.Message) bool {
	account := c.preferredIMAccount(ctx, identity)
	if account == nil {
		return false
	}
	msg := base
	msg.Platform = account.Platform
	msg.PlatformUserID = account.PlatformUserID
	// Without a live chat context the platform user id is the DM target;
	// senders fall back to it when Channel is not a real chat id.
	msg.Channel = account.PlatformUserID
	confirmed, _ := c.srv.Delivery.EnqueueAndTryConfirmed(ctx, msg)
	return confirmed
}

// deliverToPreferredIMAccepted is the durable-outbox variant used by effect
// finalization. Platform confirmation may arrive later; an inserted outbound
// row is sufficient to make crash recovery replay-safe.
func (c *RunCoordinator) deliverToPreferredIMAccepted(ctx context.Context, identity *control.IdentityContext, base delivery.Message) bool {
	account := c.preferredIMAccount(ctx, identity)
	if account == nil {
		return false
	}
	msg := base
	msg.Platform = account.Platform
	msg.PlatformUserID = account.PlatformUserID
	msg.Channel = account.PlatformUserID
	accepted, _ := c.srv.Delivery.EnqueueAndTryAccepted(ctx, msg)
	return accepted
}

func (c *RunCoordinator) withGatewayContext(input string, identity *control.IdentityContext, task *control.Task, workspace *control.Workspace, executionRoots []executionenv.RootBinding, attachments []api.MessageAttachment) string {
	var evolutionAdvice *control.EvolutionAdvice
	if c != nil && c.srv != nil && c.srv.Control != nil && c.srv.SelfEvolution.Enabled && identity != nil && task != nil {
		lookupCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		evolutionAdvice, _ = c.srv.Control.EnabledEvolutionAdvice(lookupCtx, identity.TenantID, identity.PersonID, task.ID)
		cancel()
	}
	if (workspace == nil || workspace.LocalPath == "" || task == nil) && len(executionRoots) == 0 && len(attachments) == 0 && evolutionAdvice == nil {
		return input
	}
	var sb strings.Builder
	sb.WriteString("[SelfMind daemon context]\n")
	if identity != nil {
		fmt.Fprintf(&sb, "person_id: %s\n", identity.PersonID)
		fmt.Fprintf(&sb, "platform: %s\n", identity.Platform)
		fmt.Fprintf(&sb, "platform_user_id: %s\n", identity.PlatformUserID)
	}
	if task != nil {
		fmt.Fprintf(&sb, "task_id: %s\n", task.ID)
	}
	if evolutionAdvice != nil && evolutionAdvice.Kind == "batch_read" {
		fmt.Fprintf(&sb, "evolution_candidate_id: %s\n", evolutionAdvice.CandidateID)
		sb.WriteString("This task has a read-only batching recipe backed by a verified candidate-versus-baseline comparison. When several independent local file reads/searches/listings are needed, prefer batch_read with that candidate_id. On any partial failure, follow fallback_required and use ordinary tools. Never batch writes, shell commands, credentials, or network actions.\n")
	}
	if workspace != nil && workspace.LocalPath != "" {
		fmt.Fprintf(&sb, "workspace_id: %s\n", workspace.ID)
		fmt.Fprintf(&sb, "workspace_root: %s\n", workspace.LocalPath)
		sb.WriteString("workspace_root is authoritative for this turn. Ignore remembered or historical workspace paths unless the user explicitly names one.\n")
		sb.WriteString("Use workspace_root as the default cwd for local file tools.\n")
		sb.WriteString("When the user says current project, this repo, this codebase, or names a project without an explicit path, inspect workspace_root first.\n")
		sb.WriteString("Resolve relative paths against workspace_root. Do not access files outside workspace allowed roots.\n")
	}
	additional := make([]string, 0, len(executionRoots))
	for _, binding := range executionRoots {
		if binding.Role == executionenv.RootRolePrimary || binding.Role == executionenv.RootRoleAttachment || strings.TrimSpace(binding.Path) == "" {
			continue
		}
		additional = append(additional, binding.Path)
	}
	if len(additional) > 0 {
		sb.WriteString("additional_roots:\n")
		for _, root := range additional {
			fmt.Fprintf(&sb, "- %s\n", root)
		}
		sb.WriteString("These roots are explicitly bound to this run. Use an absolute path or set cwd to the selected root; relative paths still resolve against workspace_root.\n")
	}
	if len(attachments) > 0 {
		sb.WriteString("attachments:\n")
		for i, att := range attachments {
			fmt.Fprintf(&sb, "- index: %d\n", i+1)
			if att.Kind != "" {
				fmt.Fprintf(&sb, "  kind: %s\n", att.Kind)
			}
			if att.Path != "" {
				fmt.Fprintf(&sb, "  path: %s\n", att.Path)
			}
			if att.MimeType != "" {
				fmt.Fprintf(&sb, "  mime_type: %s\n", att.MimeType)
			}
			if att.Name != "" {
				fmt.Fprintf(&sb, "  name: %s\n", att.Name)
			}
			if att.Size > 0 {
				fmt.Fprintf(&sb, "  size: %d\n", att.Size)
			}
		}
		sb.WriteString("When useful, inspect attachment paths with local tools before answering.\n")
	}
	sb.WriteString("[/SelfMind daemon context]\n\n")
	sb.WriteString(input)
	return sb.String()
}

// approvalDecisionExpiry is the control plane's ruling on how long a remembered
// decision authorizes work. Task scope is bounded by the task itself and needs no
// deadline; person scope outlives every task, so it is time-bounded — the same
// 8h window the approval-grant ledger uses, so the two cannot disagree.
func approvalDecisionExpiry(scope string) time.Time {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "person":
		return time.Now().Add(8 * time.Hour)
	case "task":
		return time.Now().Add(time.Hour)
	default:
		return time.Time{}
	}
}

// storedApprovalDecision converts one durable terminal row into the execution
// layer's typed outcome. It is shared by normal polling and the deadline-race
// path so an approval that wins concurrently with expiration is honored using
// exactly the same scope, rule, and refusal semantics.
func storedApprovalDecision(current *control.ApprovalRequest) (tools.ToolApprovalDecision, bool) {
	if current == nil {
		return tools.ToolApprovalDecision{}, false
	}
	switch current.Status {
	case "approved":
		return tools.ToolApprovalDecision{
			Approved:   true,
			ApprovalID: current.ID,
			Scope:      current.DecisionScope,
			Outcome:    tools.ApprovalOutcomeApproved,
			GrantKey:   current.DecisionGrantKey,
			ExpiresAt:  approvalDecisionExpiry(current.DecisionScope),
		}, true
	case "rejected":
		return tools.ToolApprovalDecision{
			Approved:   false,
			ApprovalID: current.ID,
			Outcome:    tools.ApprovalOutcomeDenied,
			Reason:     fallbackApprovalReason(current.DecisionNote, "rejected"),
		}, true
	case "expired":
		return tools.ToolApprovalDecision{
			Approved:   false,
			ApprovalID: current.ID,
			// Nobody refused this action. Keeping timeout distinct from deny
			// tells the model to park instead of retrying a variant.
			Outcome: tools.ApprovalOutcomeTimedOut,
			Reason:  "approval expired; do not request another approval or retry a variant; finish waiting_user",
		}, true
	default:
		return tools.ToolApprovalDecision{ApprovalID: current.ID}, false
	}
}

// claimStoredApprovalDecision closes the crash window between a durable human
// response and the live middleware consuming it. Only the original live waiter
// may claim; parked decisions are consumed by resume authorization instead.
func claimStoredApprovalDecision(ctx context.Context, store *control.Store, tenantID, runID string, current *control.ApprovalRequest) (tools.ToolApprovalDecision, bool, error) {
	decision, terminal := storedApprovalDecision(current)
	if !terminal || current == nil || (current.Status != "approved" && current.Status != "rejected") {
		return decision, terminal, nil
	}
	if current.WaiterState == "claimed" {
		if current.ClaimedByRunID == "" || current.ClaimedByRunID == runID {
			return decision, true, nil
		}
		return tools.ToolApprovalDecision{ApprovalID: current.ID}, true,
			fmt.Errorf("approval decision %s was already claimed by run %s", current.ID, current.ClaimedByRunID)
	}
	if current.WaiterState != "live" {
		return tools.ToolApprovalDecision{ApprovalID: current.ID}, true,
			fmt.Errorf("approval decision %s has no live waiter (state %s)", current.ID, current.WaiterState)
	}
	claimed, transitioned, err := store.ClaimApprovalDecision(ctx, tenantID, current.ID, runID)
	if err != nil {
		return tools.ToolApprovalDecision{ApprovalID: current.ID}, true, err
	}
	if !transitioned {
		if claimed != nil && claimed.WaiterState == "claimed" && (claimed.ClaimedByRunID == "" || claimed.ClaimedByRunID == runID) {
			resolved, _ := storedApprovalDecision(claimed)
			return resolved, true, nil
		}
		return tools.ToolApprovalDecision{ApprovalID: current.ID}, true,
			fmt.Errorf("approval decision %s could not be claimed", current.ID)
	}
	resolved, _ := storedApprovalDecision(claimed)
	return resolved, true, nil
}
