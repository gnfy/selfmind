package control

import (
	"context"
	"testing"
)

func TestTaskQueueLifecycle(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	tenant, person := identity.TenantID, identity.PersonID

	enqueue := func(content string) *QueuedTask {
		t.Helper()
		q, err := store.EnqueueQueued(ctx, QueuedTask{
			TenantID: tenant, PersonID: person, Channel: "cli", Platform: "cli",
			PlatformUserID: "local", Content: content,
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		return q
	}

	first := enqueue("first task")
	enqueue("second task")

	// FIFO ordering.
	queued, err := store.ListQueued(ctx, tenant, person, QueueStatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 2 || queued[0].Content != "first task" || queued[1].Content != "second task" {
		t.Fatalf("ListQueued order wrong: %+v", queued)
	}

	if n, err := store.CountQueued(ctx, tenant, person, QueueStatusQueued); err != nil || n != 2 {
		t.Fatalf("CountQueued = %d, %v; want 2", n, err)
	}

	// NextQueued returns the oldest.
	next, err := store.NextQueued(ctx, tenant, person)
	if err != nil || next == nil || next.ID != first.ID {
		t.Fatalf("NextQueued = %+v, %v; want %s", next, err, first.ID)
	}

	// Marking it started removes it from the queued view.
	if err := store.MarkQueued(ctx, tenant, first.ID, QueueStatusStarted); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.CountQueued(ctx, tenant, person, QueueStatusQueued); n != 1 {
		t.Fatalf("after MarkStarted queued count = %d; want 1", n)
	}
	if n, _ := store.CountQueued(ctx, tenant, person, QueueStatusStarted); n != 1 {
		t.Fatalf("started count = %d; want 1", n)
	}

	// RequeueStartedQueued flips it back (boot recovery), consuming its one
	// restart credit.
	if n, dropped, err := store.RequeueStartedQueued(ctx); err != nil || n != 1 || dropped != 0 {
		t.Fatalf("RequeueStartedQueued = %d/%d, %v; want 1/0", n, dropped, err)
	}
	if n, _ := store.CountQueued(ctx, tenant, person, QueueStatusQueued); n != 2 {
		t.Fatalf("after requeue queued count = %d; want 2", n)
	}

	// ClearQueued cancels all remaining.
	cleared, err := store.ClearQueued(ctx, tenant, person)
	if err != nil || cleared != 2 {
		t.Fatalf("ClearQueued = %d, %v; want 2", cleared, err)
	}
	if n, _ := store.CountQueued(ctx, tenant, person, QueueStatusQueued); n != 0 {
		t.Fatalf("after clear queued count = %d; want 0", n)
	}
	if next, _ := store.NextQueued(ctx, tenant, person); next != nil {
		t.Fatalf("NextQueued after clear = %+v; want nil", next)
	}
}

func TestListAllQueuedAcrossPersons(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	a, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	b, _ := store.ResolveOrCreateAccount(ctx, "default", "telegram", "tg_bob", "Bob")
	if _, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: a.TenantID, PersonID: a.PersonID, Platform: "cli", PlatformUserID: "local", Content: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: b.TenantID, PersonID: b.PersonID, Platform: "telegram", PlatformUserID: "tg_bob", Content: "b"}); err != nil {
		t.Fatal(err)
	}
	all, err := store.ListAllQueued(ctx, QueueStatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListAllQueued = %d rows; want 2", len(all))
	}
}

func TestEnqueueQueuedOnlyIgnoresIdempotencyConflict(t *testing.T) {
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
	base := QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: "cli", Platform: "cli", Content: "finalize",
		IdempotencyKey: "watch:one:finalize",
	}
	first, err := store.EnqueueQueued(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.EnqueueQueued(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("duplicate enqueue returned %q, want existing %q", second.ID, first.ID)
	}

	// A primary-key collision without an idempotency key is a real storage
	// error. It must never be hidden as a successful enqueue.
	fixed := QueuedTask{
		ID: "queue_fixed", TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: "cli", Platform: "cli", Content: "ordinary",
	}
	if _, err := store.EnqueueQueued(ctx, fixed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueQueued(ctx, fixed); err == nil {
		t.Fatal("duplicate primary key without idempotency key must fail")
	}
}

func TestRequeueSystemQueuedIsBoundedAndLeavesOrdinaryRowsAlone(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "gnfy", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	system, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: "cli-session", Platform: "cli", Content: "finalize",
		IdempotencyKey: "external-watch:test:r1:finalization",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, system.ID, QueueStatusDone); err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 2; want++ {
		requeued, err := store.RequeueSystemQueued(ctx, identity.TenantID, system.ID, 2)
		if err != nil || !requeued {
			t.Fatalf("requeue %d = %v, %v", want, requeued, err)
		}
		row, err := store.GetQueuedByIdempotencyKey(ctx, system.IdempotencyKey)
		if err != nil || row == nil || row.Status != QueueStatusQueued || row.Restarts != want {
			t.Fatalf("row after requeue %d = %+v, %v", want, row, err)
		}
		if err := store.MarkQueued(ctx, identity.TenantID, system.ID, QueueStatusFailed); err != nil {
			t.Fatal(err)
		}
	}
	if requeued, err := store.RequeueSystemQueued(ctx, identity.TenantID, system.ID, 2); err != nil || requeued {
		t.Fatalf("exhausted row requeue = %v, %v; want false", requeued, err)
	}

	ordinary, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: "cli", Platform: "cli", Content: "ordinary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, ordinary.ID, QueueStatusDone); err != nil {
		t.Fatal(err)
	}
	if requeued, err := store.RequeueSystemQueued(ctx, identity.TenantID, ordinary.ID, 2); err != nil || requeued {
		t.Fatalf("ordinary row requeue = %v, %v; want false", requeued, err)
	}
}

func TestUpdateSystemQueuedContentOnlyTouchesDurableSystemRows(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "gnfy", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	system, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Content: "old system prompt", IdempotencyKey: "system:refresh",
	})
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Content: "user message",
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := store.UpdateSystemQueuedContent(ctx, identity.TenantID, system.ID, "current system prompt")
	if err != nil || !updated {
		t.Fatalf("system refresh: updated=%v err=%v", updated, err)
	}
	row, err := store.GetQueued(ctx, identity.TenantID, system.ID)
	if err != nil || row == nil || row.Content != "current system prompt" {
		t.Fatalf("system row: row=%+v err=%v", row, err)
	}
	updated, err = store.UpdateSystemQueuedContent(ctx, identity.TenantID, ordinary.ID, "must not replace user input")
	if err != nil || updated {
		t.Fatalf("ordinary refresh: updated=%v err=%v", updated, err)
	}
	row, err = store.GetQueued(ctx, identity.TenantID, ordinary.ID)
	if err != nil || row == nil || row.Content != "user message" {
		t.Fatalf("ordinary row changed: row=%+v err=%v", row, err)
	}
}

func TestMarkQueuedIfStatusDoesNotOverwriteRecoveryTransition(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "gnfy", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	row, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Content: "recover me", Platform: "cli", PlatformUserID: "gnfy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, row.ID, QueueStatusStarted); err != nil {
		t.Fatal(err)
	}
	if changed, err := store.MarkQueuedIfStatus(ctx, identity.TenantID, row.ID, QueueStatusStarted, QueueStatusQueued); err != nil || !changed {
		t.Fatalf("restart transition = %v, %v; want true", changed, err)
	}
	// The old run's deferred completion races after shutdown. It must not turn
	// the explicitly reopened row back into done.
	if changed, err := store.MarkQueuedIfStatus(ctx, identity.TenantID, row.ID, QueueStatusStarted, QueueStatusDone); err != nil || changed {
		t.Fatalf("stale completion transition = %v, %v; want false", changed, err)
	}
	got, err := store.GetQueued(ctx, identity.TenantID, row.ID)
	if err != nil || got == nil || got.Status != QueueStatusQueued {
		t.Fatalf("queue after race = %+v, %v", got, err)
	}
}
