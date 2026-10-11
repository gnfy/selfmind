package control

import (
	"context"
	"testing"
)

func TestOutboundNotificationPartsCommitTogether(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_second_notice BEFORE INSERT ON outbound_messages WHEN NEW.part_index=2 BEGIN SELECT RAISE(ABORT,'injected part failure'); END`); err != nil {
		t.Fatal(err)
	}
	parts := []Delivery{
		{ID: "notice_1", PersonID: "person", Content: "first", PartIndex: 1, PartTotal: 2, IdempotencyKey: "notice-key-1"},
		{ID: "notice_2", PersonID: "person", Content: "second", PartIndex: 2, PartTotal: 2, IdempotencyKey: "notice-key-2"},
	}
	if _, err := store.EnqueueDeliveries(ctx, parts); err == nil {
		t.Fatal("injected second-part failure was not observed")
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM outbound_messages`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed notification left %d parts behind: %v", count, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_second_notice`); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		saved, err := store.EnqueueDeliveries(ctx, parts)
		if err != nil || len(saved) != 2 || saved[0].ID != "notice_1" || saved[1].ID != "notice_2" {
			t.Fatalf("retry did not preserve the winning rows: %+v %v", saved, err)
		}
	}
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM outbound_messages`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("notification was duplicated: %d %v", count, err)
	}
}
