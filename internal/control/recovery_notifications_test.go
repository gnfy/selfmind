package control

import (
	"context"
	"testing"
)

func TestRecoveryNotificationLifecycle(t *testing.T) {
	ctx := context.Background()
	store, _, _, run := newRecoveryFixture(t)
	if recovered, err := store.MarkInterruptedRuns(ctx, 0); err != nil || recovered != 1 {
		t.Fatalf("recover: count=%d err=%v", recovered, err)
	}

	pending, err := store.ListPendingRecoveryNotifications(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].RunID != run.ID {
		t.Fatalf("pending = %+v err=%v", pending, err)
	}
	if err := store.MarkRecoveryNotificationSent(ctx, pending[0]); err != nil {
		t.Fatal(err)
	}
	pending, err = store.ListPendingRecoveryNotifications(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("notification must be idempotently consumed: %+v err=%v", pending, err)
	}
}
