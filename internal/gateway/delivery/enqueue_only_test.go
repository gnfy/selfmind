package delivery

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Enqueue only records: the sender is never called by it, the row waits in the
// outbox, a replay of the same logical message adds nothing, and the service's
// loop later sends it exactly once.
func TestEnqueueRecordsWithoutSendingAndTheLoopSendsItOnce(t *testing.T) {
	ctx := context.Background()
	store, err := openDeliveryFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var sends int32
	svc := NewService(store, SenderFunc(func(context.Context, Message) error {
		atomic.AddInt32(&sends, 1)
		return nil
	}), Options{PollInterval: 10 * time.Millisecond})

	msg := Message{TenantID: "default", PersonID: "p1", Platform: "weixin", PlatformUserID: "wx", Channel: "wx",
		TaskID: "task-1", Kind: KindRecovery, LogicalKey: "notice-1", Content: "queued work was not started"}
	for attempt := 0; attempt < 2; attempt++ {
		accepted, err := svc.Enqueue(ctx, msg)
		if err != nil || !accepted {
			t.Fatalf("enqueue %d = %v, %v; want accepted", attempt, accepted, err)
		}
	}
	if got := atomic.LoadInt32(&sends); got != 0 {
		t.Fatalf("Enqueue sent %d times; it only records", got)
	}
	due, err := store.ListDueDeliveries(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("the outbox holds %d rows (%v), want the one logical message", len(due), err)
	}

	svc.Start(ctx)
	defer svc.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&sends) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the loop never sent the recorded message")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)
	if got := atomic.LoadInt32(&sends); got != 1 {
		t.Fatalf("the message was sent %d times, want once", got)
	}

	var nilService *Service
	if accepted, err := nilService.Enqueue(ctx, msg); accepted || err == nil {
		t.Fatalf("a missing service accepts nothing: %v, %v", accepted, err)
	}
}
