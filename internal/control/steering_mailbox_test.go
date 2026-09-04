package control

import (
	"context"
	"testing"
	"time"
)

// TestSteeringMailboxLifecycle pins the durability contract: accepted before
// acknowledgement, claimed on channel hand-off, consumed only on kernel
// proof, and consumption matching is per-run + content hash, oldest first.
func TestSteeringMailboxLifecycle(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)

	msg, err := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Channel: "cli",
		Content: "focus on the failing test first",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Status != SteeringAccepted || msg.ContentHash == "" {
		t.Fatalf("accepted row = %+v", msg)
	}
	if err := store.MarkSteeringClaimed(ctx, identity.TenantID, msg.ID); err != nil {
		t.Fatal(err)
	}

	// Consumption is exact by mailbox ID; a wrong ID is a no-op.
	if ok, err := store.ConsumeSteeringByID(ctx, identity.TenantID, run.ID, "steer-missing"); err != nil || ok {
		t.Fatalf("wrong-id consume = %v %v", ok, err)
	}
	if ok, err := store.ConsumeSteeringByID(ctx, identity.TenantID, run.ID, msg.ID); err != nil || !ok {
		t.Fatalf("consume = %v %v", ok, err)
	}
	// Second consume of the same content finds nothing (row already consumed).
	if ok, _ := store.ConsumeSteeringByID(ctx, identity.TenantID, run.ID, msg.ID); ok {
		t.Fatal("consumed row must not be consumable twice")
	}
	if leftovers, err := store.ListUnconsumedSteering(ctx, identity.TenantID, run.ID, 10); err != nil || len(leftovers) != 0 {
		t.Fatalf("unconsumed after consume = %+v err=%v", leftovers, err)
	}
}

// TestSteeringDeferralAndBootRecovery pins the crash-window healing: an
// accepted-but-unconsumed row survives as durable next-turn work without a
// guessed task edge, deferral is idempotent via the queue key, and acknowledged guidance
// never disappears merely because the daemon was offline for a long time.
func TestSteeringDeferralAndBootRecovery(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)

	fresh, err := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Channel: "wechat-room",
		Platform: "weixin", PlatformUserID: "wx-user", WorkspaceID: "ws-1", ApprovalMode: "auto-edit",
		Content: "also update the changelog",
	})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Channel: "telegram-room",
		Platform: "telegram", PlatformUserID: "tg-user", WorkspaceID: "ws-2", ApprovalMode: "read-only",
		Content: "an instruction from last week",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE steering_mailbox SET created_at = ? WHERE id = ?`,
		time.Now().Add(-48*time.Hour).Unix(), stale.ID); err != nil {
		t.Fatal(err)
	}

	deferred, expired, err := store.RecoverSteeringAtBoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deferred != 2 || expired != 0 {
		t.Fatalf("recovery = deferred %d expired %d, want 2/0", deferred, expired)
	}

	// The deferred row became exactly one queued item. Main never saw it, so the
	// queue must not guess that it belongs to the finished task.
	queued, err := store.NextQueued(ctx, identity.TenantID, identity.PersonID)
	if err != nil || queued == nil {
		t.Fatalf("queued: %+v err=%v", queued, err)
	}
	if queued.TaskID != "" || queued.Content != "an instruction from last week" {
		t.Fatalf("queued row = %+v", queued)
	}
	if queued.IdempotencyKey != "steering:"+stale.ID || queued.Platform != "telegram" || queued.PlatformUserID != "tg-user" || queued.WorkspaceID != "ws-2" || queued.ApprovalMode != "read-only" {
		t.Fatalf("idempotency key = %q", queued.IdempotencyKey)
	}
	// Replayed recovery is a no-op: nothing live remains, the queue key blocks
	// a duplicate row.
	if deferred, expired, err := store.RecoverSteeringAtBoot(ctx); err != nil || deferred != 0 || expired != 0 {
		t.Fatalf("second recovery = %d/%d err=%v", deferred, expired, err)
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_queue WHERE idempotency_key LIKE 'steering:%'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("queued steering rows = %d, want 2", rows)
	}
	// The stale accepted row was deferred rather than silently expired.
	var staleStatus string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM steering_mailbox WHERE id = ?`, stale.ID).Scan(&staleStatus); err != nil {
		t.Fatal(err)
	}
	if staleStatus != SteeringDeferred {
		t.Fatalf("stale status = %q", staleStatus)
	}
	_ = fresh
}

func TestIndependentTransferCannotDuplicateFinalizationDeferral(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	msg, err := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Content: "unseen input at finalization",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeferSteering(ctx, *msg); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueueSteeringAsIndependent(ctx, identity.TenantID, identity.PersonID, run.ID, msg.ID); err == nil {
		t.Fatal("a generic finalization deferral must not be reclassified into a second queue row")
	}
	queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, QueueStatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].IdempotencyKey != "steering:"+msg.ID {
		t.Fatalf("queued rows = %+v", queued)
	}
}

func TestSteeringExactConsumptionWithDuplicateContent(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	first, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, TaskID: task.ID, Content: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AcceptSteering(ctx, SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, TaskID: task.ID, Content: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentHash != second.ContentHash {
		t.Fatal("duplicate text must share a hash")
	}
	if ok, err := store.ConsumeSteeringByID(ctx, identity.TenantID, run.ID, second.ID); err != nil || !ok {
		t.Fatalf("consume second = %v %v", ok, err)
	}
	left, err := store.ListUnconsumedSteering(ctx, identity.TenantID, run.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != first.ID {
		t.Fatalf("leftovers = %+v", left)
	}
}

// TestSteeringExpireOnBackpressure: a back-pressure rejection terminates the
// row so it can never replay as a surprise.
func TestSteeringExpireOnBackpressure(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	msg, err := store.AcceptSteering(ctx, SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, TaskID: task.ID, Channel: "cli", Content: "rejected guidance",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSteeringExpired(ctx, identity.TenantID, msg.ID); err != nil {
		t.Fatal(err)
	}
	if deferred, expired, err := store.RecoverSteeringAtBoot(ctx); err != nil || deferred != 0 || expired != 0 {
		t.Fatalf("expired row leaked into recovery: %d/%d err=%v", deferred, expired, err)
	}
}
