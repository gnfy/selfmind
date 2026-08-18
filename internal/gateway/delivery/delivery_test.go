package delivery

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/control"
)

// TestNoDoubleDispatch reproduces the live duplicate-push bug: the
// EnqueueAndTry immediate attempt and the retry poller both saw a freshly
// enqueued (immediately due) row and both sent it. With row claiming, running
// the two dispatch paths against the same due snapshot must invoke the sender
// exactly once.
func TestNoDoubleDispatch(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		ID: "apr_test", TenantID: "default", PersonID: "p1",
	}); err != nil {
		t.Fatal(err)
	}

	var sends int32
	svc := NewService(store, SenderFunc(func(ctx context.Context, msg Message) error {
		atomic.AddInt32(&sends, 1)
		return nil
	}), Options{PollInterval: time.Hour}) // poller not started; we drive it by hand

	if err := svc.EnqueueAndTry(ctx, Message{
		TenantID: "default", PersonID: "p1", Platform: "weixin",
		PlatformUserID: "wx-user", Channel: "wx-chat",
		Content: "Approval required: [terminal] chmod +x hello3.sh",
		Kind:    KindApproval, ApprovalID: "apr_test",
	}); err != nil {
		t.Fatalf("EnqueueAndTry: %v", err)
	}
	// Simulate the poller racing in with a due snapshot taken before the
	// immediate attempt resolved: flushDue must find nothing claimable.
	svc.flushDue(ctx)

	if got := atomic.LoadInt32(&sends); got != 1 {
		t.Fatalf("sender invoked %d times, want exactly 1", got)
	}

	due, err := store.ListDueDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("nothing should remain due after a successful send: %+v", due)
	}
}

func TestResolvedApprovalDeliveryIsSupersededBeforeSend(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		ID: "apr_resolved", TenantID: "default", PersonID: "p1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ExpireApprovalRequest(ctx, "default", "apr_resolved", "run ended"); err != nil {
		t.Fatal(err)
	}

	var sends int32
	svc := NewService(store, SenderFunc(func(context.Context, Message) error {
		atomic.AddInt32(&sends, 1)
		return nil
	}), Options{PollInterval: time.Hour})
	if err := svc.EnqueueAndTry(ctx, Message{
		TenantID: "default", PersonID: "p1", Platform: "weixin", Channel: "wx-chat",
		Content: "obsolete approval", Kind: KindApproval, ApprovalID: "apr_resolved",
	}); err != nil {
		t.Fatalf("superseding stale approval: %v", err)
	}
	if got := atomic.LoadInt32(&sends); got != 0 {
		t.Fatalf("stale approval sent %d times", got)
	}
	counts, err := store.CountOutboundByStatusSince(ctx, "default", "p1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if counts["superseded"] != 1 || counts["failed"] != 0 || counts["pending_session"] != 0 {
		t.Fatalf("delivery counts = %#v", counts)
	}
}

// receiptSender fakes a platform that accepts every send but reports delivery
// confidence per message (the Weixin/iLink stale-context_token situation).
type receiptSender struct{ confirmed bool }

func (r *receiptSender) Send(ctx context.Context, msg Message) error { return nil }
func (r *receiptSender) SendWithReceipt(ctx context.Context, msg Message) (bool, error) {
	return r.confirmed, nil
}

// TestUnconfirmedDeliveryIsTerminalButDistinct: an accepted-but-doubtful send
// must finalize as sent_unconfirmed — never retried (same stale session would
// risk duplicates) yet distinguishable from 'sent' for digest surfacing.
func TestUnconfirmedDeliveryIsTerminalButDistinct(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	svc := NewService(store, &receiptSender{confirmed: false}, Options{PollInterval: time.Hour})
	if err := svc.EnqueueAndTry(ctx, Message{
		TenantID: "default", PersonID: "p1", Platform: "weixin",
		PlatformUserID: "wx", Channel: "wx", Content: "task finished",
	}); err != nil {
		t.Fatalf("EnqueueAndTry: %v", err)
	}
	due, err := store.ListDueDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("unconfirmed delivery must be terminal for the queue: %+v", due)
	}
	// Confirmed sends stay plain 'sent'.
	svc2 := NewService(store, &receiptSender{confirmed: true}, Options{PollInterval: time.Hour})
	if err := svc2.EnqueueAndTry(ctx, Message{
		TenantID: "default", PersonID: "p1", Platform: "weixin",
		PlatformUserID: "wx", Channel: "wx", Content: "second",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEnqueueConfirmedDoesNotConfuseAcceptedWithDelivered(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	unconfirmed := NewService(store, &receiptSender{confirmed: false}, Options{PollInterval: time.Hour})
	ok, err := unconfirmed.EnqueueAndTryConfirmed(ctx, Message{
		TenantID: "default", PersonID: "p1", Platform: "weixin", Channel: "wx", Content: "possibly delivered",
	})
	if err != nil || ok {
		t.Fatalf("unconfirmed send = %v, %v; want false, nil", ok, err)
	}
	confirmed := NewService(store, &receiptSender{confirmed: true}, Options{PollInterval: time.Hour})
	ok, err = confirmed.EnqueueAndTryConfirmed(ctx, Message{
		TenantID: "default", PersonID: "p1", Platform: "weixin", Channel: "wx", Content: "confirmed delivered",
	})
	if err != nil || !ok {
		t.Fatalf("confirmed send = %v, %v; want true, nil", ok, err)
	}
}

func TestLogicalDeliveryReplayReusesDurableOutboxRow(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var sends int32
	svc := NewService(store, SenderFunc(func(context.Context, Message) error {
		atomic.AddInt32(&sends, 1)
		return nil
	}), Options{PollInterval: time.Hour})

	base := Message{
		TenantID: "default", PersonID: "p1", Platform: "weixin", PlatformUserID: "wx", Channel: "wx",
		TaskID: "task-1", RunID: "run-retry", Kind: KindFinalResult, LogicalKey: "effect-1", Content: "first rendering",
	}
	accepted, err := svc.EnqueueAndTryAccepted(ctx, base)
	if err != nil || !accepted {
		t.Fatalf("first enqueue = %v, %v", accepted, err)
	}
	base.Content = "retry rendering changed"
	accepted, err = svc.EnqueueAndTryAccepted(ctx, base)
	if err != nil || !accepted {
		t.Fatalf("replayed enqueue = %v, %v", accepted, err)
	}
	if got := atomic.LoadInt32(&sends); got != 1 {
		t.Fatalf("logical replay sent %d times, want 1", got)
	}
}
