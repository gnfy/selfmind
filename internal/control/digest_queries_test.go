package control

// Store queries backing the attach digest (G0-c): tasks that finished or
// stopped early since an anchor, and outbound pushes that never confirmed.

import (
	"context"
	"testing"
	"time"
)

func TestListTaskRunTransitionsSinceUsesRunClock(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}

	type terminal struct {
		task *Task
		run  *Run
	}
	mkTerminal := func(title, status string) terminal {
		t.Helper()
		task, err := store.CreateTask(ctx, TaskCreate{
			TenantID: identity.TenantID,
			PersonID: identity.PersonID,
			Title:    title,
			Channel:  "cli",
		})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, "cli", title)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
			Identity: *identity, RunID: run.ID, RunStatus: status,
			TaskID: task.ID, TaskStatus: status, Summary: "summary of " + title,
		}); err != nil {
			t.Fatal(err)
		}
		return terminal{task: task, run: run}
	}

	since := time.Now().Add(-time.Hour)
	finished := mkTerminal("Ship the report", "done")
	interrupted := mkTerminal("Refactor the parser", "interrupted")
	oldFinished := mkTerminal("Ancient chore", "done")
	for _, item := range []struct {
		run  *Run
		when time.Time
	}{
		{finished.run, since.Add(2 * time.Minute)},
		{interrupted.run, since.Add(3 * time.Minute)},
		{oldFinished.run, since.Add(-time.Minute)},
	} {
		if _, err := store.db.ExecContext(ctx, `UPDATE runs SET finished_at = ? WHERE id = ?`, item.when.Unix(), item.run.ID); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ListTaskRunTransitionsSince(ctx, identity.TenantID, identity.PersonID, []string{"done", "completed"}, since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TaskID != finished.task.ID {
		t.Fatalf("finished-since query returned %+v, want only %q", got, finished.task.ID)
	}
	if got[0].Summary != "summary of Ship the report" {
		t.Fatalf("summary not loaded: %+v", got[0])
	}

	got, err = store.ListTaskRunTransitionsSince(ctx, identity.TenantID, identity.PersonID, []string{"failed", "interrupted"}, since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TaskID != interrupted.task.ID {
		t.Fatalf("disrupted-since query returned %+v, want only %q", got, interrupted.task.ID)
	}

	// When a task has more than one terminal run in the window, the latest
	// transition wins across status classes so a recovered task is not reported
	// as both finished and interrupted.
	retry, err := store.StartRun(ctx, interrupted.task, "cli", "retry parser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: retry.ID, RunStatus: "done",
		TaskID: interrupted.task.ID, TaskStatus: "done", Summary: "retry finished",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET finished_at = ? WHERE id = ?`, since.Add(4*time.Minute).Unix(), retry.ID); err != nil {
		t.Fatal(err)
	}
	got, err = store.ListTaskRunTransitionsSince(ctx, identity.TenantID, identity.PersonID, []string{"done", "interrupted"}, since, 10)
	if err != nil {
		t.Fatal(err)
	}
	var parserTransitions []TaskRunTransition
	for _, transition := range got {
		if transition.TaskID == interrupted.task.ID {
			parserTransitions = append(parserTransitions, transition)
		}
	}
	if len(parserTransitions) != 1 || parserTransitions[0].Status != "done" {
		t.Fatalf("latest transition must win across statuses: %+v", parserTransitions)
	}

	// Bounded: a limit of 1 with two qualifying rows returns exactly one.
	mkTerminal("Second finished", "done")
	got, err = store.ListTaskRunTransitionsSince(ctx, identity.TenantID, identity.PersonID, []string{"done", "completed"}, since, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("limit not applied: %d rows", len(got))
	}

	// No statuses means nothing to ask for.
	if got, err := store.ListTaskRunTransitionsSince(ctx, identity.TenantID, identity.PersonID, nil, since, 10); err != nil || got != nil {
		t.Fatalf("empty status list should return nil, nil; got %+v, %v", got, err)
	}
}

func TestListTasksByStatusReturnsCurrentUnresolvedState(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Resume parser", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "older interruption")
	if err != nil {
		t.Fatal(err)
	}
	// An interruption is unresolved state only when the run carried real work;
	// a durable plan is that evidence.
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "parse resumes", []RunPlanStepInput{{Step: "read the resume", Status: "completed"}, {Step: "parse the resume", Status: "in_progress"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, TaskID: task.ID,
		RunStatus: "interrupted", Summary: "older interruption",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListTasksByStatus(ctx, identity.TenantID, identity.PersonID, []string{"failed", "interrupted"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != task.ID || got[0].CurrentSummary != "older interruption" {
		t.Fatalf("unresolved task query returned %+v", got)
	}
}

func TestListUndeliveredOutbound(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}

	enqueue := func(content string) *Delivery {
		t.Helper()
		d, err := store.EnqueueDelivery(ctx, Delivery{
			TenantID: identity.TenantID,
			PersonID: identity.PersonID,
			Platform: "weixin",
			Channel:  "weixin",
			Content:  content,
		})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	unconfirmed := enqueue("build finished — see the report")
	failed := enqueue("approval needed")
	delivered := enqueue("all good")
	stale := enqueue("from another era")

	if err := store.MarkDeliverySentUnconfirmed(ctx, unconfirmed.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliveryFailedPermanent(ctx, failed.ID, "no sender for platform"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliveryAttempt(ctx, delivered.ID, true, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	// A stale unconfirmed push older than the anchor must not resurface.
	if err := store.MarkDeliverySentUnconfirmed(ctx, stale.ID); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Hour)
	if _, err := store.db.ExecContext(ctx, `UPDATE outbound_messages SET updated_at = ? WHERE id = ?`,
		since.Add(-time.Minute).Unix(), stale.ID); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListUndeliveredOutbound(ctx, identity.TenantID, identity.PersonID, since, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("undelivered rows = %d (%+v), want 2", len(got), got)
	}
	byID := map[string]Delivery{}
	for _, d := range got {
		byID[d.ID] = d
	}
	if d, ok := byID[unconfirmed.ID]; !ok || d.Status != "sent_unconfirmed" {
		t.Fatalf("sent_unconfirmed row missing/incorrect: %+v", got)
	}
	if d, ok := byID[failed.ID]; !ok || d.Status != "failed" {
		t.Fatalf("failed row missing/incorrect: %+v", got)
	}

	// Bounded.
	got, err = store.ListUndeliveredOutbound(ctx, identity.TenantID, identity.PersonID, since, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("limit not applied: %d rows", len(got))
	}
}
