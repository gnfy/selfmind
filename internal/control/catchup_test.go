package control

import (
	"context"
	"testing"
	"time"
)

func seedUnconfirmed(t *testing.T, s *Store, personID, channel, content string) Delivery {
	t.Helper()
	ctx := context.Background()
	d, err := s.EnqueueDelivery(ctx, Delivery{
		TenantID: "default", PersonID: personID, Platform: "weixin",
		PlatformUserID: "wx-user", Channel: channel, Content: content,
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimDelivery(ctx, d.ID); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := s.MarkDeliverySentUnconfirmed(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	return *d
}

func seedPendingSession(t *testing.T, s *Store, personID, channel, content string, maxAttempts int) Delivery {
	t.Helper()
	ctx := context.Background()
	d, err := s.EnqueueDelivery(ctx, Delivery{
		TenantID: "default", PersonID: personID, Platform: "weixin",
		PlatformUserID: "wx-user", Channel: channel, Content: content,
		MaxAttempts: maxAttempts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDeliveryPendingSession(ctx, d.ID, "fresh session required"); err != nil {
		t.Fatal(err)
	}
	return *d
}

func TestDeliveryHealthByPlatformSeparatesTransportStates(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	unconfirmed := seedUnconfirmed(t, store, "p1", "wx", "maybe")
	_ = unconfirmed
	sent, err := store.EnqueueDelivery(ctx, Delivery{TenantID: "default", PersonID: "p1", Platform: "telegram", Channel: "tg", Content: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, _ := store.ClaimDelivery(ctx, sent.ID); !claimed {
		t.Fatal("claim sent delivery")
	}
	if err := store.MarkDeliveryAttempt(ctx, sent.ID, true, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	health, err := store.DeliveryHealthByPlatformSince(ctx, "default", "p1", time.Now().Add(-time.Hour))
	if err != nil || len(health) != 2 {
		t.Fatalf("health = %+v, %v", health, err)
	}
	byPlatform := map[string]DeliveryPlatformHealth{}
	for _, item := range health {
		byPlatform[item.Platform] = item
	}
	if byPlatform["weixin"].Unconfirmed != 1 || byPlatform["telegram"].Sent != 1 {
		t.Fatalf("health = %+v", health)
	}
}

func TestLatestDeliveryEndpointStateIsEndpointScoped(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	one, err := store.EnqueueDelivery(ctx, Delivery{
		TenantID: "default", PersonID: "p1", Platform: "weixin", PlatformUserID: "wx-1", Channel: "wx-1", Content: "one",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliveryPendingSession(ctx, one.ID, "prepare failed"); err != nil {
		t.Fatal(err)
	}
	other, err := store.EnqueueDelivery(ctx, Delivery{
		TenantID: "default", PersonID: "p1", Platform: "weixin", PlatformUserID: "wx-2", Channel: "wx-2", Content: "two",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliveryAttempt(ctx, other.ID, true, "", time.Time{}); err != nil {
		t.Fatal(err)
	}

	state, err := store.LatestDeliveryEndpointState(ctx, "default", "p1", "weixin", "wx-1")
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Status != "pending_session" {
		t.Fatalf("state = %+v, want pending_session", state)
	}
	missing, err := store.LatestDeliveryEndpointState(ctx, "default", "p1", "weixin", "wx-missing")
	if err != nil || missing != nil {
		t.Fatalf("missing state = %+v, err = %v", missing, err)
	}
}

// TestCatchUpEligibilityAndClaim pins the store-side anti-duplicate rails:
// oldest-first ordering, the one-shot claim, and the freshness window.
func TestCatchUpEligibilityAndClaim(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	first := seedUnconfirmed(t, s, "p1", "wx-chat", "older notice")
	time.Sleep(1100 * time.Millisecond) // created_at is unix-seconds; force distinct ordering
	second := seedUnconfirmed(t, s, "p1", "wx-chat", "newer notice")

	since := time.Now().Add(-time.Hour)
	rows, err := s.ListCatchUpEligible(ctx, "default", "p1", "weixin", "wx-user", "wx-chat", since, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != first.ID || rows[1].ID != second.ID {
		t.Fatalf("want oldest-first [%s %s], got %+v", first.ID, second.ID, rows)
	}

	// Channel scoping: a different chat sees nothing.
	if rows, _ := s.ListCatchUpEligible(ctx, "default", "p1", "weixin", "wx-user", "other-chat", since, 5); len(rows) != 0 {
		t.Fatalf("different channel must not match: %+v", rows)
	}

	// One-shot claim: first wins, second loses, and the row leaves eligibility.
	if ok, err := s.ClaimDeliveryCatchUp(ctx, first.ID); err != nil || !ok {
		t.Fatalf("first claim should win: %v %v", ok, err)
	}
	if ok, _ := s.ClaimDeliveryCatchUp(ctx, first.ID); ok {
		t.Fatal("second claim on the same row must lose (at-most-once rail)")
	}
	rows, _ = s.ListCatchUpEligible(ctx, "default", "p1", "weixin", "wx-user", "wx-chat", since, 5)
	if len(rows) != 1 || rows[0].ID != second.ID {
		t.Fatalf("claimed row must leave eligibility, got %+v", rows)
	}

	// Freshness window: age the remaining row past the window; it disappears.
	if _, err := s.db.Exec(`UPDATE outbound_messages SET updated_at = ? WHERE id = ?`,
		time.Now().Add(-8*time.Hour).Unix(), second.ID); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.ListCatchUpEligible(ctx, "default", "p1", "weixin", "wx-user", "wx-chat", time.Now().Add(-4*time.Hour), 5); len(rows) != 0 {
		t.Fatalf("stale row must not be re-pushed: %+v", rows)
	}

	// A 'sent' row is never eligible even without a claim.
	third := seedUnconfirmed(t, s, "p1", "wx-chat", "will confirm")
	if err := s.MarkDeliveryAttempt(ctx, third.ID, true, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := s.ListCatchUpEligible(ctx, "default", "p1", "weixin", "wx-user", "wx-chat", since, 5); len(rows) != 0 {
		t.Fatalf("sent rows must not be eligible: %+v", rows)
	}
}

// TestCountOutboundByStatusSince backs the /diag outbound-health section.
func TestCountOutboundByStatusSince(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	seedUnconfirmed(t, s, "p1", "wx", "one")
	d := seedUnconfirmed(t, s, "p1", "wx", "two")
	if err := s.MarkDeliveryAttempt(ctx, d.ID, true, "", time.Time{}); err != nil {
		t.Fatal(err)
	}

	counts, err := s.CountOutboundByStatusSince(ctx, "default", "p1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if counts["sent_unconfirmed"] != 1 || counts["sent"] != 1 {
		t.Fatalf("counts = %+v, want 1 sent_unconfirmed + 1 sent", counts)
	}
}

func TestPendingSessionDiagnosticsAreScopedAndBounded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	first := seedPendingSession(t, s, "p1", "wx-chat", "first", 3)
	seedPendingSession(t, s, "p1", "other-chat", "second", 3)

	count, err := s.CountPendingSessionOutbound(ctx, "default", "p1")
	if err != nil || count != 2 {
		t.Fatalf("pending count=%d err=%v, want 2", count, err)
	}
	rows, err := s.ListPendingSessionOutbound(ctx, "default", "p1", 5)
	if err != nil || len(rows) != 2 {
		t.Fatalf("pending rows=%+v err=%v", rows, err)
	}
	ref := first.ID[:8]
	got, err := s.FindPendingSessionDelivery(ctx, "default", "p1", "weixin", "wx-user", "wx-chat", ref)
	if err != nil || got.ID != first.ID {
		t.Fatalf("resolved=%+v err=%v", got, err)
	}
	if _, err := s.FindPendingSessionDelivery(ctx, "default", "p1", "weixin", "wx-user", "other-chat", ref); err == nil {
		t.Fatal("a delivery from another chat must not resolve")
	}
	if _, err := s.FindPendingSessionDelivery(ctx, "default", "p1", "weixin", "wx-user", "wx-chat", "%%%%%%%%"); err == nil {
		t.Fatal("LIKE wildcard input must be rejected")
	}

	if err := s.MarkDeliveryPendingSession(ctx, first.ID, "still stale"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDeliveryPendingSession(ctx, first.ID, "still stale"); err != nil {
		t.Fatal(err)
	}
	eligible, err := s.ListCatchUpEligible(ctx, "default", "p1", "weixin", "wx-user", "wx-chat", time.Now().Add(-time.Hour), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(eligible) != 0 {
		t.Fatalf("max-attempt pending delivery remained auto-eligible: %+v", eligible)
	}
	dismissed, err := s.DismissPendingSessionDelivery(ctx, first.ID)
	if err != nil || !dismissed {
		t.Fatalf("dismissed=%v err=%v", dismissed, err)
	}
	if _, err := s.FindPendingSessionDelivery(ctx, "default", "p1", "weixin", "wx-user", "wx-chat", ref); err == nil {
		t.Fatal("dismissed delivery remained recoverable")
	}
	count, err = s.CountPendingSessionOutbound(ctx, "default", "p1")
	if err != nil || count != 1 {
		t.Fatalf("pending count after dismiss=%d err=%v, want 1", count, err)
	}
}
