package control

// Store queries backing the attach digest (G0-c): tasks that finished or
// stopped early since an anchor, and outbound pushes that never confirmed.

import (
	"context"
	"testing"
	"time"
)

func TestListTasksByStatusSince(t *testing.T) {
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

	mkTask := func(title, status string) *Task {
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
		if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, status, "summary of "+title, nil); err != nil {
			t.Fatal(err)
		}
		return task
	}

	finished := mkTask("Ship the report", "completed")
	interrupted := mkTask("Refactor the parser", "interrupted")
	mkTask("Still working", "in_progress")
	oldFinished := mkTask("Ancient chore", "completed")

	// Backdate the ancient task past the anchor; the anchor filter must drop it.
	since := time.Now().Add(-time.Hour)
	if _, err := store.db.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`,
		since.Add(-time.Minute).Unix(), oldFinished.ID); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListTasksByStatusSince(ctx, identity.TenantID, identity.PersonID, []string{"done", "completed"}, since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != finished.ID {
		t.Fatalf("finished-since query returned %+v, want only %q", got, finished.ID)
	}
	if got[0].CurrentSummary != "summary of Ship the report" {
		t.Fatalf("summary not loaded: %+v", got[0])
	}

	got, err = store.ListTasksByStatusSince(ctx, identity.TenantID, identity.PersonID, []string{"failed", "interrupted"}, since, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != interrupted.ID {
		t.Fatalf("disrupted-since query returned %+v, want only %q", got, interrupted.ID)
	}

	// Bounded: a limit of 1 with two qualifying rows returns exactly one.
	mkTask("Second finished", "done")
	got, err = store.ListTasksByStatusSince(ctx, identity.TenantID, identity.PersonID, []string{"done", "completed"}, since, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("limit not applied: %d rows", len(got))
	}

	// No statuses means nothing to ask for.
	if got, err := store.ListTasksByStatusSince(ctx, identity.TenantID, identity.PersonID, nil, since, 10); err != nil || got != nil {
		t.Fatalf("empty status list should return nil, nil; got %+v, %v", got, err)
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
