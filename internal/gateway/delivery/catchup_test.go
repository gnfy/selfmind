package delivery

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
)

// switchableReceiptSender fakes the Weixin/iLink session lifecycle: sends are
// unconfirmed while the context_token is stale, confirmed after an inbound
// refreshes it. It records every delivered content in order.
type switchableReceiptSender struct {
	confirmed bool
	err       error
	sent      []string
}

func (r *switchableReceiptSender) Send(ctx context.Context, msg Message) error {
	r.sent = append(r.sent, msg.Content)
	return r.err
}

func (r *switchableReceiptSender) SendWithReceipt(ctx context.Context, msg Message) (bool, error) {
	r.sent = append(r.sent, msg.Content)
	return r.confirmed, r.err
}

type refreshRequiredError string

func (e refreshRequiredError) Error() string                { return string(e) }
func (e refreshRequiredError) SessionRefreshRequired() bool { return true }

// TestCatchUpUnconfirmedRepushesOnceBounded pins the P0-1 catch-up contract:
// after the peer's inbound refreshes the session, unconfirmed pushes are
// re-sent oldest-first up to the cap, flip to 'sent' when confirmed, and a
// second catch-up re-pushes NOTHING (at-most-once).
func TestCatchUpUnconfirmedRepushesOnceBounded(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sender := &switchableReceiptSender{confirmed: false}
	svc := NewService(store, sender, Options{PollInterval: time.Hour, CatchUpLimit: 2})

	// Three pushes while the session is stale — all finalize sent_unconfirmed.
	for _, content := range []string{"first", "second", "third"} {
		if err := svc.EnqueueAndTry(ctx, Message{
			TenantID: "default", PersonID: "p1", Platform: "weixin",
			PlatformUserID: "wx", Channel: "wx-chat", Content: content,
		}); err != nil {
			t.Fatal(err)
		}
	}
	sender.sent = nil

	// Inbound arrives, token fresh: catch-up re-pushes oldest-first, capped at 2.
	sender.confirmed = true
	if n := svc.CatchUpUnconfirmed(ctx, "default", "p1", "weixin", "wx-chat"); n != 2 {
		t.Fatalf("confirmed re-pushes = %d, want 2 (cap)", n)
	}
	if len(sender.sent) != 2 || sender.sent[0] != "first" || sender.sent[1] != "second" {
		t.Fatalf("re-pushed = %v, want oldest-first [first second]", sender.sent)
	}

	// Second catch-up: only "third" is still eligible; first/second are 'sent'.
	sender.sent = nil
	if n := svc.CatchUpUnconfirmed(ctx, "default", "p1", "weixin", "wx-chat"); n != 1 {
		t.Fatalf("second catch-up = %d, want 1 (the remaining row)", n)
	}
	if len(sender.sent) != 1 || sender.sent[0] != "third" {
		t.Fatalf("second catch-up sent %v, want [third]", sender.sent)
	}

	// Third catch-up: nothing left — at-most-once held for every row.
	sender.sent = nil
	if n := svc.CatchUpUnconfirmed(ctx, "default", "p1", "weixin", "wx-chat"); n != 0 {
		t.Fatalf("third catch-up = %d, want 0", n)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("nothing should be re-sent, got %v", sender.sent)
	}
}

// TestCatchUpStillUnconfirmedNeverLoops: a re-push that is STILL unconfirmed
// consumes the row's one catch-up claim — it must not be re-pushed again by the
// next inbound (the failure mode would be an infinite duplicate drip).
func TestCatchUpStillUnconfirmedNeverLoops(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sender := &switchableReceiptSender{confirmed: false}
	svc := NewService(store, sender, Options{PollInterval: time.Hour})
	if err := svc.EnqueueAndTry(ctx, Message{
		TenantID: "default", PersonID: "p1", Platform: "weixin",
		PlatformUserID: "wx", Channel: "wx-chat", Content: "doomed",
	}); err != nil {
		t.Fatal(err)
	}
	sender.sent = nil

	// Token still stale: the re-push is attempted once, stays unconfirmed.
	if n := svc.CatchUpUnconfirmed(ctx, "default", "p1", "weixin", "wx-chat"); n != 0 {
		t.Fatalf("unconfirmed re-push must not count as confirmed, got %d", n)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("exactly one re-push attempt expected, got %v", sender.sent)
	}

	// Next inbound: the claim is consumed; no more attempts ever.
	sender.sent = nil
	if n := svc.CatchUpUnconfirmed(ctx, "default", "p1", "weixin", "wx-chat"); n != 0 || len(sender.sent) != 0 {
		t.Fatalf("claimed row must never re-push again (n=%d sent=%v)", n, sender.sent)
	}
}

func TestCriticalPrepareFailureWaitsForFreshInboundThenRecovers(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sender := &switchableReceiptSender{err: refreshRequiredError("iLink sendmessage error: ret=-2 errcode=0 errmsg=prepare failed")}
	svc := NewService(store, sender, Options{PollInterval: time.Hour})
	if err := svc.EnqueueAndTry(ctx, Message{
		TenantID: "default", PersonID: "p1", Platform: "weixin",
		PlatformUserID: "wx", Channel: "wx-chat", Content: "final answer", Kind: KindFinalResult,
	}); err == nil {
		t.Fatal("prepare failure must surface to the immediate caller")
	}
	rows, err := store.ListCatchUpEligible(ctx, "default", "p1", "weixin", "wx-chat", time.Now().Add(-time.Hour), 10)
	if err != nil || len(rows) != 1 || rows[0].Status != "pending_session" {
		t.Fatalf("pending-session rows=%+v err=%v", rows, err)
	}

	// A new inbound refreshed the session. The critical result is replayed once
	// and becomes confirmed instead of remaining a permanent failure.
	sender.err = nil
	sender.confirmed = true
	sender.sent = nil
	if n := svc.CatchUpRecoverable(ctx, "default", "p1", "weixin", "wx-chat"); n != 1 {
		t.Fatalf("confirmed catch-up=%d, want 1", n)
	}
	if len(sender.sent) != 1 || sender.sent[0] != "final answer" {
		t.Fatalf("catch-up sent=%v", sender.sent)
	}
	if rows, _ := store.ListCatchUpEligible(ctx, "default", "p1", "weixin", "wx-chat", time.Now().Add(-time.Hour), 10); len(rows) != 0 {
		t.Fatalf("confirmed row remained eligible: %+v", rows)
	}
}

func TestManualPendingSessionRetryIsPeerScoped(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sender := &switchableReceiptSender{err: refreshRequiredError("prepare failed")}
	svc := NewService(store, sender, Options{PollInterval: time.Hour})
	if err := svc.EnqueueAndTry(ctx, Message{
		TenantID: "default", PersonID: "p1", Platform: "weixin",
		PlatformUserID: "wx", Channel: "wx-chat", Content: "durable result", Kind: KindFinalResult,
	}); err == nil {
		t.Fatal("initial prepare failure must surface")
	}
	rows, err := store.ListPendingSessionOutbound(ctx, "default", "p1", 5)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending rows=%+v err=%v", rows, err)
	}
	ref := rows[0].ID[:8]
	if _, _, err := svc.RetryPendingSession(ctx, "default", "p1", "weixin", "other-chat", ref); err == nil {
		t.Fatal("manual retry from another chat must fail")
	}

	sender.err = nil
	sender.confirmed = true
	sender.sent = nil
	id, status, err := svc.RetryPendingSession(ctx, "default", "p1", "weixin", "wx-chat", ref)
	if err != nil || id != rows[0].ID || status != "sent" {
		t.Fatalf("retry id=%q status=%q err=%v", id, status, err)
	}
	if len(sender.sent) != 1 || sender.sent[0] != "durable result" {
		t.Fatalf("manual retry sent=%v", sender.sent)
	}
	if _, _, err := svc.RetryPendingSession(ctx, "default", "p1", "weixin", "wx-chat", ref); err == nil {
		t.Fatal("a confirmed delivery must not be manually replayable")
	}
}

func TestCatchUpPermanentFailureLeavesNoFakePendingRow(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	sender := &switchableReceiptSender{err: refreshRequiredError("prepare failed")}
	svc := NewService(store, sender, Options{PollInterval: time.Hour})
	if err := svc.EnqueueAndTry(ctx, Message{
		TenantID: "default", PersonID: "p1", Platform: "weixin",
		PlatformUserID: "wx", Channel: "wx-chat", Content: "result", Kind: KindFinalResult,
	}); err == nil {
		t.Fatal("initial prepare failure must surface")
	}
	sender.err = context.DeadlineExceeded
	sender.sent = nil
	if n := svc.CatchUpRecoverable(ctx, "default", "p1", "weixin", "wx-chat"); n != 0 {
		t.Fatalf("failed catch-up count=%d, want 0", n)
	}
	if rows, _ := store.ListPendingSessionOutbound(ctx, "default", "p1", 5); len(rows) != 0 {
		t.Fatalf("permanent failure remained pending: %+v", rows)
	}
	undelivered, err := store.ListUndeliveredOutbound(ctx, "default", "p1", time.Now().Add(-time.Hour), 5)
	if err != nil || len(undelivered) != 1 || undelivered[0].Status != "failed" {
		t.Fatalf("undelivered=%+v err=%v", undelivered, err)
	}
}
