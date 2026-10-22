package eval

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	appcore "selfmind/internal/app"
	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/promptassets"
)

type RunOptions struct {
	ConfigPath     string
	TenantID       string
	Provider       string
	Model          string
	Workspace      string
	OutputPath     string
	RecordContent  bool
	ProgressWriter io.Writer
	TurnTimeout    time.Duration
}

type RunResult struct {
	CaseID          string         `json:"case_id"`
	Status          string         `json:"status"`
	OutputPath      string         `json:"output_path"`
	Duration        time.Duration  `json:"duration"`
	ToolCalls       int            `json:"tool_calls"`
	ActionToolCalls int            `json:"action_tool_calls"`
	ProgressUpdates int            `json:"progress_updates"`
	ToolErrors      int            `json:"tool_errors"`
	InputTokens     int            `json:"input_tokens"`
	OutputTokens    int            `json:"output_tokens"`
	Checks          []CheckResult  `json:"checks"`
	Error           string         `json:"error,omitempty"`
	Provenance      *RunProvenance `json:"provenance,omitempty"`
	Efficiency      *Efficiency    `json:"efficiency,omitempty"`
	Samples         []RunResult    `json:"samples,omitempty"`
}

type runtimeHarness struct {
	stopApprovalTelemetry func()
	cfg                   *config.Config
	mem                   *memory.MemoryManager
	controlStore          *control.Store
	gateway               *appcore.GatewayDeps
	server                *httpapi.Server
	tenantID              string
	provider              string
	model                 string
	// deliverySender is set only for cases that seed deliveries; it records what
	// the recovery path actually pushed.
	deliverySender *evalDeliverySender
	// leases records which worker served each Run and holds hold_worker Runs.
	leases *workerLeaseLog
	// stubDir holds the MCP stub binary and its start logs.
	stubDir string
	// choices maps each turn choice the case seeded or was shown to the person
	// it was asked of.
	choices map[string]string
}

func RunCaseFile(ctx context.Context, path string, opts RunOptions) (*RunResult, error) {
	c, err := LoadCase(path)
	if err != nil {
		return nil, err
	}
	return RunCase(ctx, c, opts)
}

func RunCase(ctx context.Context, c *Case, opts RunOptions) (*RunResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil {
		return nil, fmt.Errorf("case is nil")
	}
	if c.CaptureReviewRequired {
		return nil, fmt.Errorf("captured draft requires an isolated fixture and reviewed assertions; clear capture_review_required only after review")
	}
	repeat := c.Repeat
	if repeat < 1 {
		repeat = 1
	}
	if repeat == 1 {
		return runSingle(ctx, c, opts, 0, 1)
	}
	var samples []RunResult
	var firstErr error
	for s := 0; s < repeat; s++ {
		started := time.Now()
		r, err := runSingle(ctx, c, opts, s, repeat)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			r = &RunResult{CaseID: c.ID, Status: "error", Duration: time.Since(started), Error: err.Error()}
		}
		samples = append(samples, *r)
	}
	result := aggregateSamples(c.ID, samples, c.PassRate)
	allErrors := true
	for _, sample := range samples {
		if sample.Status != "error" {
			allErrors = false
		}
	}
	if allErrors {
		return result, firstErr
	}
	return result, nil
}

func runSingle(ctx context.Context, c *Case, opts RunOptions, sampleIdx, totalSamples int) (*RunResult, error) {
	sessionID := caseSessionID(c.ID, sampleIdx, totalSamples, llm.VCRRecordMode())
	// Eval runs are data-isolated BY DEFAULT: every case gets a throwaway temp
	// data dir (fresh control.db + memory), so record/replay sessions never
	// write eval-* persons, current_task rows, or runs into the user's real
	// ~/.selfmind data. `shared_data: true` is the explicit opt-out for a case
	// that genuinely needs pre-existing durable state (none exist today — each
	// case creates its own eval-<id> identity). Workspace isolation stays
	// scenario-driven: cases that seed or assert world state also get a scratch
	// workspace, while e.g. `workspace: "."` cases still probe the real repo.
	// VCR cassettes are keyed by case ID under a separate .vcr dir, so data-dir
	// isolation never invalidates recorded cassettes.
	var dataDirOverride string
	var cleanup func()
	workspace, err := resolveWorkspace(c, opts.Workspace)
	if err != nil {
		return nil, err
	}
	isolateWorkspace := needsWorkspaceIsolation(c)
	if !c.SharedData || isolateWorkspace {
		root, err := makeEvalTempRoot(c.ID)
		if err != nil {
			return nil, err
		}
		cleanup = func() {
			if err := cleanupEvalTempRoot(root); err != nil {
				log.Warn("eval: temporary root cleanup failed", "root", root, "error", err)
			}
		}
		dataDirOverride = filepath.Join(root, "data")
		if isolateWorkspace {
			workspace = filepath.Join(root, "workspace")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				cleanup()
				return nil, err
			}
			if c.Setup != nil {
				if err := applyFileSeeds(workspace, c.Setup.Files); err != nil {
					cleanup()
					return nil, err
				}
			}
		}
	}
	// Keep every consumer on one spelling of the same directory. On macOS,
	// /var is a symlink to /private/var; execution-root admission canonicalizes
	// that alias, while VCR otherwise expands its workspace placeholder with the
	// original spelling. The mismatch makes legitimate replayed file calls look
	// out of scope. Best-effort canonicalization preserves the prior behavior for
	// a deliberately unavailable workspace while aligning all existing roots.
	workspace = canonicalEvalWorkspace(workspace)
	if cleanup != nil {
		defer cleanup()
	}

	outPath := opts.OutputPath
	if strings.TrimSpace(outPath) == "" {
		outPath = DefaultRunPath(c, firstNonEmpty(opts.Provider, c.Provider, "default"))
	}
	if totalSamples > 1 {
		outPath = strings.TrimSuffix(outPath, ".jsonl") + fmt.Sprintf("-s%d.jsonl", sampleIdx+1)
	}
	rec, err := NewRecorder(outPath, opts.RecordContent || c.RecordContent)
	if err != nil {
		return nil, err
	}
	defer rec.Close()

	h, err := newRuntimeHarness(opts, c, dataDirOverride)
	if err != nil {
		return nil, err
	}
	defer h.Close()

	// Resolve the eval identity up front so setup can seed memory/task, then
	// reuse it for the post-run world-state assertions.
	identity, err := h.controlStore.ResolveOrCreateAccount(ctx, h.tenantID, "eval", "eval-"+c.ID, "SelfMind Eval")
	if err != nil {
		return nil, err
	}
	// The case person's other endpoint accounts (CLI sessions, IM accounts)
	// are bound before any seed or turn names them.
	accounts, err := bindPersonAccounts(ctx, h.controlStore, c, identity)
	if err != nil {
		return nil, err
	}
	// Bind the workspace explicitly whenever the data dir is fresh (the
	// default). The gateway's CLI-cwd heuristic only auto-registers a workspace
	// for cli/terminal channels, so IM-channel scenarios (weixin/feishu/qq)
	// would otherwise run with no workspace scope in an empty control.db.
	// Registering it and passing its ID on every turn binds the scope
	// regardless of channel.
	var workspaceID string
	if dataDirOverride != "" {
		if ws, werr := h.controlStore.RegisterWorkspace(ctx, control.Workspace{
			TenantID:      identity.TenantID,
			OwnerPersonID: identity.PersonID,
			Name:          "eval-workspace",
			LocalPath:     workspace,
			AllowedRoots:  []string{workspace},
		}); werr == nil && ws != nil {
			workspaceID = ws.ID
			_ = h.controlStore.SetCurrentWorkspace(ctx, identity.TenantID, identity.PersonID, ws.ID)
		}
	}
	var seededTaskID string
	var seededParkedRunIDs []string
	if c.Setup != nil {
		seeded, err := applyStateSeeds(ctx, h.controlStore, h.mem, identity, workspaceID, workspace, firstNonEmpty(c.Channel, "cli"), c.Setup, &seededParkedRunIDs)
		if err != nil {
			return nil, err
		}
		seededTaskID = seeded
		choiceIDs, err := applyEndpointSeeds(ctx, h.controlStore, c, endpointSeedContext{
			accounts: accounts, workspace: workspace, workspaceID: workspaceID, taskID: seededTaskID, parkedRuns: seededParkedRunIDs,
		})
		if err != nil {
			return nil, err
		}
		for _, id := range choiceIDs {
			h.choices[id] = identity.PersonID
		}
	}
	existingRunIDs := map[string]bool{}
	if identity != nil {
		existingRuns, err := h.controlStore.ListRecentRunsForPerson(ctx, identity.TenantID, identity.PersonID, 100)
		if err != nil {
			return nil, fmt.Errorf("snapshot seeded runs: %w", err)
		}
		for _, run := range existingRuns {
			existingRunIDs[run.RunID] = true
		}
	}

	start := time.Now()
	rec.StartCase(c, h.provider, h.model, workspace)
	provenance := newRunProvenance(c, h.cfg, h.provider, h.model)
	provenance.RecordingSession = sessionID
	rec.Write(JSONLEvent{Type: "eval.provenance", Provenance: provenance})
	var taskIDs []string
	var runIDs []string
	var createdRunIDs []string
	var inputTokens, outputTokens int
	var lastStatus string
	var lastOutcome string
	var lastCompletionReason string
	var lastResumable bool
	var lastVerificationState string
	var lastConditionState string
	var lastHTTPStatus int
	// Track every person the case touched (the case identity plus any per-turn
	// platform_user_id "stranger" identities) so post-case run finalization can
	// sweep exactly the runs this eval created and nothing else.
	seenPersons := map[string]bool{}
	if identity != nil && identity.PersonID != "" {
		seenPersons[identity.PersonID] = true
	}
	backgroundCtx, cancelBackground := context.WithCancel(evalTurnVCRContext(httpapi.WithStreamObserver(ctx, rec.ObserveStreamEvent), sessionID, workspace))
	defer func() {
		cancelBackground()
		deadline := time.Now().Add(5 * time.Second)
		for h.server.ActiveRunCount() > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}()
	h.server.BackgroundRunContext = backgroundCtx
	// VCR hygiene, once per case execution: reset the per-session call counter
	// so numbering always starts at 0000 (the counter is process-global and a
	// prior run of the same case in this process would otherwise leave a 0001+
	// hole in recordings and break replays); in record mode also wipe the
	// case's previous cassette generation so files never interleave.
	llm.ResetVCRSession(sessionID)
	if llm.VCRRecordMode() {
		if err := llm.WipeVCRSessionRecordings("", sessionID); err != nil {
			return nil, fmt.Errorf("wipe stale cassettes for %s: %w", c.ID, err)
		}
	}
	// Per-turn run ids, indexed by turn, so a later turn's reply_to_turn can
	// resolve to the exact run that earlier turn started (a turn that starts
	// no run — queued, candidates, control command — leaves its slot empty).
	turnRunIDs := make([]string, len(c.Turns))
	var turnChecks []CheckResult
	// Held turns finish after later turns. Their Tasks and Runs count as the
	// case's work, but taskIDs and runIDs, which name the subject of state
	// assertions, keep only foreground turns.
	var heldTaskIDs, heldRunIDs []string
	// noteResponse keeps the ids and usage every turn contributes.
	noteResponse := func(i int, resp api.MessageResponse, isHeld bool) {
		if resp.Identity != nil && resp.Identity.PersonID != "" {
			seenPersons[resp.Identity.PersonID] = true
		}
		if resp.Task != nil {
			if isHeld {
				heldTaskIDs = append(heldTaskIDs, resp.Task.ID)
			} else {
				taskIDs = append(taskIDs, resp.Task.ID)
			}
		}
		if resp.Run != nil {
			if isHeld {
				heldRunIDs = append(heldRunIDs, resp.Run.ID)
			} else {
				runIDs = append(runIDs, resp.Run.ID)
			}
			if !existingRunIDs[resp.Run.ID] {
				createdRunIDs = append(createdRunIDs, resp.Run.ID)
				existingRunIDs[resp.Run.ID] = true
			}
			turnRunIDs[i] = resp.Run.ID
		}
		if resp.Choice != nil && resp.Choice.ID != "" && resp.Identity != nil {
			h.choices[resp.Choice.ID] = resp.Identity.PersonID
		}
		inputTokens += resp.Usage.InputTokens
		outputTokens += resp.Usage.OutputTokens
	}
	var held []*heldTurn
	// A case that returns early must not close the store under a held Run.
	defer func() {
		h.leases.releaseHolds()
		for _, turn := range held {
			<-turn.done
		}
	}()
	for i, turn := range c.Turns {
		turnStart := time.Now()
		channel := firstNonEmpty(turn.Channel, c.Channel, "cli")
		input := turn.Input
		if turn.SeededRunOrdinal > 0 {
			input += " " + seededParkedRunIDs[turn.SeededRunOrdinal-1]
		}
		if turn.SeededEffectOrdinal > 0 {
			claims, err := h.controlStore.ExternalEffectClaimsForEffect(ctx, identity.TenantID, identity.PersonID,
				seededParkedRunIDs[turn.SeededEffectOrdinal-1], seededEffectID)
			if err != nil || len(claims) == 0 {
				return nil, fmt.Errorf("turn %d: seeded effect of parked run %d is missing: %v", i+1, turn.SeededEffectOrdinal, err)
			}
			input += " " + claims[0].ID
		}
		if turn.TurnRunOrdinal > 0 {
			runID := turnRunIDs[turn.TurnRunOrdinal-1]
			if runID == "" {
				// Sending the bare control would act on whatever is current.
				turnChecks = append(turnChecks, CheckResult{
					Name:    fmt.Sprintf("turn-%d:turn_run_ordinal", i+1),
					Message: fmt.Sprintf("turn %d started no Run, so this turn was not sent", turn.TurnRunOrdinal),
				})
				continue
			}
			input += " " + runID
		}
		rec.StartTurn(i, input, channel)
		replyToRunID := ""
		if turn.ReplyToTurn > 0 && turn.ReplyToTurn <= i {
			replyToRunID = turnRunIDs[turn.ReplyToTurn-1]
		}
		observer := rec.ObserveStreamEvent
		if opts.ProgressWriter != nil {
			observer = func(event llm.StreamEvent) {
				rec.ObserveStreamEvent(event)
				writeLiveProgress(opts.ProgressWriter, c.ID, i, event)
			}
		}
		// Tag the turn with a VCR session (the case id) so model calls can be
		// recorded/replayed deterministically when SELFMIND_EVAL_VCR is set.
		// The turn mark lets the worker lease hook name the turn of every Run.
		vcrCtx := withEvalTurn(evalTurnVCRContext(httpapi.WithStreamObserver(ctx, observer), sessionID, workspace), i+1)
		// A per-turn platform_user_id override simulates a different platform user
		// (identity-isolation scenarios); the default keeps the case's identity.
		req, err := h.turnRequest(c, turn, input, channel, replyToRunID, workspace, workspaceID)
		if err != nil {
			return nil, fmt.Errorf("turn %d: %w", i+1, err)
		}
		if turn.HoldWorker {
			// The Run stays on its worker while later turns run; its result is
			// recorded once it has been stopped. The turn budget bounds it.
			held = append(held, h.startHeldTurn(vcrCtx, turnBudget(c, opts), i, req))
			lease, _ := h.leases.turnLease(i + 1)
			turnRunIDs[i] = lease.runID
			if len(turn.AssertState) > 0 {
				world := h.worldState(ctx, identity, lease.taskID, lease.runID, workspace, turn.AssertState)
				for _, check := range EvaluateStatePredicates(turn.AssertState, world) {
					check.Name = fmt.Sprintf("turn-%d:%s", i+1, check.Name)
					turnChecks = append(turnChecks, check)
				}
			}
			continue
		}
		// Per-turn deadline: a stalled provider stream (bytes stop arriving
		// without an EOF) would otherwise hang the eval indefinitely. The
		// deadline cancels the underlying request so the turn fails cleanly.
		turnCtx, cancelTurn := context.WithTimeout(vcrCtx, turnBudget(c, opts))
		var resp api.MessageResponse
		var status int
		if turn.Scheduled {
			resp, status = h.fireScheduledTurn(turnCtx, req)
		} else {
			resp, status = h.server.ProcessMessage(turnCtx, req)
		}
		cancelTurn()
		if turn.WaitForMaintenance {
			maintenanceCtx, cancelMaintenance := context.WithTimeout(
				evalTurnVCRContext(ctx, c.ID, workspace), turnBudget(c, opts),
			)
			h.server.RunMaintenancePass(maintenanceCtx)
			cancelMaintenance()
		}
		noteResponse(i, resp, false)
		// A missing outcome is absence of this turn's evidence, not permission
		// to reuse the previous turn's status or verification verdict.
		lastOutcome, lastCompletionReason, lastResumable, lastVerificationState = "", "", false, ""
		lastConditionState = ""
		if resp.Outcome != nil {
			lastOutcome = resp.Outcome.Status
			lastCompletionReason = resp.Outcome.CompletionReason
			lastResumable = resp.Outcome.Resumable
			if resp.Outcome.Verification != nil {
				lastVerificationState = resp.Outcome.Verification.State
				lastConditionState = resp.Outcome.Verification.ConditionState
			}
		}
		lastStatus = "completed"
		if resp.Turn != nil && strings.TrimSpace(resp.Turn.Status) != "" {
			lastStatus = resp.Turn.Status
		}
		if resp.Error != "" || status >= 400 {
			lastStatus = "failed"
		}
		lastHTTPStatus = status
		rec.FinishTurn(i, status, resp.Content, resp.Error, resp.Usage.InputTokens, resp.Usage.OutputTokens, turnStart)
		if len(turn.AssertState) > 0 {
			world := h.worldState(ctx, resp.Identity, lastNonEmpty(taskIDs), lastNonEmpty(runIDs), workspace, turn.AssertState)
			for _, check := range EvaluateStatePredicates(turn.AssertState, world) {
				check.Name = fmt.Sprintf("turn-%d:%s", i+1, check.Name)
				turnChecks = append(turnChecks, check)
			}
		}
		if turn.WaitForExternalWatches {
			waitCtx, cancelWait := context.WithTimeout(backgroundCtx, turnBudget(c, opts))
			child, outcome, continuationUsage, waitErr := h.waitForExternalWatchContinuation(waitCtx, resp, c.ApprovalMode)
			cancelWait()
			if waitErr != nil {
				return nil, waitErr
			}
			inputTokens += continuationUsage.InputTokens
			outputTokens += continuationUsage.OutputTokens
			runIDs = append(runIDs, child.ID)
			createdRunIDs = append(createdRunIDs, child.ID)
			lastStatus, lastOutcome = outcome.Status, outcome.Status
			lastCompletionReason, lastResumable = outcome.CompletionReason, outcome.Resumable
			lastVerificationState = ""
			lastConditionState = ""
			if outcome.Verification != nil {
				lastVerificationState = outcome.Verification.State
				lastConditionState = outcome.Verification.ConditionState
			}
		}
	}
	// Held turns end when the case stops their Runs; the case's status stays
	// that of its last foreground turn.
	h.joinHeldTurns(held)
	for _, turn := range held {
		noteResponse(turn.index, turn.resp, true)
		checks := h.heldTurnChecks(ctx, turn)
		if ChecksPassed(checks) {
			rec.FinishStoppedTurn(turn.index, turn.status, turn.resp.Content, turn.resp.Usage.InputTokens, turn.resp.Usage.OutputTokens, turn.started)
		} else {
			rec.FinishTurn(turn.index, turn.status, turn.resp.Content, turn.resp.Error, turn.resp.Usage.InputTokens, turn.resp.Usage.OutputTokens, turn.started)
		}
		turnChecks = append(turnChecks, checks...)
	}
	// Every eval turn is synchronous, so its run must be terminal once
	// ProcessMessage returns. Anything still `running` here is a finalization
	// bug or an interrupted write; force a terminal state so eval never leaves
	// phantom running runs behind (in shared_data mode they would show up in
	// the user's /tasks forever).
	forceFinalized := finalizeLeftoverRuns(ctx, h.controlStore, h.tenantID, seenPersons, 3*time.Second)
	snap := rec.Snapshot()
	snap.TaskIDs = append(append([]string(nil), taskIDs...), heldTaskIDs...)
	snap.RunIDs = append(append([]string(nil), runIDs...), heldRunIDs...)
	snap.CreatedRunIDs = createdRunIDs
	snap.HTTPStatus = lastHTTPStatus
	snap.Workspace = observedRunWorkspace(ctx, h.controlStore, h.tenantID, runIDs)
	snap.ExpectedWorkspace = workspace
	snap.DurationSeconds = time.Since(start).Seconds()
	// For eval status checks, prefer the request-level result. Long-lived tasks
	// may remain "running" after a successful turn, while the smoke case is only
	// asking whether this interaction completed.
	snap.OutcomeStatus = firstNonEmpty(lastStatus, lastOutcome)
	snap.CompletionReason = lastCompletionReason
	snap.Resumable = lastResumable
	snap.VerificationState = lastVerificationState
	snap.ConditionState = lastConditionState
	checks := append(EvaluateCase(c, snap), turnChecks...)
	if finalizationCheck, ok := forcedRunFinalizationCheck(forceFinalized); ok {
		// Cleanup keeps the eval database reusable, but the case must fail: a
		// synchronous turn returning with a running run is a product regression.
		checks = append(checks, finalizationCheck)
	}

	// World-state predicates: assert on control.db / files / memory — an oracle
	// the model cannot game by phrasing its answer.
	if len(c.AssertState) > 0 {
		subjectTaskID := lastNonEmpty(taskIDs)
		if subjectTaskID == "" {
			if cur, _ := h.controlStore.CurrentTask(ctx, identity.TenantID, identity.PersonID); cur != nil {
				subjectTaskID = cur.ID
			}
		}
		if subjectTaskID == "" {
			// A deterministic reply (candidates, control commands) returns no
			// Task; the Thread the case seeded is then the only sensible subject
			// for durable-event assertions.
			subjectTaskID = seededTaskID
		}
		world := h.worldState(ctx, identity, subjectTaskID, lastNonEmpty(runIDs), workspace, c.AssertState)
		checks = append(checks, EvaluateStatePredicates(c.AssertState, world)...)
	}

	status := "passed"
	expectedGatewayRejection := c.Expect.HTTPStatus >= 400 && lastHTTPStatus == c.Expect.HTTPStatus
	if (lastStatus == "failed" && !expectedGatewayRejection) || !ChecksPassed(checks) {
		status = "failed"
	}
	rec.FinishCase(status, checks, inputTokens, outputTokens)
	report, err := LoadReport(outPath)
	if err != nil {
		return nil, fmt.Errorf("read eval evidence: %w", err)
	}
	if report.CaseCount != 1 {
		return nil, fmt.Errorf("eval result was not durably recorded")
	}
	return &RunResult{
		CaseID:          c.ID,
		Status:          status,
		OutputPath:      outPath,
		Duration:        time.Since(start),
		ToolCalls:       snap.ToolCalls,
		ActionToolCalls: snap.ActionToolCalls,
		ProgressUpdates: snap.ProgressUpdates,
		ToolErrors:      snap.ToolErrors,
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		Checks:          checks,
		Provenance:      provenance,
		Efficiency:      report.Efficiency,
	}, nil
}

func forcedRunFinalizationCheck(count int) (CheckResult, bool) {
	if count <= 0 {
		return CheckResult{}, false
	}
	return CheckResult{
		Name:    "run_finalization",
		OK:      false,
		Message: fmt.Sprintf("forced %d leftover running run(s) to interrupted after the case", count),
	}, true
}

func observedRunWorkspace(ctx context.Context, store *control.Store, tenantID string, runIDs []string) string {
	if store == nil {
		return ""
	}
	for i := len(runIDs) - 1; i >= 0; i-- {
		runID := strings.TrimSpace(runIDs[i])
		if runID == "" {
			continue
		}
		run, err := store.GetRun(ctx, tenantID, runID)
		if err != nil || run == nil || strings.TrimSpace(run.WorkspaceID) == "" {
			continue
		}
		workspace, err := store.GetWorkspace(ctx, tenantID, run.WorkspaceID)
		if err == nil && workspace != nil {
			return strings.TrimSpace(workspace.LocalPath)
		}
	}
	return ""
}

// evalTurnVCRContext tags a turn only when the explicit eval recorder/replayer
// owns VCR. A normal live eval may run while the user's flight recorder is on;
// giving that flight wrapper the eval case id would create orphan recordings
// under ~/.selfmind/flight/<case-id> with no FlightMeta and could capture a
// transient provider failure as if it were a user turn.
func evalTurnVCRContext(ctx context.Context, caseID, workspace string) context.Context {
	if llm.EvalVCRActive() {
		ctx = llm.WithVCRSession(ctx, caseID)
	}
	return llm.WithVCRWorkspace(ctx, workspace)
}

// finalizeLeftoverRuns guarantees every run this eval created reached a
// terminal status before the harness moves on. Eval turns are synchronous, so
// under normal operation the sweep finds nothing; the bounded wait only covers
// in-flight finalization writes racing the turn's return. Leftovers past the
// deadline are forced to `interrupted` (run + owning task) so they can never
// linger as phantom `running` rows. The sweep is scoped to the tenant AND the
// persons this case touched, so a `shared_data` case can never stomp a real
// concurrent run.
func finalizeLeftoverRuns(ctx context.Context, store *control.Store, tenantID string, personIDs map[string]bool, wait time.Duration) int {
	if store == nil || len(personIDs) == 0 {
		return 0
	}
	persons := make([]string, 0, len(personIDs))
	for id := range personIDs {
		persons = append(persons, id)
	}
	deadline := time.Now().Add(wait)
	for {
		runs, err := store.ListRunningRuns(ctx, tenantID, persons)
		if err != nil || len(runs) == 0 {
			return 0
		}
		if time.Now().After(deadline) {
			for _, r := range runs {
				_ = store.FinishRun(ctx, r.TenantID, r.ID, "interrupted")
				if r.TaskID != "" {
					_ = store.UpdateTaskStatus(ctx, r.TenantID, r.TaskID, "interrupted",
						"Eval run did not reach a terminal status; finalized by the eval harness.", nil)
				}
			}
			return len(runs)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// turnBudget is the per-turn wall-clock deadline. It uses the case's
// MaxDurationSeconds when set, the explicit option, or a safe default that
// prevents a stalled provider stream from hanging the whole eval.
// recordingTurnBudgetFloor is the minimum turn budget while RECORDING.
//
// max_duration_seconds is a REPLAY assertion: it bounds how long the case may
// take when the model's answers come from a cassette and only the tools really
// run. Recording the same case pays full model latency on every call, and the
// gap is large — a repository-inspection case took 288s live and 40s on
// replay, a factor of seven. Holding a recording to the replay budget makes
// any case with a realistic budget impossible to record: the run is killed
// part-way and leaves a truncated cassette.
//
// The floor applies to recording only. Replay keeps the case's own budget,
// because that is the property the case is asserting.
const recordingTurnBudgetFloor = 15 * time.Minute

// evalApprovalWait bounds every approval wait in an eval run. No person can
// answer, so the wait only has to let the pending row settle.
const evalApprovalWait = 100 * time.Millisecond

func turnBudget(c *Case, opts RunOptions) time.Duration {
	budget := resolveTurnBudget(c, opts)
	if llm.VCRRecordMode() && budget < recordingTurnBudgetFloor {
		return recordingTurnBudgetFloor
	}
	return budget
}

func resolveTurnBudget(c *Case, opts RunOptions) time.Duration {
	if opts.TurnTimeout > 0 {
		return opts.TurnTimeout
	}
	if c != nil && c.Expect.MaxDurationSeconds > 0 {
		return time.Duration(c.Expect.MaxDurationSeconds) * time.Second
	}
	if v := strings.TrimSpace(os.Getenv("SELFMIND_EVAL_TURN_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 200 * time.Second
}

func lastNonEmpty(values []string) string {
	for i := len(values) - 1; i >= 0; i-- {
		if strings.TrimSpace(values[i]) != "" {
			return strings.TrimSpace(values[i])
		}
	}
	return ""
}

func writeLiveProgress(w io.Writer, caseID string, turnIndex int, event llm.StreamEvent) {
	if w == nil {
		return
	}
	switch event.EventType {
	case "agent.thinking", "agent.step":
		if text := strings.TrimSpace(event.Content); text != "" {
			fmt.Fprintf(w, "  [%s/%d] %s\n", caseID, turnIndex+1, text)
		}
	case "tool.started":
		if event.ToolName != "" {
			fmt.Fprintf(w, "  [%s/%d] tool: %s started\n", caseID, turnIndex+1, event.ToolName)
		}
	case "tool.completed":
		status := "done"
		if event.Err != nil {
			status = "failed"
		}
		fmt.Fprintf(w, "  [%s/%d] tool: %s %s %.1fs\n", caseID, turnIndex+1, event.ToolName, status, event.DurationSeconds)
	case "model_stream_first_token":
		fmt.Fprintf(w, "  [%s/%d] first token\n", caseID, turnIndex+1)
	}
}

func newRuntimeHarness(opts RunOptions, c *Case, dataDirOverride string) (*runtimeHarness, error) {
	cfg, err := config.LoadConfig(config.Options{Path: opts.ConfigPath, CreateIfMissing: true})
	if err != nil {
		return nil, err
	}
	provider := firstNonEmpty(opts.Provider, c.Provider)
	model := firstNonEmpty(opts.Model, c.Model)
	applyEvalModelOverride(cfg, provider, model)
	configureCredentiallessReplayRoutes(cfg)
	// Isolated scenarios get a fresh data dir so control.db / memory start clean
	// and never touch the user's real ~/.selfmind data.
	isolatedEvalConfig(cfg, dataDirOverride)
	// The case's runtime block, applied before any worker is built: capacity,
	// the cron tool (its scheduler never starts) and the MCP stub servers.
	stubDir := ""
	if dataDirOverride != "" {
		stubDir = filepath.Join(filepath.Dir(dataDirOverride), "mcp")
	}
	if err := applyRuntimeConfig(cfg, c.Runtime, stubDir, c.Path()); err != nil {
		return nil, err
	}
	log.Init(log.Options{Level: cfg.Agent.LogLevel})

	mem, dataDir, err := appcore.InitStorage(cfg)
	if err != nil {
		return nil, err
	}
	modelChanges, err := newEvalModelChangeService(cfg, dataDir, dataDirOverride != "")
	if err != nil {
		if mem != nil {
			_ = mem.Close()
		}
		return nil, err
	}
	if dataDirOverride != "" {
		// The real gateway installs this before constructing tools. Eval must do
		// the same or $SELFMIND_RUN_TMP is absent and a command such as
		// "$SELFMIND_RUN_TMP/file" becomes an out-of-workspace /file write that
		// can wait forever for an approval no eval user can answer.
		if err := executionenv.SetRuntimeRoot(filepath.Join(filepath.Dir(dataDirOverride), "runtime")); err != nil {
			if mem != nil {
				_ = mem.Close()
			}
			return nil, fmt.Errorf("configure eval execution runtime: %w", err)
		}
	}
	tenantID := firstNonEmpty(opts.TenantID, os.Getenv("SELF_TENANT_ID"))
	if tenantID == "" {
		tenantID = fmt.Sprintf("eval-%s-%d", sanitizeFilePart(c.ID), time.Now().UnixNano())
	}
	if c.Setup != nil {
		if err := applySkillSeeds(tenantID, cfg.Evolution.SkillsDir, c.Setup.Skills); err != nil {
			if mem != nil {
				_ = mem.Close()
			}
			return nil, err
		}
	}
	evalPrompts := promptassets.Empty(filepath.Join(dataDir, "prompts"))
	controlStore, err := control.OpenStore(dataDir)
	if err != nil {
		if mem != nil {
			_ = mem.Close()
		}
		return nil, err
	}
	// The daemon's own construction path: with the control store, every worker
	// gets the daemon-owned tools, external-effect claims and the durable
	// review queue, so a case cannot run a smaller or different tool set.
	gwDeps, err := appcore.InitGateway(appcore.GatewayOptions{
		DataDir: dataDir, Memory: mem, Config: cfg, TenantID: tenantID, Prompts: evalPrompts,
		Control: controlStore, Workers: c.Runtime.workers(),
	})
	if err != nil {
		_ = controlStore.Close()
		if mem != nil {
			_ = mem.Close()
		}
		return nil, err
	}
	skillStorage, err := appcore.ResolveSkillStorage(cfg)
	if err != nil {
		gwDeps.Close()
		_ = controlStore.Close()
		if mem != nil {
			_ = mem.Close()
		}
		return nil, err
	}
	displayProvider, displayModel, _ := appcore.ResolveModelDisplay(cfg)
	server := &httpapi.Server{
		Control:         controlStore,
		Gateway:         gwDeps.Gateway,
		DefaultTenantID: tenantID,
		SkillStorage:    skillStorage,
		ModelChanges:    modelChanges,
		// Automatic semantic recall (Work Timeline P2): eval runs the same
		// selector path as real input — no eval-only shortcut around recall.
		Recall: httpapi.NewRecallEngine(controlStore, mem, appcore.SemanticRecallExpander(mem, cfg, tenantID, evalPrompts)),
		// Cross-endpoint explicit memory commands (/remember, /forget) use the
		// same production path in eval.
		Memory: mem,
		// Tool-output spool (execution-quality W1): same derivation as the
		// daemon runner, so eval exercises the artifact + read-back flow
		// inside its isolated data dir.
		ToolOutputDir: filepath.Join(dataDir, "tool-output"),
		// Attachment import store: same derivation as the daemon runner so an
		// eval case with attachments exercises the production import + scope
		// path inside the isolated data dir.
		AttachmentsDir: filepath.Join(dataDir, "attachments"),
	}
	// The daemon's MCP views: /tools readonly and /diag read the same shared
	// manager the workers call.
	if gwDeps.MCP != nil {
		server.MCPHealthFunc = gwDeps.MCP.Health
		server.MCPServersFunc = gwDeps.MCP.Servers
	}
	// No human is attached to eval, so an unexpected ask must fail promptly in
	// every approval mode. Full-auto still asks for capability escalations such
	// as network:shared, and the production budget parked such a call until 30
	// seconds before the turn deadline: 494 of one case's 510 seconds on a
	// Linux runner whose sandbox withheld the network.
	server.ApprovalWait, server.ApprovalWaitUnattended = evalApprovalWait, evalApprovalWait
	// The daemon's admission ceiling: the durable check and the process
	// registry share it, and it may not exceed the worker count.
	if err := server.ConfigureWorkRunCapacity(c.Runtime.capacity(), len(gwDeps.Workers)); err != nil {
		gwDeps.Close()
		_ = controlStore.Close()
		if mem != nil {
			_ = mem.Close()
		}
		return nil, err
	}
	// Like the daemon, durable background reviews run through the primary
	// worker's engine, so a review a worker queued has an executor when the case
	// runs a maintenance pass.
	if primary := gwDeps.Workers[0].Agent; primary.ReviewEngine != nil {
		server.SkillReviewer = primary.ReviewEngine
	}
	leases := newWorkerLeaseLog()
	gwDeps.Gateway.SetWorkerLeaseHook(leases.hook)
	if c.ApprovalMode == "smart" {
		server.ApprovalJudge = appcore.NewConfiguredApprovalJudge(mem, cfg, tenantID)
	}
	if caseNeedsPostRunMaintenance(c) {
		server.PostRunAnalyzer = appcore.NewConfiguredPostRunAnalyzer(mem, cfg, tenantID, evalPrompts, controlStore)
		server.PostRunMaintenance = httpapi.PostRunMaintenanceOptions{
			Debounce: -1, MaxWait: -1, BatchMaxRuns: 10,
		}
		if server.PostRunAnalyzer == nil {
			gwDeps.Close()
			_ = controlStore.Close()
			if mem != nil {
				_ = mem.Close()
			}
			return nil, fmt.Errorf("eval case %s requires post-run maintenance, but no memory_extract route is configured", c.ID)
		}
	}
	harness := &runtimeHarness{
		cfg:          cfg,
		mem:          mem,
		controlStore: controlStore,
		gateway:      gwDeps,
		server:       server,
		tenantID:     tenantID,
		provider:     firstNonEmpty(displayProvider, provider, "default"),
		model:        firstNonEmpty(displayModel, model, "default"),
		leases:       leases,
		stubDir:      stubDir,
		choices:      map[string]string{},
	}
	if c.ApprovalMode == "smart" {
		harness.stopApprovalTelemetry = appcore.InstallApprovalTelemetry(controlStore)
	}
	// Delivery is opt-in per case: wiring it always would give every case a push
	// surface and change which notification paths its runs take.
	server.Delivery = newEvalDeliveryService(c, harness)
	return harness, nil
}

func caseNeedsPostRunMaintenance(c *Case) bool {
	if c == nil {
		return false
	}
	for _, turn := range c.Turns {
		if turn.WaitForMaintenance {
			return true
		}
	}
	return false
}

// newEvalModelChangeService keeps model readiness under the same throwaway
// root as control.db. The agent still uses cfg in memory, but ModelChanges
// reloads from a file and otherwise reads the operator's model-state.json next
// to cfg.Path. That made local replay inherit a verified receipt while a clean
// CI runner parked every model-backed turn before its first cassette call.
func newEvalModelChangeService(cfg *config.Config, dataDir string, isolated bool) (*modelchange.Service, error) {
	if cfg == nil {
		return nil, fmt.Errorf("configure eval model readiness: config is nil")
	}
	if !isolated {
		return &modelchange.Service{ConfigPath: cfg.Path}, nil
	}

	// Persist only the non-secret route snapshot needed by ModelChanges. Do not
	// copy provider credentials or the operator's full configuration into eval
	// artifacts, even though the throwaway root is mode 0700 and is deleted.
	stateCfg := &config.Config{}
	modelchange.ApplySnapshot(stateCfg, modelchange.SnapshotFromConfig(cfg))
	stateCfg.Agent.Soul = ""
	stateCfg.Storage.DataDir = dataDir
	stateCfg.Auth.CredentialsFile = filepath.Join(dataDir, "auth.json")
	stateCfg.Models.Source = "eval"
	configPath := filepath.Join(dataDir, "model-runtime", "config.yaml")
	if err := config.SaveConfig(configPath, stateCfg); err != nil {
		return nil, fmt.Errorf("write isolated eval model config: %w", err)
	}
	service := &modelchange.Service{ConfigPath: configPath}
	ready := llm.EvalVCRReplayMode()
	if !ready && strings.TrimSpace(cfg.Path) != "" {
		// Live/record eval still needs real operator readiness. Mirror only the
		// ready verdict into the throwaway state; do not let eval model commands
		// read or mutate the operator's durable transaction file.
		if sourceStatus, inspectErr := (&modelchange.Service{ConfigPath: cfg.Path}).Inspect(); inspectErr == nil {
			ready = sourceStatus.ModelReady()
		}
	}
	if ready {
		// A replay cassette is the harness's provider boundary. Establish the
		// matching isolated startup receipt so the production readiness gate lets
		// the turn reach that boundary. Live/record mode reaches here only when
		// the source configuration was already ready. Misses still fail closed in
		// the VCR layer.
		status, err := service.AcceptMigrationReadiness()
		if err != nil {
			return nil, fmt.Errorf("establish isolated eval model readiness: %w", err)
		}
		if !status.ModelReady() {
			return nil, fmt.Errorf("establish isolated eval model readiness: main route is not ready")
		}
	}
	return service, nil
}

// makeEvalTempRoot keeps isolated workspaces away from the system temp tree.
// Bubblewrap binds a run's scratch directory at /tmp; placing the runtime root
// beneath /tmp would hide its own real path inside the sandbox. Prefer the user
// cache directory and fall back to the current directory only when HOME itself
// is a test temp directory.
func makeEvalTempRoot(caseID string) (string, error) {
	prefix := "selfmind-eval-" + sanitizeFilePart(caseID) + "-"
	if cacheDir, err := os.UserCacheDir(); err == nil && strings.TrimSpace(cacheDir) != "" && !pathWithin(os.TempDir(), cacheDir) {
		base := filepath.Join(cacheDir, "selfmind", "eval")
		if err := os.MkdirAll(base, 0o700); err == nil {
			if dir, err := os.MkdirTemp(base, prefix); err == nil {
				return dir, nil
			}
		}
	}
	// Tests commonly replace HOME with a directory below /tmp, making the user
	// cache unsuitable for bubblewrap. Prefer the OS-wide persistent temp root
	// before falling back to the repository cwd; on WSL this also avoids DrvFS
	// delete-sharing delays while SQLite closes WAL handles.
	persistentTmp := filepath.Clean("/var/tmp")
	if filepath.IsAbs(persistentTmp) && !pathWithin(os.TempDir(), persistentTmp) {
		base := filepath.Join(persistentTmp, "selfmind", "eval")
		if err := os.MkdirAll(base, 0o700); err == nil {
			if dir, err := os.MkdirTemp(base, prefix); err == nil {
				return dir, nil
			}
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return os.MkdirTemp(cwd, "."+prefix)
}

func cleanupEvalTempRoot(root string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err := os.RemoveAll(root); err != nil {
			lastErr = err
			// Toolchain caches such as GOMODCACHE intentionally contain 0555
			// directories. They are safe to read in the sandbox but their owner
			// cannot unlink children during teardown. Repair only directories in
			// this runner-owned temporary tree; WalkDir does not follow symlinks.
			if chmodErr := makeEvalTempTreeRemovable(root); chmodErr != nil {
				lastErr = fmt.Errorf("%v; prepare retry: %w", err, chmodErr)
			}
		} else if _, err := os.Lstat(root); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("temporary root still exists after removal")
		}
		time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
	}
	return lastErr
}

func makeEvalTempTreeRemovable(root string) error {
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode&0o700 == 0o700 {
			return nil
		}
		return os.Chmod(path, mode|0o700)
	})
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func pathWithin(parent, candidate string) bool {
	parentAbs, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(parentAbs), filepath.Clean(candidateAbs))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func applyEvalModelOverride(cfg *config.Config, provider, model string) {
	if cfg == nil {
		return
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" && model == "" {
		return
	}
	current := cfg.EffectivePrimary()
	if provider == "" {
		provider = current.Provider
	}
	reasoning := ""
	if provider == current.Provider {
		if model == "" {
			model = current.Model
		}
		reasoning = current.Reasoning
	}
	// The current resolver reads models.primary before all legacy fields. Use
	// the same canonical setter as `selfmind model`; writing only cfg.Model here
	// silently left eval --provider/--model on the configured primary route.
	cfg.SetPrimaryModel(provider, model, reasoning)
	// A recording must use one explicit route for every model call, including
	// semantic recall and other bounded roles. Otherwise a healthy --provider
	// can still record an unrelated auxiliary provider's failure at ordinal 0,
	// permanently poisoning the cassette before the foreground turn starts.
	cfg.Models.Auxiliary = config.ModelSelectionConfig{
		Provider:  provider,
		Model:     model,
		Reasoning: reasoning,
	}
	cfg.Models.Roles = nil
}

const (
	evalReplayProvider = "eval-replay"
	evalReplayModel    = "cassette"
)

// configureCredentiallessReplayRoutes gives clean offline runners a complete
// in-memory provider route for auxiliary roles. The foreground keeps its
// configured route (or the existing VCR-wrapped diagnostic provider), while
// strict replay returns a cassette miss before the inert loopback transport
// can run. Record/live eval keeps the operator's real provider and readiness
// requirements.
func configureCredentiallessReplayRoutes(cfg *config.Config) {
	if cfg == nil || !llm.EvalVCRReplayMode() {
		return
	}
	provider := config.CustomProvider{
		Name:     evalReplayProvider,
		BaseURL:  "http://127.0.0.1:1/v1",
		Protocol: "openai-compatible",
		Auth:     "none",
		Model:    evalReplayModel,
	}
	found := false
	for i := range cfg.Providers.Custom {
		if strings.EqualFold(strings.TrimSpace(cfg.Providers.Custom[i].Name), evalReplayProvider) {
			cfg.Providers.Custom[i] = provider
			found = true
			break
		}
	}
	if !found {
		cfg.Providers.Custom = append(cfg.Providers.Custom, provider)
	}
	cfg.Models.Auxiliary = config.ModelSelectionConfig{
		Provider: evalReplayProvider,
		Model:    evalReplayModel,
	}
	cfg.Models.Roles = nil
}

// isolatedEvalConfig redirects every config-derived durable path into the
// case's throwaway temp data dir. Storage.DataDir covers control.db, memory,
// and the tool-output spool (all derive from the data dir). Evolution.SkillsDir
// needs an explicit override: app wiring defaults the skills base dir to
// ~/.selfmind. App wiring injects this base into skill tools, learning audit,
// curation, and post-run maintenance; leaving it empty would let an eval-only
// person partition escape into the real home directory. Any future config field
// whose default resolves under the user's home dir and is consumed by app
// wiring must be redirected here before the harness builds the runtime.
func isolatedEvalConfig(cfg *config.Config, dataDir string) {
	if cfg == nil {
		return
	}
	// Release evidence never inherits operator identity, including the explicit
	// shared_data path where durable storage is intentionally not redirected.
	cfg.Agent.Soul = ""
	if strings.TrimSpace(dataDir) == "" {
		return
	}
	cfg.Storage.DataDir = dataDir
	cfg.Evolution.SkillsDir = filepath.Join(dataDir, "skills")
}

func (h *runtimeHarness) Close() {
	if h == nil {
		return
	}
	if h.stopApprovalTelemetry != nil {
		h.stopApprovalTelemetry()
	}
	if h.leases != nil {
		h.leases.releaseHolds()
	}
	h.gateway.Close()
	if h.controlStore != nil {
		_ = h.controlStore.Close()
	}
	if h.mem != nil {
		_ = h.mem.Close()
	}
}

func resolveWorkspace(c *Case, override string) (string, error) {
	raw := firstNonEmpty(override, c.Workspace)
	if raw == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		raw = cwd
	}
	raw = os.ExpandEnv(raw)
	if strings.HasPrefix(raw, "~/") || raw == "~" {
		home, _ := os.UserHomeDir()
		if raw == "~" {
			raw = home
		} else {
			raw = filepath.Join(home, raw[2:])
		}
	}
	if !filepath.IsAbs(raw) {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return "", err
		}
		raw = abs
	}
	return filepath.Clean(raw), nil
}

func canonicalEvalWorkspace(raw string) string {
	raw = filepath.Clean(strings.TrimSpace(raw))
	if raw == "" || raw == "." {
		return raw
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return raw
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(absolute)
}

func DefaultRunPath(c *Case, provider string) string {
	caseID := "case"
	suite := "default"
	if c != nil {
		caseID = sanitizeFilePart(c.ID)
		suite = sanitizeFilePart(firstNonEmpty(c.Suite, filepath.Base(filepath.Dir(c.Path())), "default"))
	}
	provider = sanitizeFilePart(firstNonEmpty(provider, "default"))
	date := time.Now().Format("2006-01-02")
	return filepath.Join("evalruns", date, suite+"-"+caseID+"-"+provider+".jsonl")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func sanitizeFilePart(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if out == "" {
		return "default"
	}
	return out
}
