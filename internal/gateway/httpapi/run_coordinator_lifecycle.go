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
		// A caller cancellation or caller deadline is terminal: request/eval turn
		// budgets deliberately bound the daemon-owned run. Provider-internal
		// timeouts, EOF, and rate-limit exhaustion leave ctx live and remain
		// resumable interruptions because durable evidence may already exist.
	} else if ctx.Err() != nil || errors.Is(runErr, context.Canceled) {
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

// resolveTask decides which task label this turn runs under (Work Timeline P3,
// docs/work-timeline.md "Labels"/"Ingress"). Explicit evidence is
// deterministic and wins: a caller-supplied task id, an IntentContinue
// classification (router cue or short acceptance), or the one-shot /resume
// pin. Everything else gets a harmless PRE-LABEL guess: the person's current
// task when it is OPEN (non-terminal, non-archived), else a fresh placeholder
// label. The guess is safe because context is spine-based (P1) and recall
// (P2) — labels never gate what the model sees — and the post-run labeler
// re-points a wrong guess; a mislabel is a display bug, not context
// corruption. task_runs.task_id stays NOT NULL and the control plane
// (queue/approvals/busy/steer) is untouched.
func (c *RunCoordinator) resolveTask(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) (*control.Task, taskAttach, error) {
	store := c.srv.Control
	if req.TaskID != "" {
		task, err := store.GetTask(ctx, identity.TenantID, req.TaskID)
		if err != nil || task != nil {
			task, err = c.bindTaskWorkspaceIfMissing(ctx, identity, task, req, err)
			return task, taskAttach{}, err
		}
	}
	// The /resume pin covers exactly the NEXT agent-bound message, so it is
	// consumed here no matter which branch wins below — a stale pin must never
	// capture unrelated work later.
	pinned := c.srv.consumeResumePin(ctx, identity)
	if intent.Intent == router.IntentContinue {
		task, err := c.srv.resolveContinueTask(ctx, identity)
		if err != nil || task != nil {
			task, err = c.bindTaskWorkspaceIfMissing(ctx, identity, task, req, err)
			return task, taskAttach{}, err
		}
		return nil, taskAttach{}, fmt.Errorf("no task to continue; start a new task or use /resume <task_id>")
	}
	// An explicit /resume is a deliberate act, so it may reopen even an
	// ARCHIVED label; the other terminal statuses (done/cancelled/failed) stay
	// closed to the pin exactly as before.
	if pinned != nil && (!terminalTaskStatus(pinned.Status) || archivedTaskStatus(pinned.Status)) {
		task, err := c.bindTaskWorkspaceIfMissing(ctx, identity, pinned, req, nil)
		return task, taskAttach{}, err
	}
	// Pre-label guess: reuse the person's current OPEN label. Display-only —
	// the workspace still follows the request (workspaceForTask) and the
	// post-run labeler corrects a wrong guess.
	if current, err := store.CurrentTask(ctx, identity.TenantID, identity.PersonID); err == nil &&
		current != nil && current.IsVisible() && !current.IsInbox() && !terminalTaskStatus(current.Status) {
		return current, taskAttach{preLabel: true}, nil
	}
	// No open label → new placeholder. An explicit request workspace wins;
	// otherwise the person's current workspace seeds the task scope.
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
	return task, taskAttach{created: true, preLabel: true}, err
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
	// For EXPLICIT attaches (task id, continuation cue, /resume pin) the task's
	// own workspace binding is authoritative and must survive a resume/continue:
	// a CLI `继续` of an IM task must run in the dir the prior run built in, not
	// the terminal's cwd-derived workspace (prepareRequestWorkspace sets
	// req.WorkspaceID from ClientCWD for local CLI turns) — otherwise every file
	// op trips out-of-root approvals against the wrong tree.
	//
	// For a PRE-LABEL guess the label is display-only, so the REQUEST wins: the
	// run executes in the explicitly requested (or cwd-derived) workspace even
	// when the guessed label is bound elsewhere. This is what makes a wrong
	// pre-label harmless — execution scope never inherits a guessed label's
	// workspace (Work Timeline P3).
	workspaceID := ""
	if attach.preLabel {
		workspaceID = strings.TrimSpace(req.WorkspaceID)
	}
	if workspaceID == "" && task != nil {
		workspaceID = task.WorkspaceID
	}
	if workspaceID == "" {
		workspaceID = req.WorkspaceID
	}
	if workspaceID == "" {
		return store.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID)
	}
	return store.GetWorkspace(ctx, identity.TenantID, workspaceID)
}

func (c *RunCoordinator) installExecutionScope(identity *control.IdentityContext, task *control.Task, run *control.Run, workspace *control.Workspace, req api.MessageRequest, leases ...*executionenv.Lease) func() {
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
	if len(leases) > 0 && leases[0] != nil {
		scope.LeaseID = leases[0].ID
		scope.EnvironmentSnapshotID = leases[0].EnvironmentSnapshotID
		scope.EnvironmentGeneration = leases[0].EnvironmentGeneration
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
	scope.Clarify = c.gatewayClarify(identity, task, run, scope.Channel)
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
	triageIntent := triageIntentFromRequest(req.Content)
	scope.TriageIntent = func() string { return triageIntent }
	// Grants back class-level approval memory (session/persistent allowlist);
	// the control store satisfies tools.ApprovalGrantStore structurally.
	if c.srv != nil && c.srv.Control != nil {
		scope.Grants = c.srv.Control
		scope.CapabilityStore = c.srv.Control
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
func (c *RunCoordinator) gatewayClarify(identity *control.IdentityContext, task *control.Task, run *control.Run, channel string) tools.ClarifyHandler {
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
		// The run ctx is not available here (the clarify handler signature carries
		// no ctx), so the wait is bounded purely by clarifyWaitTimeout; the
		// orphan sweep (ExpireOrphanedClarifies) is the backstop if the daemon is
		// killed mid-wait.
		waitCtx, cancel := context.WithTimeout(context.Background(), clarifyWaitTimeout)
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
		c.notifyClarifyRequested(context.Background(), identity, taskID, runID, reqChannel, clarify)

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				// The waiter is gone (timeout): a row left 'pending' would keep
				// intercepting the next free-text message as an answer to a
				// question nobody is waiting on. Expire it; the orphan sweep is
				// the backstop for waiters that die without reaching this line.
				_ = store.ExpireClarifyRequest(context.WithoutCancel(waitCtx), identity.TenantID, clarify.ID, "waiter gone: "+waitCtx.Err().Error())
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

func (c *RunCoordinator) toolApprovalHandler(identity *control.IdentityContext, task *control.Task, run *control.Run, channel string) tools.ToolApprovalHandler {
	return func(ctx context.Context, req tools.ToolApprovalRequest) (tools.ToolApprovalDecision, error) {
		if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil {
			return tools.ToolApprovalDecision{}, fmt.Errorf("approval store is not available")
		}
		store := c.srv.Control
		if ctx == nil {
			ctx = context.Background()
		}
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()

		taskID := req.TaskID
		runID := req.RunID
		if taskID == "" && task != nil {
			taskID = task.ID
		}
		if runID == "" && run != nil {
			runID = run.ID
		}
		decisions := buildApprovalDecisions(req)
		approval, err := store.CreateApprovalRequest(waitCtx, control.ApprovalRequest{
			TenantID:         identity.TenantID,
			PersonID:         identity.PersonID,
			TaskID:           taskID,
			RunID:            runID,
			ActionType:       "tool_call",
			RequestedChannel: fallback(channel, identity.Platform),
			Payload: mustJSON(map[string]interface{}{
				"tool":        req.ToolName,
				"reason":      req.Reason,
				"args":        redactApprovalArgs(req.Args),
				"grant_class": req.GrantClass,
				// WHERE it runs and HOW BIG the change is: a decision made without
				// them is a guess. All four are display-only context produced by the
				// execution scope, never by the model's args.
				"environment":    req.Environment,
				"cwd":            req.Cwd,
				"change_summary": req.ChangeSummary,
				"triage_state":   req.TriageState,
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
					"target": approvalActionTarget(req.Args),
					// Decision context for the panel: where it runs, how big the
					// change is, what a "remember this" would authorize, and
					// whether automatic triage was even able to rule. The TUI
					// builds its panel from this event, so anything absent here
					// is invisible at decision time.
					"environment":      req.Environment,
					"cwd":              req.Cwd,
					"change_summary":   req.ChangeSummary,
					"grant_class":      req.GrantClass,
					"triage_state":     req.TriageState,
					"triage_rationale": req.TriageRationale,
					"triage_risk":      req.TriageRisk,
					"decisions":        decisions,
				}),
			})
		}
		c.notifyApprovalRequested(context.Background(), identity, taskID, runID, fallback(channel, identity.Platform), approval)

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				// The waiter is gone (timeout / run cancelled): a row left
				// 'pending' would pollute every later list — bare y turns
				// ambiguous and /approve 1 hits a dead request (observed
				// live). Expire it; the recovery sweep is the backstop for
				// waiters that die without reaching this line.
				_ = store.ExpireApprovalRequest(context.WithoutCancel(waitCtx), identity.TenantID, approval.ID, "waiter gone: "+waitCtx.Err().Error())
				return tools.ToolApprovalDecision{
					Approved:   false,
					ApprovalID: approval.ID,
					// Typed as a TIMEOUT, not a rejection (batch B2): nobody
					// refused this, nobody answered. The execution layer renders a
					// different sentence for each so the model parks the work
					// instead of trying a variant of a "rejected" action.
					Outcome: tools.ApprovalOutcomeTimedOut,
					Reason:  "approval expired; do not request another approval or retry a variant; finish waiting_user",
				}, nil
			case <-ticker.C:
				current, err := store.GetApprovalRequest(waitCtx, identity.TenantID, approval.ID)
				if err != nil {
					return tools.ToolApprovalDecision{ApprovalID: approval.ID}, err
				}
				if current == nil {
					return tools.ToolApprovalDecision{ApprovalID: approval.ID}, fmt.Errorf("approval request disappeared: %s", approval.ID)
				}
				switch current.Status {
				case "approved":
					// DecisionScope (set when the human answered with a grant
					// scope) tells the middleware whether to remember this class
					// for the task/person.
					return tools.ToolApprovalDecision{
						Approved:   true,
						ApprovalID: approval.ID,
						Scope:      current.DecisionScope,
						Outcome:    tools.ApprovalOutcomeApproved,
						// The rule the human picked, when they picked one. The
						// execution layer validates it against the candidates this
						// call offered before storing anything.
						GrantKey: current.DecisionGrantKey,
						// The control plane rules on how long its authorization
						// lasts; the execution layer only redeems it.
						ExpiresAt: approvalDecisionExpiry(current.DecisionScope),
					}, nil
				case "rejected":
					return tools.ToolApprovalDecision{
						Approved:   false,
						ApprovalID: approval.ID,
						Outcome:    tools.ApprovalOutcomeDenied,
						Reason:     fallbackApprovalReason(current.DecisionNote, "rejected"),
					}, nil
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
	// Track "notified" honestly: stamp notified_at only when a push actually went
	// out (IM origin, or CLI detached). A suppressed CLI-attached push leaves it
	// NULL so the escrow sweep can re-push after the CLI leaves (Fix 2).
	if c.routePendingNotification(ctx, identity, channel, base) && c.srv.Control != nil {
		_ = c.srv.Control.MarkApprovalNotified(ctx, identity.TenantID, approval.ID)
	}
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
// The bool return reports whether a message was actually enqueued for delivery
// (so the caller can stamp notified_at); a suppressed CLI-attached push returns
// false.
func (c *RunCoordinator) routePendingNotification(ctx context.Context, identity *control.IdentityContext, channel string, base delivery.Message) bool {
	if c == nil || c.srv == nil || c.srv.Delivery == nil || identity == nil {
		return false
	}
	if identity.Platform != "cli" {
		msg := base
		msg.Platform = identity.Platform
		msg.PlatformUserID = identity.PlatformUserID
		msg.Channel = channel
		return c.srv.Delivery.EnqueueAndTry(ctx, msg) == nil
	}
	if c.srv.presenceTracker().IsAttached(identity.PersonID, "cli") {
		return false
	}
	return c.deliverToPreferredIM(ctx, identity, base)
}

// escrowApprovalNotification re-pushes a pending approval that was never
// notified (initial CLI push suppressed, or the send failed) once the person is
// no longer attached on CLI (Fix 2). It routes to the SINGLE preferred IM
// endpoint and stamps notified_at only after the enqueue succeeds, so a failed
// push retries on the next sweep and a resolved approval is never marked.
func (c *RunCoordinator) escrowApprovalNotification(ctx context.Context, approval *control.ApprovalRequest) {
	if c == nil || c.srv == nil || c.srv.Control == nil || approval == nil {
		return
	}
	// Person is back at the CLI: the live TUI shows the inline prompt, so leave
	// the push for a later sweep rather than duplicating to IM.
	if c.srv.presenceTracker().IsAttached(approval.PersonID, "cli") {
		return
	}
	identity := &control.IdentityContext{TenantID: approval.TenantID, PersonID: approval.PersonID}
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
func (c *RunCoordinator) escrowClarifyNotification(ctx context.Context, clarify *control.ClarifyRequest) {
	if c == nil || c.srv == nil || c.srv.Control == nil || clarify == nil {
		return
	}
	if c.srv.presenceTracker().IsAttached(clarify.PersonID, "cli") {
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
// The bool return reports whether the message was enqueued (an account existed
// and EnqueueAndTry accepted it), so escrow/initial callers can stamp
// notified_at only on a real send.
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
	return c.srv.Delivery.EnqueueAndTry(ctx, msg) == nil
}

func (c *RunCoordinator) withGatewayContext(input string, identity *control.IdentityContext, task *control.Task, workspace *control.Workspace, attachments []api.MessageAttachment) string {
	if (workspace == nil || workspace.LocalPath == "" || task == nil) && len(attachments) == 0 {
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
	if workspace != nil && workspace.LocalPath != "" {
		fmt.Fprintf(&sb, "workspace_id: %s\n", workspace.ID)
		fmt.Fprintf(&sb, "workspace_root: %s\n", workspace.LocalPath)
		sb.WriteString("workspace_root is authoritative for this turn. Ignore remembered or historical workspace paths unless the user explicitly names one.\n")
		sb.WriteString("Use workspace_root as the default cwd for local file tools.\n")
		sb.WriteString("When the user says current project, this repo, this codebase, or names a project without an explicit path, inspect workspace_root first.\n")
		sb.WriteString("Resolve relative paths against workspace_root. Do not access files outside workspace allowed roots.\n")
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
