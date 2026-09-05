package control

import (
	"context"
	"testing"
	"time"
)

func TestMarkInboundSeen(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first, err := store.MarkInboundSeen(ctx, "feishu", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("first sighting must report first-seen")
	}

	again, err := store.MarkInboundSeen(ctx, "feishu", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Fatal("second sighting must report duplicate")
	}

	// Same id on another platform is a different message.
	other, err := store.MarkInboundSeen(ctx, "qq", "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if !other {
		t.Fatal("same id on a different platform must be first-seen")
	}

	// No id → nothing to dedup on; always first-seen.
	empty, err := store.MarkInboundSeen(ctx, "feishu", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		t.Fatal("empty id must report first-seen")
	}
}

func TestMarkInboundSeenPrunesOldRows(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	old := time.Now().Add(-inboundDedupRetention - time.Hour).Unix()
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO inbound_dedup(platform, message_id, created_at) VALUES('feishu','stale',?)`, old); err != nil {
		t.Fatal(err)
	}

	// Any insert prunes rows past the retention window, so the stale id is
	// forgotten and reports first-seen again.
	if _, err := store.MarkInboundSeen(ctx, "feishu", "fresh"); err != nil {
		t.Fatal(err)
	}
	first, err := store.MarkInboundSeen(ctx, "feishu", "stale")
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Fatal("row older than the retention window must have been pruned")
	}
}
