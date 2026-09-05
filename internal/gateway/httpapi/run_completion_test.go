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
