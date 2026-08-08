package control

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestBindQueuedRunAllowsOnlyOneConcurrentLauncher(t *testing.T) {
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
		Content: "launch once", Platform: "cli", PlatformUserID: "gnfy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, row.ID, QueueStatusStarted); err != nil {
		t.Fatal(err)
	}

	const launchers = 16
	results := make(chan struct {
		runID string
		err   error
	}, launchers)
	var wg sync.WaitGroup
	for i := 0; i < launchers; i++ {
		runID := fmt.Sprintf("run_%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- struct {
				runID string
				err   error
			}{runID: runID, err: store.BindQueuedRun(ctx, identity.TenantID, row.ID, runID)}
		}()
	}
	wg.Wait()
	close(results)

	winner := ""
	for result := range results {
		if result.err == nil {
			if winner != "" {
				t.Fatalf("multiple launchers bound the queue row: %s and %s", winner, result.runID)
			}
			winner = result.runID
		}
	}
	if winner == "" {
		t.Fatal("no launcher bound the started queue row")
	}
	got, err := store.GetQueued(ctx, identity.TenantID, row.ID)
	if err != nil || got == nil || got.RunID != winner {
		t.Fatalf("bound row = %+v, %v; want winner %s", got, err, winner)
	}
	if err := store.BindQueuedRun(ctx, identity.TenantID, row.ID, winner); err != nil {
		t.Fatalf("idempotent winner rebind failed: %v", err)
	}
}

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

func TestTaskQueueUsesClassPriorityAndNotBefore(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	enqueue := func(content, class string, notBefore time.Time) *QueuedTask {
		t.Helper()
		q, err := store.EnqueueQueued(ctx, QueuedTask{
			TenantID: identity.TenantID, PersonID: identity.PersonID,
			Platform: "cli", Channel: "cli", Content: content,
			Class: class, NotBefore: notBefore,
		})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	enqueue("cron first", QueueClassCron, time.Time{})
	foreground := enqueue("foreground later", QueueClassForeground, time.Time{})
	enqueue("delayed foreground", QueueClassForeground, time.Now().Add(time.Hour))

	rows, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, QueueStatusQueued)
	if err != nil || len(rows) != 3 {
		t.Fatalf("ListQueued = %d, %v", len(rows), err)
	}
	if rows[0].ID != foreground.ID || rows[0].Priority != QueuePriorityForeground {
		t.Fatalf("scheduler order = %+v", rows)
	}
	next, err := store.NextQueued(ctx, identity.TenantID, identity.PersonID)
	if err != nil || next == nil || next.ID != foreground.ID {
		t.Fatalf("NextQueued = %+v, %v", next, err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, foreground.ID, QueueStatusStarted); err != nil {
		t.Fatal(err)
	}
	next, err = store.NextQueued(ctx, identity.TenantID, identity.PersonID)
	if err != nil || next == nil || next.Content != "cron first" {
		t.Fatalf("delayed row became runnable: %+v, %v", next, err)
	}
}

func TestQueueIdempotencyIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: "tenant-a", PersonID: "person-a", Platform: "cli", Channel: "cli",
		Content: "first tenant", IdempotencyKey: "shared-effect",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: "tenant-b", PersonID: "person-b", Platform: "cli", Channel: "cli",
		Content: "second tenant", IdempotencyKey: "shared-effect",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.TenantID == second.TenantID {
		t.Fatalf("tenant-scoped queue keys collided: first=%+v second=%+v", first, second)
	}
	replayed, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: "tenant-b", PersonID: "person-b", Platform: "cli", Channel: "cli",
		Content: "ignored replay", IdempotencyKey: "shared-effect",
	})
	if err != nil || replayed.ID != second.ID || replayed.Content != second.Content {
		t.Fatalf("tenant replay = %+v, %v; want existing tenant-b row %+v", replayed, err, second)
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
	if err := store.MarkQueued(ctx, identity.TenantID, system.ID, QueueStatusFailed); err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 2; want++ {
		requeued, err := store.RequeueSystemQueued(ctx, identity.TenantID, system.ID, 2)
		if err != nil || !requeued {
			t.Fatalf("requeue %d = %v, %v", want, requeued, err)
		}
		row, err := store.GetQueuedByIdempotencyKey(ctx, system.TenantID, system.IdempotencyKey)
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

func TestQueueClaimLeasePreventsLiveSystemRequeue(t *testing.T) {
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
		Content: "finalize", IdempotencyKey: "external-watch:lease:r1:finalization",
	})
	if err != nil {
		t.Fatal(err)
	}
	token, claimed, err := store.ClaimQueued(ctx, identity.TenantID, row.ID, time.Hour)
	if err != nil || !claimed || token == "" {
		t.Fatalf("claim = %q/%v, %v", token, claimed, err)
	}
	if err := store.BindQueuedRunClaimed(ctx, identity.TenantID, row.ID, "run_live", token); err != nil {
		t.Fatal(err)
	}
	if requeued, err := store.RequeueSystemQueued(ctx, identity.TenantID, row.ID, 3); err != nil || requeued {
		t.Fatalf("live leased row requeue = %v, %v; want false", requeued, err)
	}
	stored, err := store.GetQueued(ctx, identity.TenantID, row.ID)
	if err != nil || stored == nil || stored.Status != QueueStatusStarted || stored.AttemptGeneration != 1 {
		t.Fatalf("leased row = %+v, %v", stored, err)
	}
}

func TestQueueClaimRejectsStaleWorker(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "gnfy", "Alice")
	row, _ := store.EnqueueQueued(ctx, QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID, Content: "work"})
	token, claimed, err := store.ClaimQueued(ctx, identity.TenantID, row.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if err := store.BindQueuedRunClaimed(ctx, identity.TenantID, row.ID, "run_1", "stale-token"); err == nil {
		t.Fatal("stale worker unexpectedly bound the queue row")
	}
	if err := store.BindQueuedRunClaimed(ctx, identity.TenantID, row.ID, "run_1", token); err != nil {
		t.Fatal(err)
	}
}

func TestDoneSystemQueueOnlyRequeuesWithoutSuccessfulRunEvent(t *testing.T) {
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
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "finalization",
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: "cli-session", Platform: "cli", Content: "finalize",
		IdempotencyKey: "external-watch:done:r1:finalization",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, row.ID, QueueStatusStarted); err != nil {
		t.Fatal(err)
	}
	if err := store.BindQueuedRun(ctx, identity.TenantID, row.ID, "run_incomplete"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, row.ID, QueueStatusDone); err != nil {
		t.Fatal(err)
	}
	if requeued, err := store.RequeueDoneSystemQueuedIfUnmaterialized(ctx, identity.TenantID, row.ID, 2); err != nil || !requeued {
		t.Fatalf("incomplete done row requeue = %v, %v; want true", requeued, err)
	}

	if err := store.MarkQueued(ctx, identity.TenantID, row.ID, QueueStatusStarted); err != nil {
		t.Fatal(err)
	}
	if err := store.BindQueuedRun(ctx, identity.TenantID, row.ID, "run_complete"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: "run_complete", Type: "run.finished"}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, row.ID, QueueStatusDone); err != nil {
		t.Fatal(err)
	}
	if requeued, err := store.RequeueDoneSystemQueuedIfUnmaterialized(ctx, identity.TenantID, row.ID, 2); err != nil || requeued {
		t.Fatalf("completed done row requeue = %v, %v; want false", requeued, err)
	}
}

func TestRequeueStartedQueuedSettlesMaterializedRunWithoutReplay(t *testing.T) {
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
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "finalization",
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: "cli-session", Platform: "cli", Content: "finalize",
		IdempotencyKey: "external-watch:started:r1:finalization",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, row.ID, QueueStatusStarted); err != nil {
		t.Fatal(err)
	}
	if err := store.BindQueuedRun(ctx, identity.TenantID, row.ID, "run_materialized"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: "run_materialized", Type: "run.finished"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO effect_receipts
		(effect_key, tenant_id, task_id, run_id, kind, delivery_enqueued, created_at)
		VALUES (?, ?, ?, ?, 'run_finalization', 1, ?)`,
		row.IdempotencyKey, identity.TenantID, task.ID, "run_materialized", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	requeued, dropped, err := store.RequeueStartedQueued(ctx)
	if err != nil || requeued != 0 || dropped != 0 {
		t.Fatalf("reconcile materialized started row = %d/%d, %v; want 0/0", requeued, dropped, err)
	}
	got, err := store.GetQueued(ctx, identity.TenantID, row.ID)
	if err != nil || got == nil || got.Status != QueueStatusDone || got.Restarts != 0 || got.RunID != "run_materialized" {
		t.Fatalf("materialized row = %+v, %v; want done without restart", got, err)
	}
}

func TestRequeueStartedQueuedReplaysUntilFinalResultIsDurable(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "gnfy", "Alice")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "finalization"})
	row, err := store.EnqueueQueued(ctx, QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Channel: "cli", Platform: "cli",
		Content: "finalize", IdempotencyKey: "external-watch:outbox:r1:finalization",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, row.ID, QueueStatusStarted); err != nil {
		t.Fatal(err)
	}
	if err := store.BindQueuedRun(ctx, identity.TenantID, row.ID, "run_materialized"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: "run_materialized", Type: "run.finished"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO effect_receipts
		(effect_key, tenant_id, task_id, run_id, kind, delivery_enqueued, created_at)
		VALUES (?, ?, ?, ?, 'run_finalization', 0, ?)`,
		row.IdempotencyKey, identity.TenantID, task.ID, "run_materialized", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	requeued, dropped, err := store.RequeueStartedQueued(ctx)
	if err != nil || requeued != 1 || dropped != 0 {
		t.Fatalf("reconcile pending outbox row = %d/%d, %v; want 1/0", requeued, dropped, err)
	}
	got, err := store.GetQueued(ctx, identity.TenantID, row.ID)
	if err != nil || got == nil || got.Status != QueueStatusQueued || got.RunID != "" {
		t.Fatalf("pending outbox row = %+v, %v; want queued for replay", got, err)
	}
}

func TestEffectReceiptsAreTenantScoped(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO effect_receipts
			(effect_key, tenant_id, task_id, run_id, kind, delivery_enqueued, created_at)
			VALUES ('same-effect', ?, 'task', 'run', 'test', 0, ?)`, tenant, time.Now().Unix()); err != nil {
			t.Fatalf("insert receipt for %s: %v", tenant, err)
		}
	}
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		owned, err := store.EffectOwnedByRun(ctx, tenant, "same-effect", "run")
		if err != nil || !owned {
			t.Fatalf("tenant %s receipt = %v, %v", tenant, owned, err)
		}
	}
}

func TestRequeueStartedQueuedReopensFailedBoundRun(t *testing.T) {
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
		Channel: "cli-session", Platform: "cli", Content: "retry",
		IdempotencyKey: "external-watch:failed:r1:finalization",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, row.ID, QueueStatusStarted); err != nil {
		t.Fatal(err)
	}
	if err := store.BindQueuedRun(ctx, identity.TenantID, row.ID, "run_failed"); err != nil {
		t.Fatal(err)
	}

	requeued, dropped, err := store.RequeueStartedQueued(ctx)
	if err != nil || requeued != 1 || dropped != 0 {
		t.Fatalf("reconcile failed started row = %d/%d, %v; want 1/0", requeued, dropped, err)
	}
	got, err := store.GetQueued(ctx, identity.TenantID, row.ID)
	if err != nil || got == nil || got.Status != QueueStatusQueued || got.Restarts != 1 || got.RunID != "" {
		t.Fatalf("reopened row = %+v, %v; want queued with cleared run", got, err)
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
