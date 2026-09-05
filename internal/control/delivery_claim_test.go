package control

import (
	"context"
	"testing"
	"time"
)

// TestClaimDeliveryExactlyOnce encodes the double-dispatch guard: the
// EnqueueAndTry immediate attempt and the retry poller both see a freshly
// enqueued row, and exactly one of them may win the claim. A duplicate claim
// must fail until the attempt is resolved, and a crashed claim (stale
// 'sending') must become reclaimable so the message is not stranded.
func TestClaimDeliveryExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	d, err := store.EnqueueDelivery(ctx, Delivery{
		TenantID: "default", PersonID: "p1", Platform: "weixin",
		PlatformUserID: "wx-user", Channel: "wx-chat", Content: "approval ping",
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.ClaimDelivery(ctx, d.ID)
	if err != nil || !first {
		t.Fatalf("first claim should win: claimed=%v err=%v", first, err)
	}
	second, err := store.ClaimDelivery(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("second dispatcher must lose the claim while the first is sending")
	}

	// A claimed row is invisible to the due poller (it is being sent).
	due, err := store.ListDueDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("claimed delivery should not be listed as due: %+v", due)
	}

	// Success resolves the claim terminally: no further claims possible.
	if err := store.MarkDeliveryAttempt(ctx, d.ID, true, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	again, err := store.ClaimDelivery(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Fatal("sent delivery must not be claimable")
	}
}

// TestClaimDeliveryStaleReclaim covers dispatcher-crash recovery: a row stuck
// in 'sending' beyond staleSendingSeconds becomes due and claimable again.
func TestClaimDeliveryStaleReclaim(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	d, err := store.EnqueueDelivery(ctx, Delivery{
		TenantID: "default", PersonID: "p1", Platform: "weixin",
		PlatformUserID: "wx-user", Channel: "wx-chat", Content: "ping",
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimDelivery(ctx, d.ID); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}

	// Simulate a dispatcher crash: age the claim past the staleness window.
	if _, err := store.db.ExecContext(ctx,
		`UPDATE outbound_messages SET updated_at = ? WHERE id = ?`,
		time.Now().Unix()-staleSendingSeconds-1, d.ID); err != nil {
		t.Fatal(err)
	}

	due, err := store.ListDueDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != d.ID {
		t.Fatalf("stale sending row should be due again: %+v", due)
	}
	reclaimed, err := store.ClaimDelivery(ctx, d.ID)
	if err != nil || !reclaimed {
		t.Fatalf("stale claim should be reclaimable: %v %v", reclaimed, err)
	}
}
