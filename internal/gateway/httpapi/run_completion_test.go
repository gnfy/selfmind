package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"selfmind/internal/control"
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
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
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
	if outcome.Status != "interrupted" || outcome.CompletionReason != "provider_or_transport_error" || !outcome.Resumable {
		t.Fatalf("outcome = %#v", outcome)
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

func TestFinalizeErroredRunPreservesStallAttributionAcrossGatewayBoundary(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
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
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
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
