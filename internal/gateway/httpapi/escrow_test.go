package httpapi

// Fix 2: pending-approval/clarify escrow. An approval raised while the CLI is
// attached is suppressed (the inline prompt shows it); if the person then leaves
// the CLI, the periodic escrow pass re-pushes it to the preferred IM exactly
// once so it never sits pending invisibly.

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/delivery"
)

func countKind(msgs []delivery.Message, kind string) int {
	n := 0
	for _, m := range msgs {
		if m.Kind == kind {
			n++
		}
	}
	return n
}

// TestEscrowRepushesApprovalAfterCLIDetaches walks the whole lifecycle: created
// while attached (suppressed, notified_at empty) → presence expires → sweep
// pushes once → second sweep is a no-op.
func TestEscrowRepushesApprovalAfterCLIDetaches(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	// Approval raised while the CLI is attached: the initial push is suppressed
	// and notified_at stays empty.
	daemon.touchPresence(ctx, identity)
	daemon.coordinator().notifyApprovalRequested(ctx, identity, task.ID, "", "session-uuid", approval)
	if len(recorder.messages) != 0 {
		t.Fatalf("attached CLI must suppress the initial push, got %+v", recorder.messages)
	}

	// Threshold not reached yet (huge threshold) → escrow does nothing even once
	// detached.
	daemon.presenceTracker().now = func() time.Time { return time.Now().Add(presenceTTL + time.Second) }
	daemon.sweepPendingNotifications(time.Hour)
	if len(recorder.messages) != 0 {
		t.Fatalf("below-threshold escrow must not push, got %+v", recorder.messages)
	}

	// Threshold reached and CLI detached → exactly one push to the preferred IM.
	daemon.sweepPendingNotifications(time.Millisecond)
	if got := countKind(recorder.messages, delivery.KindApproval); got != 1 {
		t.Fatalf("escrow must push exactly once, got %d approval messages (%+v)", got, recorder.messages)
	}
	if recorder.messages[0].Platform != "weixin" {
		t.Fatalf("escrow must target the preferred IM, got %s", recorder.messages[0].Platform)
	}

	// Second sweep is a no-op (notified_at is stamped).
	daemon.sweepPendingNotifications(time.Millisecond)
	if got := countKind(recorder.messages, delivery.KindApproval); got != 1 {
		t.Fatalf("escrow must be idempotent, got %d approval messages", got)
	}
}

// TestEscrowSkipsWhileCLIAttached: an un-notified pending approval is left alone
// while the person is still at the CLI.
func TestEscrowSkipsWhileCLIAttached(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	daemon.touchPresence(ctx, identity) // attached
	daemon.sweepPendingNotifications(time.Millisecond)
	if len(recorder.messages) != 0 {
		t.Fatalf("attached CLI must suppress escrow, got %+v", recorder.messages)
	}
}

// TestEscrowRepushesClarify: the clarify path escrows exactly like approvals.
func TestEscrowRepushesClarify(t *testing.T) {
	daemon, store, identity, task, approval := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	// Keep the helper approval out of the way so this test isolates clarifies.
	if err := store.MarkApprovalNotified(ctx, identity.TenantID, approval.ID); err != nil {
		t.Fatal(err)
	}
	clarify, err := store.CreateClarifyRequest(ctx, control.ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, Question: "which port?",
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})

	daemon.presenceTracker().now = func() time.Time { return time.Now().Add(presenceTTL + time.Second) } // detached
	daemon.sweepPendingNotifications(time.Millisecond)
	if got := countKind(recorder.messages, delivery.KindClarify); got != 1 {
		t.Fatalf("escrow must push the clarify once, got %d (%+v)", got, recorder.messages)
	}

	// notified_at is set → the question drops out of the escrow scan.
	pending, err := store.ListPendingClarifiesForEscrow(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range pending {
		if c.ID == clarify.ID {
			t.Fatal("escrowed clarify must be marked notified")
		}
	}
}

// TestEscrowDisabledByZeroThreshold: threshold 0 disables the pass entirely.
func TestEscrowDisabledByZeroThreshold(t *testing.T) {
	daemon, store, identity, _, _ := newApprovalTestServer(t)
	ctx := context.Background()
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wxid_123", "Me on WeChat"); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingSender{}
	daemon.Delivery = delivery.NewService(store, recorder, delivery.Options{})
	daemon.presenceTracker().now = func() time.Time { return time.Now().Add(presenceTTL + time.Second) } // detached

	daemon.sweepPendingNotifications(0)
	if len(recorder.messages) != 0 {
		t.Fatalf("threshold 0 must disable escrow, got %+v", recorder.messages)
	}
}
