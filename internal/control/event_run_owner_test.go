package control

import (
	"context"
	"testing"
	"time"
)

// TestAppendEventResolvesOwnerFromRun pins the event-ownership authority. Every
// surface builds its approval and clarification panels from these events, so a
// caller holding only a Run must be able to emit one: gating emission on a Task
// meant the durable approval row was created, nobody was notified, and the TUI
// rendered "Running" over work that was actually waiting on a human.
func TestAppendEventResolvesOwnerFromRun(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	thread, err := store.CreateTask(ctx, TaskCreate{
		TenantID: "default", PersonID: "p1", Title: "release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread, "cli", "deploy")
	if err != nil {
		t.Fatal(err)
	}

	// The caller has a Run and no Task id — the normal case now that Runs
	// execute without one.
	event, err := store.AppendEvent(ctx, Event{
		RunID:      run.ID,
		Type:       "approval.requested",
		Visibility: "task",
		Channel:    "cli",
		Payload:    []byte(`{"approval_id":"apr_x"}`),
	})
	if err != nil {
		t.Fatalf("run-only approval event was dropped: %v", err)
	}
	if event.TaskID != thread.ID {
		t.Fatalf("thread = %q, want the run's thread %q", event.TaskID, thread.ID)
	}
	if event.TenantID != "default" || event.PersonID != "p1" {
		t.Fatalf("identity = %q/%q, want default/p1", event.TenantID, event.PersonID)
	}

	// And it must be readable back on the person's stream, which is what the
	// TUI and /status subscribe to.
	events, err := store.ListPersonEventsSince(ctx, "default", "p1", time.Time{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == "approval.requested" && e.RunID == run.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("approval.requested is not on the person event stream")
	}
}

// TestAppendEventRefusesWithoutAnyOwner: dropping the Task gate must not turn
// into an unowned-event bucket. `thread_id = ”` is a collision bucket that
// person-scoped queries do not filter, so refusing loudly beats writing one.
func TestAppendEventRefusesWithoutAnyOwner(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if _, err := store.AppendEvent(ctx, Event{Type: "approval.requested"}); err == nil {
		t.Fatal("an event with neither a thread nor a run should be refused")
	}
	if _, err := store.AppendEvent(ctx, Event{RunID: "run_missing", Type: "approval.requested"}); err == nil {
		t.Fatal("an event naming an unknown run should be refused")
	}
}

// TestAppendEventCorrectsAStaleScopeThread replays the live failure that made a
// production release look hung: a tool scope frozen BEFORE a continuation claim
// still names the interaction placeholder the claim superseded, so the caller
// hands AppendEvent a thread id that no longer exists. Trusting it meant the
// threads lookup returned no rows and the approval event — the one every
// surface builds its panel from — was dropped, three times in one run, while
// the TUI kept rendering "Running".
func TestAppendEventCorrectsAStaleScopeThread(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	thread, err := store.CreateTask(ctx, TaskCreate{
		TenantID: "default", PersonID: "p1", Title: "release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, thread, "cli", "deploy")
	if err != nil {
		t.Fatal(err)
	}

	event, err := store.AppendEvent(ctx, Event{
		TaskID:     "task_superseded-placeholder",
		RunID:      run.ID,
		Type:       "approval.requested",
		Visibility: "task",
		Channel:    "cli",
		Payload:    []byte(`{"approval_id":"apr_x"}`),
	})
	if err != nil {
		t.Fatalf("a stale scope thread dropped the approval event: %v", err)
	}
	if event.TaskID != thread.ID {
		t.Fatalf("thread = %q, want the run's own thread %q", event.TaskID, thread.ID)
	}
}
