package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
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
