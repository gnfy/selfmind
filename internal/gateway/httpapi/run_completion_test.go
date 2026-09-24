package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/runpool"
)

func TestStructuredWaitingExternalSurvivesGenericTurnCompletion(t *testing.T) {
	outcome := reconcileTurnCompletion(api.RunOutcome{
		Status:  "waiting_external",
		Summary: "Watching the external build.",
	}, router.TurnCompletion{
		Status:           "completed",
		CompletionReason: "completed",
	}, true)

	if outcome.Status != "waiting_external" || outcome.CompletionReason != "waiting_external" || outcome.Resumable {
		t.Fatalf("outcome = %#v", outcome)
	}
	if got := turnStatusForOutcome(outcome); got != "waiting_external" {
		t.Fatalf("turn status = %q, want waiting_external", got)
	}
}

func TestTurnStatusCompletesAnOpenTaskOutcome(t *testing.T) {
	for _, status := range []string{"running", "in_progress"} {
		if got := turnStatusForOutcome(api.RunOutcome{Status: status}); got != "completed" {
			t.Fatalf("turn status for %q = %q, want completed", status, got)
		}
	}
}

func TestDirectAnswerNormalizesRunningOutcomeToDone(t *testing.T) {
	outcome := reconcileTurnCompletion(api.RunOutcome{
		Status:  "running",
		Summary: "Answered directly without tools.",
	}, router.TurnCompletion{
		Status:           "completed",
		CompletionReason: "completed",
	}, false)

	if outcome.Status != "done" || outcome.CompletionReason != "completed" || outcome.Resumable {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestDirectAnswerCompletesRunWithoutClosingTaskLabel(t *testing.T) {
	outcome := api.RunOutcome{Status: "done", CompletionReason: "completed"}
	if got := taskStatusForFinalization(outcome, false); got != "in_progress" {
		t.Fatalf("plain answer task status = %q, want in_progress", got)
	}
	if got := taskStatusForFinalization(outcome, true); got != "done" {
		t.Fatalf("structured completion task status = %q, want done", got)
	}
}

func TestExternalFailureDoesNotBecomeAgentFailure(t *testing.T) {
	outcome := reconcileExternalWatchOutcome(api.RunOutcome{
		Status: "failed", CompletionReason: "failed", Summary: "The build failed.",
	}, &control.ExternalWatch{
		ID: "watch_123", Status: control.ExternalWatchFailed,
		CheckerStatus: control.WatchCheckerOK, OperationStatus: control.WatchOperationFailed,
	})
	if outcome.Status != "done" || outcome.CompletionReason != "completed_with_external_failure" {
		t.Fatalf("outcome = %#v", outcome)
	}
	if outcome.External == nil || outcome.External.Status != control.ExternalWatchFailed {
		t.Fatalf("external outcome = %#v", outcome.External)
	}
	if got := taskStatusForFinalization(outcome, true); got != "blocked" {
		t.Fatalf("task status = %q, want blocked", got)
	}
}

func TestExternalFailureDoesNotHideFinalizerVerificationFailure(t *testing.T) {
	outcome := reconcileExternalWatchOutcome(api.RunOutcome{
		Status:       api.RunStatusVerificationPartial,
		Verification: &api.VerificationOutcome{State: "failed"},
	}, &control.ExternalWatch{ID: "watch_123", Status: control.ExternalWatchFailed})
	if outcome.Status != api.RunStatusVerificationPartial {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestExternalTimeoutParksTaskForUser(t *testing.T) {
	outcome := reconcileExternalWatchOutcome(api.RunOutcome{Status: "waiting_external"}, &control.ExternalWatch{
		ID: "watch_456", Status: control.ExternalWatchTimedOut,
	})
	if outcome.Status != "done" || outcome.CompletionReason != "completed_with_external_timeout" {
		t.Fatalf("outcome = %#v", outcome)
	}
	if got := taskStatusForFinalization(outcome, true); got != "waiting_user" {
		t.Fatalf("task status = %q, want waiting_user", got)
	}
}

func TestMissingFinalResponseCannotCompleteRun(t *testing.T) {
	outcome := reconcileMissingFinalResponse(api.RunOutcome{
		Status:           "done",
		CompletionReason: "completed",
		Summary:          "SelfMind finished this turn without producing a final response.",
	}, false, false)

	if outcome.Status != "interrupted" || outcome.CompletionReason != "missing_final_response" || !outcome.Resumable {
		t.Fatalf("outcome = %#v", outcome)
	}
	if got := taskStatusForFinalization(outcome, false); got != "interrupted" {
		t.Fatalf("task status = %q, want interrupted", got)
	}
}

func TestMissingFinalResponsePreservesKnownBlocker(t *testing.T) {
	want := api.RunOutcome{Status: "blocked", CompletionReason: "plan_unresolved", Summary: "The required destination check has not passed."}
	got := reconcileMissingFinalResponse(want, false, false)
	if got.Status != want.Status || got.CompletionReason != want.CompletionReason || got.Summary != want.Summary || !got.Resumable {
		t.Fatalf("known blocker was lost: %#v", got)
	}
}

func TestMissingFinalSummaryRetainsEvidenceWithoutClaimingCompletion(t *testing.T) {
	outcome := api.RunOutcome{Files: []string{"report.csv"}, Verification: &api.VerificationOutcome{State: "not_run", Summary: "No structured verification evidence."}}
	got, summary := missingFinalEvidenceSummary("run_test", outcome, nil)
	for _, want := range []string{"**Work remains**\n\n", "without a final response", "report.csv", "not_run", "/resume run_test"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "completed") || summary != "The run stopped without a final response." {
		t.Fatalf("missing response was called complete: result=%q summary=%q", got, summary)
	}
}

// A rejected finish_run leaves no structured outcome. The accepted plan still
// says how far the work got; the evidence after it says what was observed.
func TestMissingFinalSummaryLeadsWithAcceptedPlanProgress(t *testing.T) {
	outcome := api.RunOutcome{
		Files:        []string{"releases/2026-09-24-record.md"},
		Verification: &api.VerificationOutcome{State: "stale", Summary: "1 verification check(s) need review after changes to their inputs."},
	}
	steps := []taskPlanStep{
		{Step: "触发 Cloud Build 并回读终态", Status: "completed"},
		{Step: "Retire the duplicate probe", Status: "cancelled"},
		{Step: "写入发布记录并推送", Status: "in_progress"},
		{Step: "Report the result", Status: "pending"},
	}
	got, summary := missingFinalEvidenceSummary("run_test", outcome, steps)
	for _, want := range []string{
		"**Work remains**\n\nThe run stopped without a final response: 2 of 4 steps resolved. Open plan step: 写入发布记录并推送",
		"**Completed plan steps**\n- 触发 Cloud Build 并回读终态",
		"**Open plan steps**\n- 写入发布记录并推送\n- Report the result",
		"releases/2026-09-24-record.md", "Recorded verification: stale", "/resume run_test",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "Retire the duplicate probe") {
		t.Fatalf("a cancelled step was presented as work: %s", got)
	}
	if summary != "The run stopped without a final response: 2 of 4 steps resolved. Open plan step: 写入发布记录并推送" {
		t.Fatalf("summary = %q", summary)
	}
}

// A release plan whose seven steps all completed while its last check went
// stale must still show its final step: that is where the work remains.
func TestMissingFinalSummaryListsTheWholeTypicalPlan(t *testing.T) {
	var steps []taskPlanStep
	for i := 1; i <= 7; i++ {
		steps = append(steps, taskPlanStep{Step: fmt.Sprintf("release step %d", i), Status: "completed"})
	}
	got, _ := missingFinalEvidenceSummary("run_test", api.RunOutcome{Verification: &api.VerificationOutcome{State: "stale"}}, steps)
	if !strings.Contains(got, "7 of 7 steps resolved") || !strings.Contains(got, "- release step 7") || strings.Contains(got, "more recorded") {
		t.Fatalf("typical plan was not listed whole: %s", got)
	}
}

// The coordinator reads progress from the durable plan, not from the turn's
// narration.
func TestMissingFinalSummaryReadsAcceptedRunPlan(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "publish", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "publish")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	if steps := daemon.coordinator().acceptedPlanSteps(ctx, identity.TenantID, run.ID); len(steps) != 0 {
		t.Fatalf("a run without a plan projected steps: %#v", steps)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "publish", []control.RunPlanStepInput{
		{Step: "dispatch the build", Status: "completed"}, {Step: "write the release record", Status: "in_progress"},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := missingFinalEvidenceSummary(run.ID, api.RunOutcome{}, daemon.coordinator().acceptedPlanSteps(ctx, identity.TenantID, run.ID))
	for _, want := range []string{"1 of 2 steps resolved. Open plan step: write the release record", "**Completed plan steps**\n- dispatch the build", "/resume " + run.ID} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
}

// The structured summary is the answer when no separate prose exists. A CJK
// decision request that fits the character bound must survive intact even when
// lists follow; only a runaway field is cut.
func TestStructuredResultFallbackBoundsSummaryByCharacters(t *testing.T) {
	decision := strings.Repeat("预检已完成，源版本与主干一致。", 26) + "请你确认两件事：是否派发构建，以及是否接受降级的发布记录。"
	got := structuredResultFallback(api.RunOutcome{
		Status: "waiting_user", Summary: decision,
		Done: []string{"预检结果已记录"}, NextSteps: []string{strings.Repeat("确认后派发构建并回读终态", 30)},
	})
	if !strings.Contains(got, decision) {
		t.Fatalf("decision request was cut: %s", got)
	}
	if want := "- " + strings.Repeat("确认后派发构建并回读终态", 26) + "确认后派发构建并..."; !strings.Contains(got, want) {
		t.Fatalf("list item was not bounded by characters: %s", got)
	}
	runaway := strings.Repeat("长", resultSummaryRunes+50)
	got = structuredResultFallback(api.RunOutcome{Status: "waiting_user", Summary: runaway})
	if strings.Contains(got, runaway) || !strings.Contains(got, strings.Repeat("长", resultSummaryRunes)+"...") {
		t.Fatalf("runaway summary was not bounded: %d runes", len([]rune(got)))
	}
}

func TestStructuredOutcomeIsFinalWithoutSeparateProse(t *testing.T) {
	outcome := reconcileMissingFinalResponse(api.RunOutcome{
		Status:           "done",
		CompletionReason: "completed",
		Summary:          "Implemented and verified.",
	}, true, false)

	if outcome.Status != "done" || outcome.CompletionReason != "completed" || outcome.Resumable {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestStructuredResultFallbackKeepsDecisionAndEvidenceReadable(t *testing.T) {
	got := structuredResultFallback(api.RunOutcome{
		Status: "waiting_user", Summary: "预检完成。\n\n等待确认。", Done: []string{"项目和源版本已核对", "预检结果已记录"},
		NextSteps: []string{"确认后派发构建"}, Risks: []string{"发布脚本不支持无 tag 输入"},
	})
	for _, want := range []string{"**Awaiting your decision**", "预检完成。\n\n等待确认。", "**Completed**\n- 项目和源版本已核对", "**Next steps**\n- 确认后派发构建", "**Risks**\n- 发布脚本不支持无 tag 输入"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted result missing %q: %s", want, got)
		}
	}
}

func TestUnresolvedPlanFallbackDoesNotPresentProgressNarrationAsResult(t *testing.T) {
	got, summary := unresolvedPlanFallback(api.RunOutcome{
		Status: "interrupted", Summary: "Retrying the plan close-out because...",
		External: &api.ExternalOutcome{Status: "succeeded"},
	}, []taskPlanStep{{Step: "Publish", Status: "completed"}, {Step: "Record outcome", Status: "in_progress"}})
	for _, want := range []string{"1 of 2 steps resolved", "External observation: succeeded", "**Completed plan steps**\n- Publish", "**Open plan steps**\n- Record outcome"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatted plan result missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "Retrying") || !strings.Contains(summary, "1 of 2") {
		t.Fatalf("progress narration leaked into result: result=%q summary=%q", got, summary)
	}
}

func TestIncompleteTurnOverridesStructuredWaitingExternal(t *testing.T) {
	outcome := reconcileTurnCompletion(api.RunOutcome{
		Status: "waiting_external",
	}, router.TurnCompletion{
		Status:           "incomplete",
		CompletionReason: "output_limit",
		Resumable:        true,
	}, true)

	if outcome.Status != "interrupted" || outcome.CompletionReason != "output_limit" || !outcome.Resumable {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestStructuredDoneSurvivesExhaustedActionBudget(t *testing.T) {
	outcome := reconcileTurnCompletion(api.RunOutcome{
		Status:  "done",
		Summary: "Implemented and verified.",
	}, router.TurnCompletion{
		Status:           "incomplete",
		CompletionReason: "tool_budget_exhausted",
		Resumable:        true,
	}, true)

	if outcome.Status != "done" || outcome.CompletionReason != "completed" || outcome.Resumable {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestFinalizeErroredRunIsDurableAndResumable(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "provider failure",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	run, err := store.StartRun(ctx, task, "cli", "keep working")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	server := &Server{Control: store, DefaultTenantID: "default"}
	outcome := server.coordinator().finalizeErroredRun(ctx, identity, task, run, "cli", errors.New("unexpected EOF"))
	if outcome.Status != "interrupted" || outcome.CompletionReason != "provider_or_transport_error" || !outcome.Resumable ||
		!outcome.RecoveryScheduled || outcome.Recovery != nil {
		t.Fatalf("outcome = %#v", outcome)
	}
	if content, errorText := interruptedRunResponse(task.Title, outcome); errorText != "" ||
		!strings.Contains(content, "recovery run is queued") || strings.Contains(content, "unexpected EOF") {
		t.Fatalf("response content=%q error=%q", content, errorText)
	}
	runs, err := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 10)
	if err != nil || len(runs) != 1 || runs[0].Status != "interrupted" {
		t.Fatalf("runs = %#v, err=%v", runs, err)
	}
	gotTask, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || gotTask.Status != "interrupted" || gotTask.ActiveRunID != "" {
		t.Fatalf("task = %#v, err=%v", gotTask, err)
	}
	events, err := store.ListTaskEvents(ctx, task.ID, 10)
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	found := false
	for _, event := range events {
		if event.Type == "run.interrupted" && event.RunID == run.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("run.interrupted event missing: %#v", events)
	}
	job, err := store.GetMaintenanceJob(ctx, identity.TenantID, run.ID, postRunAnalyzerVersion)
	if err != nil || job == nil || job.PayloadJSON == "" {
		t.Fatalf("maintenance replay payload missing: job=%#v err=%v", job, err)
	}
	var replay postRunJobPayload
	if err := json.Unmarshal([]byte(job.PayloadJSON), &replay); err != nil {
		t.Fatalf("decode maintenance replay: %v", err)
	}
	if replay.Outcome.Status != "interrupted" || replay.Run.ID != run.ID {
		t.Fatalf("maintenance replay = %#v", replay)
	}
}

func TestFinalizeErroredRunReturnsStructuredHandoffWhenMutationCannotAutoResume(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "external release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "release and verify")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordToolDispatch(ctx, identity.TenantID, control.ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "call-release", ToolName: "terminal", ArgsHash: "private-hash",
		RetryClass: "side_effect", EffectID: "effect-release", Strategy: "mutate",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordToolOutcome(ctx, identity.TenantID, run.ID, "call-release", true); err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store, DefaultTenantID: "default"}
	outcome := server.coordinator().finalizeErroredRun(ctx, identity, task, run, "cli", errors.New("unexpected EOF"))
	if outcome.RecoveryScheduled || outcome.Recovery == nil || outcome.Recovery.Reason != "known_effect_requires_user_resume" {
		t.Fatalf("outcome=%#v", outcome)
	}
	content, errorText := interruptedRunResponse(task.Title, outcome)
	if errorText != "" || !strings.Contains(content, "Recovery handoff:") ||
		!strings.Contains(content, "Original goal: release and verify") ||
		!strings.Contains(content, "/resume "+run.ID) || strings.Contains(content, "private-hash") {
		t.Fatalf("response content=%q error=%q", content, errorText)
	}
}

func TestFinalizeErroredRunPreservesStallAttributionAcrossGatewayBoundary(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "stalled", "Local User")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "stalled run", Channel: "cli",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	run, err := store.StartRun(ctx, task, "cli", "run the tool")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	server := &Server{Control: store, DefaultTenantID: "default"}
	outcome := server.coordinator().finalizeErroredRun(ctx, identity, task, run, "cli",
		fmt.Errorf("watchdog boundary: %w", runpool.ErrStalled))
	if outcome.Status != "interrupted" || outcome.CompletionReason != "stalled" || !outcome.Resumable {
		t.Fatalf("outcome = %#v", outcome)
	}
	storedRuns, err := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 10)
	if err != nil || len(storedRuns) != 1 || storedRuns[0].Status != "interrupted" {
		t.Fatalf("runs = %#v, err=%v", storedRuns, err)
	}
}

func TestFinalizeErroredRunPreservesDurableStructuredOutcome(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "durable finish",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	run, err := store.StartRun(ctx, task, "cli", "finish the work")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	_, err = store.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      run.ID,
		Type:       "run.outcome",
		Visibility: "task",
		Channel:    "cli",
		Payload: mustJSON(api.RunOutcome{
			Status:  "done",
			Summary: "The requested work was completed.",
			Done:    []string{"Merged the pull request."},
		}),
	})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	server := &Server{Control: store, DefaultTenantID: "default"}
	outcome := server.coordinator().finalizeErroredRun(ctx, identity, task, run, "cli", errors.New("unexpected EOF"))
	if outcome.Status != "done" || outcome.CompletionReason != "completed" || outcome.Resumable {
		t.Fatalf("outcome = %#v", outcome)
	}
	runs, err := store.ListTaskRuns(ctx, identity.TenantID, task.ID, 10)
	if err != nil || len(runs) != 1 || runs[0].Status != "done" {
		t.Fatalf("runs = %#v, err=%v", runs, err)
	}
	gotTask, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || gotTask.Status != "done" {
		t.Fatalf("task = %#v, err=%v", gotTask, err)
	}
}
