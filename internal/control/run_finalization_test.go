package control

import (
	"context"
	"testing"
)

func TestMaterializeRunFinalizationIsAtomicAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatalf("ResolveOrCreateAccount: %v", err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "atomic finalization",
		Channel:  "cli-session",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	run, err := store.StartRun(ctx, task, "cli-session", "finish this")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	input := RunFinalization{
		Identity:           *identity,
		RunID:              run.ID,
		RunStatus:          "done",
		TaskID:             task.ID,
		TaskStatus:         "done",
		Summary:            "finished atomically",
		NextSteps:          []string{"ship it"},
		Channel:            "cli-session",
		AssistantContent:   "All work is complete.",
		AnalyzerVersion:    2,
		MaintenancePayload: `{"kind":"post-run"}`,
		Handoff: Handoff{
			Summary:      "finished atomically",
			DoneItems:    []string{"implemented"},
			NextSteps:    []string{"ship it"},
			ChangedFiles: []string{"main.go"},
			TestStatus:   "passed",
		},
		Event: Event{Type: "run.finished", Payload: []byte(`{"status":"done"}`)},
	}
	first, err := store.MaterializeRunFinalization(ctx, input)
	if err != nil {
		t.Fatalf("first finalization: %v", err)
	}
	second, err := store.MaterializeRunFinalization(ctx, input)
	if err != nil {
		t.Fatalf("replayed finalization: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("replay returned event %q; want %q", second.ID, first.ID)
	}

	assertCount := func(table, where string, args ...any) {
		t.Helper()
		var count int
		query := "SELECT COUNT(*) FROM " + table
		if where != "" {
			query += " WHERE " + where
		}
		if err := store.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("%s count = %d; want 1", table, count)
		}
	}
	assertCount("task_events", "run_id = ? AND type = 'run.finished'", run.ID)
	assertCount("channel_messages", "task_id = ? AND role = 'assistant'", task.ID)
	assertCount("task_handoffs", "task_id = ?", task.ID)
	assertCount("maintenance_jobs", "run_id = ? AND analyzer_version = 2", run.ID)

	storedRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || storedRun == nil || storedRun.Status != "done" || storedRun.FinishedAt == nil {
		t.Fatalf("stored run = %+v, %v; want terminal done", storedRun, err)
	}
	storedTask, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || storedTask == nil {
		t.Fatalf("stored task = %+v, %v", storedTask, err)
	}
	if storedTask.Status != "done" || storedTask.ActiveRunID != "" || storedTask.CurrentSummary != input.Summary {
		t.Fatalf("stored task = %+v; want done with cleared active run", storedTask)
	}
}

func TestMaterializeRunFinalizationRollsBackMismatchedTask(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatalf("ResolveOrCreateAccount: %v", err)
	}
	first, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "second"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, first, "cli", "work")
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity:   *identity,
		RunID:      run.ID,
		TaskID:     second.ID,
		RunStatus:  "done",
		TaskStatus: "done",
		Summary:    "must roll back",
	})
	if err == nil {
		t.Fatal("mismatched run/task finalization unexpectedly succeeded")
	}
	storedRun, getErr := store.GetRun(ctx, identity.TenantID, run.ID)
	if getErr != nil || storedRun == nil || storedRun.Status != "running" || storedRun.FinishedAt != nil {
		t.Fatalf("run changed after rollback: %+v, %v", storedRun, getErr)
	}
	storedSecond, getErr := store.GetTask(ctx, identity.TenantID, second.ID)
	if getErr != nil || storedSecond == nil || storedSecond.Status != "new" || storedSecond.CurrentSummary != "" {
		t.Fatalf("task changed after rollback: %+v, %v", storedSecond, getErr)
	}
	var sideEffects int
	if err := store.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM task_events WHERE run_id = ?) +
		        (SELECT COUNT(*) FROM maintenance_jobs WHERE run_id = ?)`,
		run.ID, run.ID).Scan(&sideEffects); err != nil {
		t.Fatalf("count rollback side effects: %v", err)
	}
	if sideEffects != 0 {
		t.Fatalf("rollback left %d side effects; want 0", sideEffects)
	}
}
