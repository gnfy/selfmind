package control

import (
	"context"
	"encoding/json"
	"testing"
)

func labelTestStore(t *testing.T) (*Store, *IdentityContext) {
	t.Helper()
	store := newTestStore(t)
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Me")
	if err != nil {
		t.Fatal(err)
	}
	return store, identity
}

func mustCreateTask(t *testing.T, store *Store, identity *IdentityContext, title string) *Task {
	t.Helper()
	task, err := store.CreateTask(context.Background(), TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title, Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestRenameTask(t *testing.T) {
	store, identity := labelTestStore(t)
	ctx := context.Background()
	task := mustCreateTask(t, store, identity, "provisional")

	if err := store.RenameTask(ctx, identity.TenantID, task.ID, "Stable title"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetTask(ctx, identity.TenantID, task.ID)
	if got == nil || got.Title != "Stable title" {
		t.Fatalf("title = %+v", got)
	}
	if err := store.RenameTask(ctx, identity.TenantID, "task_missing", "x"); err == nil {
		t.Fatal("renaming a missing task must error")
	}
	if err := store.RenameTask(ctx, identity.TenantID, task.ID, "  "); err == nil {
		t.Fatal("empty title must error")
	}
}

func TestRunCountsAndListTaskRuns(t *testing.T) {
	store, identity := labelTestStore(t)
	ctx := context.Background()
	a := mustCreateTask(t, store, identity, "A")
	b := mustCreateTask(t, store, identity, "B")

	for i := 0; i < 2; i++ {
		run, err := store.StartRun(ctx, a, "cli", "run on A")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
			t.Fatal(err)
		}
	}
	run, err := store.StartRun(ctx, b, "cli", "run on B")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.FinishRun(ctx, identity.TenantID, run.ID, "done")

	counts, err := store.RunCountsByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[a.ID] != 2 || counts[b.ID] != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	runs, err := store.ListTaskRuns(ctx, identity.TenantID, a.ID, 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("ListTaskRuns(A) = %d (%v)", len(runs), err)
	}
	if runs[0].TaskID != a.ID || runs[0].Status != "done" {
		t.Fatalf("run row = %+v", runs[0])
	}
}

// TestLatestRunSummariesPicksNewestPerTask: one grouped query returns the most
// recent run's input summary for each of the person's tasks.
func TestLatestRunSummariesPicksNewestPerTask(t *testing.T) {
	store, identity := labelTestStore(t)
	ctx := context.Background()
	a := mustCreateTask(t, store, identity, "A")
	b := mustCreateTask(t, store, identity, "B")

	for _, summary := range []string{"first ask", "second ask"} {
		run, err := store.StartRun(ctx, a, "cli", summary)
		if err != nil {
			t.Fatal(err)
		}
		_ = store.FinishRun(ctx, identity.TenantID, run.ID, "done")
	}
	run, err := store.StartRun(ctx, b, "cli", "only ask on B")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.FinishRun(ctx, identity.TenantID, run.ID, "done")

	got, err := store.LatestRunSummaries(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if got[a.ID] != "second ask" {
		t.Fatalf("latest summary for A = %q, want %q", got[a.ID], "second ask")
	}
	if got[b.ID] != "only ask on B" {
		t.Fatalf("latest summary for B = %q", got[b.ID])
	}
}

func TestLatestRunOutcomesByPersonPicksNewestTerminalEvent(t *testing.T) {
	store, identity := labelTestStore(t)
	ctx := context.Background()
	task := mustCreateTask(t, store, identity, "release")
	run, err := store.StartRun(ctx, task, "cli", "publish")
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"provider_error", "daemon_recovery"} {
		payload, _ := json.Marshal(map[string]interface{}{
			"outcome": map[string]interface{}{"completion_reason": reason, "resumable": true},
		})
		if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "run.interrupted", Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.LatestRunOutcomesByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if got[task.ID].CompletionReason != "daemon_recovery" || !got[task.ID].Resumable {
		t.Fatalf("latest outcome = %+v", got[task.ID])
	}
}

// TestLatestHandoffFilesByPerson: the grouped query returns the LATEST
// handoff's changed files per task and omits tasks with no handoff.
func TestLatestHandoffFilesByPerson(t *testing.T) {
	store, identity := labelTestStore(t)
	ctx := context.Background()
	withFiles := mustCreateTask(t, store, identity, "with files")
	bare := mustCreateTask(t, store, identity, "no handoff")

	if _, err := store.SaveHandoff(ctx, Handoff{TaskID: withFiles.ID, Summary: "v1", ChangedFiles: []string{"old.html"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, Handoff{TaskID: withFiles.ID, Summary: "v2", ChangedFiles: []string{"games/new.html", "notes.md"}}); err != nil {
		t.Fatal(err)
	}

	got, err := store.LatestHandoffFilesByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	files := got[withFiles.ID]
	if len(files) != 2 || files[0] != "games/new.html" {
		t.Fatalf("latest handoff files = %+v", files)
	}
	if _, ok := got[bare.ID]; ok {
		t.Fatalf("task without handoff must be absent, got %+v", got)
	}
}

// TestPendingCountsByTask: pending approvals and questions group by task in
// two queries; answered/expired rows and rows without a task never count.
func TestPendingCountsByTask(t *testing.T) {
	store, identity := labelTestStore(t)
	ctx := context.Background()
	task := mustCreateTask(t, store, identity, "guarded work")
	other := mustCreateTask(t, store, identity, "quiet work")

	for i := 0; i < 2; i++ {
		if _, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
			TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Taskless approval: never attributed to a card.
	if _, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
	}); err != nil {
		t.Fatal(err)
	}
	clarify, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, Question: "which one?",
	})
	if err != nil {
		t.Fatal(err)
	}
	answered, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, Question: "already handled?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AnswerClarifyRequest(ctx, identity.TenantID, identity.PersonID, answered.ID, "yes", "cli"); err != nil {
		t.Fatal(err)
	}
	_ = clarify

	approvals, questions, err := store.PendingCountsByTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if approvals[task.ID] != 2 {
		t.Fatalf("approvals[task] = %d, want 2 (%+v)", approvals[task.ID], approvals)
	}
	if questions[task.ID] != 1 {
		t.Fatalf("questions[task] = %d, want 1 (%+v)", questions[task.ID], questions)
	}
	if approvals[other.ID] != 0 || questions[other.ID] != 0 {
		t.Fatalf("quiet task must stay at zero: %+v / %+v", approvals, questions)
	}
}
