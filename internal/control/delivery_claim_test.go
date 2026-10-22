package control

import (
	"context"
	"testing"
	"time"
)

// TestClaimDeliveryExactlyOnce encodes the double-dispatch guard: the
// EnqueueAndTry immediate attempt and the retry poller both see a freshly
// enqueued row, and exactly one of them may win the claim. A duplicate claim
// must fail until the attempt is resolved. A crashed claim preserves an
// unknown send effect instead of granting another blind attempt.
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

// A dispatcher crash cannot distinguish before-send from after-acceptance.
// Recovery exposes the uncertainty, without allowing the due loop to resend.
func TestStaleDeliveryClaimRequiresInboundRecovery(t *testing.T) {
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
	if len(due) != 0 {
		t.Fatalf("unknown send effect became a blind retry: %+v", due)
	}
	reclaimed, err := store.ClaimDelivery(ctx, d.ID)
	if err != nil || reclaimed {
		t.Fatalf("unknown send was reclaimed: %v %v", reclaimed, err)
	}
	if err := store.RecoverStaleDeliveryClaims(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := store.DeliveryStatus(ctx, d.ID)
	if err != nil || status != "sent_unconfirmed" {
		t.Fatalf("unknown send was hidden: %s %v", status, err)
	}
	for i := 0; i < 2; i++ {
		claimed, err := store.ClaimDeliveryCatchUp(ctx, d.ID)
		if err != nil || claimed != (i == 0) {
			t.Fatalf("bounded catch-up claim %d: %t %v", i, claimed, err)
		}
	}
}
