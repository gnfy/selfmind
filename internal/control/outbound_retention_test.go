package control

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestPruneOutboundDeliveriesPreservesRecoverableRows(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	now := time.Now()
	old := now.Add(-15 * 24 * time.Hour).Unix()
	recent := now.Add(-time.Hour).Unix()

	cases := []struct {
		id          string
		status      string
		kind        string
		attempts    int
		maxAttempts int
		lastError   string
		updatedAt   int64
		want        bool
	}{
		{id: "sent-old", status: "sent", updatedAt: old},
		{id: "dismissed-old", status: "dismissed", updatedAt: old},
		{id: "failed-old", status: "failed", lastError: "no sender", updatedAt: old},
		{id: "sent-recent", status: "sent", updatedAt: recent, want: true},
		{id: "pending", status: "pending", updatedAt: old, want: true},
		{id: "retry", status: "retry", updatedAt: old, want: true},
		{id: "sending", status: "sending", updatedAt: old, want: true},
		{id: "pending-session", status: "pending_session", updatedAt: old, want: true},
		{id: "unconfirmed", status: "sent_unconfirmed", updatedAt: old, want: true},
		{
			id:          "recoverable-failed-ret",
			status:      "failed",
			kind:        "final_result",
			attempts:    1,
			maxAttempts: 3,
			lastError:   "iLink sendmessage error: ret=-2",
			updatedAt:   old,
			want:        true,
		},
		{
			id:          "recoverable-failed-prepare",
			status:      "failed",
			kind:        "recovery",
			attempts:    1,
			maxAttempts: 3,
			lastError:   "Prepare Failed",
			updatedAt:   old,
			want:        true,
		},
		{
			id:          "exhausted-failed",
			status:      "failed",
			kind:        "final_result",
			attempts:    3,
			maxAttempts: 3,
			lastError:   "ret=-2",
			updatedAt:   old,
		},
	}

	for _, tc := range cases {
		maxAttempts := tc.maxAttempts
		if maxAttempts == 0 {
			maxAttempts = 3
		}
		_, err := store.db.ExecContext(ctx,
			`INSERT INTO outbound_messages
			   (id, tenant_id, person_id, platform, channel, content, kind, status,
			    attempts, max_attempts, next_attempt_at, last_error, part_index,
			    part_total, created_at, updated_at)
			 VALUES (?, ?, ?, 'weixin', 'peer', 'test', ?, ?, ?, ?, ?, ?, 1, 1, ?, ?)`,
			tc.id, DefaultTenantID, "person", tc.kind, tc.status, tc.attempts,
			maxAttempts, tc.updatedAt, tc.lastError, tc.updatedAt, tc.updatedAt)
		if err != nil {
			t.Fatalf("insert %s: %v", tc.id, err)
		}
	}

	pruned, err := store.PruneOutboundDeliveries(ctx, 14*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 4 {
		t.Fatalf("pruned=%d want 4", pruned)
	}

	for _, tc := range cases {
		var count int
		if err := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM outbound_messages WHERE id = ?`, tc.id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if got := count == 1; got != tc.want {
			t.Errorf("%s retained=%v want %v", tc.id, got, tc.want)
		}
	}
}

func TestPruneOutboundDeliveriesDisabled(t *testing.T) {
	store := newTestStore(t)

	ctx := context.Background()
	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO outbound_messages
		   (id, tenant_id, person_id, platform, channel, content, status,
		    attempts, max_attempts, next_attempt_at, part_index, part_total,
		    created_at, updated_at)
		 VALUES ('sent-old', ?, 'person', 'weixin', 'peer', 'test', 'sent',
		         1, 3, ?, 1, 1, ?, ?)`,
		DefaultTenantID, old, old, old); err != nil {
		t.Fatal(err)
	}

	pruned, err := store.PruneOutboundDeliveries(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 0 {
		t.Fatalf("pruned=%d want 0", pruned)
	}

	var count int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outbound_messages WHERE id = 'sent-old'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal(fmt.Errorf("disabled retention removed terminal row"))
	}
}
