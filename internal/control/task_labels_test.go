package control

import (
	"context"
	"testing"
)

func labelTestStore(t *testing.T) (*Store, *IdentityContext) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
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

// TestReassignRunMovesEverythingAndCleansPlaceholder: the run row, its events,
// and its artifacts move in one transaction; the empty auto-created source is
// deleted, its handoffs fold into the target, and a current_task pointer at
// the source is repointed.
func TestReassignRunMovesEverythingAndCleansPlaceholder(t *testing.T) {
	store, identity := labelTestStore(t)
	ctx := context.Background()
	target := mustCreateTask(t, store, identity, "target label")
	placeholder := mustCreateTask(t, store, identity, "placeholder") // current now

	run, err := store.StartRun(ctx, placeholder, "cli", "the run")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.FinishRun(ctx, identity.TenantID, run.ID, "done")
	if _, err := store.AppendEvent(ctx, Event{TaskID: placeholder.ID, RunID: run.ID, Type: "run.finished"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveArtifact(ctx, Artifact{TaskID: placeholder.ID, RunID: run.ID, Kind: "file", URI: "/tmp/x.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveHandoff(ctx, Handoff{TaskID: placeholder.ID, Summary: "built x"}); err != nil {
		t.Fatal(err)
	}

	if err := store.ReassignRun(ctx, identity.TenantID, run.ID, placeholder.ID, target.ID, true); err != nil {
		t.Fatal(err)
	}

	runs, _ := store.ListTaskRuns(ctx, identity.TenantID, target.ID, 10)
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("run not moved: %+v", runs)
	}
	events, _ := store.ListTaskEvents(ctx, target.ID, 20)
	found := false
	for _, ev := range events {
		if ev.RunID == run.ID && ev.Type == "run.finished" {
			found = true
		}
	}
	if !found {
		t.Fatalf("events did not move: %+v", events)
	}
	artifacts, _ := store.ListTaskArtifacts(ctx, target.ID, 20)
	if len(artifacts) != 1 {
		t.Fatalf("artifacts did not move: %+v", artifacts)
	}
	handoff, _ := store.LatestHandoff(ctx, target.ID)
	if handoff == nil || handoff.Summary != "built x" {
		t.Fatalf("handoff did not fold into the target: %+v", handoff)
	}
	if ghost, _ := store.GetTask(ctx, identity.TenantID, placeholder.ID); ghost != nil {
		t.Fatalf("empty placeholder should be deleted: %+v", ghost)
	}
	current, _ := store.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if current == nil || current.ID != target.ID {
		t.Fatalf("current_task should repoint to the target: %+v", current)
	}
}

// TestReassignRunKeepsSourceWithOtherRuns: cleanup never deletes a source that
// still owns runs, and cleanupFrom=false never deletes at all.
func TestReassignRunKeepsSourceWithOtherRuns(t *testing.T) {
	store, identity := labelTestStore(t)
	ctx := context.Background()
	target := mustCreateTask(t, store, identity, "target")
	source := mustCreateTask(t, store, identity, "source with history")

	keep, err := store.StartRun(ctx, source, "cli", "older run")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.FinishRun(ctx, identity.TenantID, keep.ID, "done")
	move, err := store.StartRun(ctx, source, "cli", "moved run")
	if err != nil {
		t.Fatal(err)
	}
	_ = store.FinishRun(ctx, identity.TenantID, move.ID, "done")

	if err := store.ReassignRun(ctx, identity.TenantID, move.ID, source.ID, target.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetTask(ctx, identity.TenantID, source.ID); got == nil {
		t.Fatal("source with remaining runs must survive cleanup")
	}
	if runs, _ := store.ListTaskRuns(ctx, identity.TenantID, source.ID, 10); len(runs) != 1 {
		t.Fatalf("source should keep its other run, got %d", len(runs))
	}
	if runs, _ := store.ListTaskRuns(ctx, identity.TenantID, target.ID, 10); len(runs) != 1 {
		t.Fatalf("target should own the moved run, got %d", len(runs))
	}

	// Guard: reassigning to a nonexistent target fails atomically.
	if err := store.ReassignRun(ctx, identity.TenantID, keep.ID, source.ID, "task_missing", true); err == nil {
		t.Fatal("reassign to a missing target must error")
	}
	if runs, _ := store.ListTaskRuns(ctx, identity.TenantID, source.ID, 10); len(runs) != 1 {
		t.Fatalf("failed reassign must not move the run, got %d on source", len(runs))
	}
}
