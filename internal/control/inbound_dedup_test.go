package control

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestInboundIdentityAndScope(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	owner := InboundOwner{TenantID: "default", PersonID: "person-a"}
	payload := []byte(`{"content":"work"}`)
	first, err := store.BeginInbound(ctx, "feishu", "msg-1", payload, owner)
	if err != nil || first != InboundPending {
		t.Fatalf("first receipt = %q, %v", first, err)
	}
	// Same id on another platform is a different message.
	other, err := store.BeginInbound(ctx, "qq", "msg-1", payload, owner)
	if err != nil || other != InboundPending {
		t.Fatalf("other platform receipt = %q, %v", other, err)
	}
	if _, err := store.BeginInbound(ctx, "feishu", " ", payload, owner); err == nil {
		t.Fatal("an empty platform id cannot create a durable receipt")
	}
}

func TestInboundReceiptFailureWindows(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	payload := []byte(`{"content":"first request"}`)
	owner := InboundOwner{TenantID: "default", PersonID: "person-a", Preview: "first request"}
	state, err := store.BeginInbound(ctx, "telegram", "update-1", payload, owner)
	if err != nil || state != InboundPending {
		t.Fatalf("before dispatch state = %q, %v", state, err)
	}
	// A crash before the claim may safely return to the pending path.
	state, err = store.BeginInbound(ctx, "telegram", "update-1", payload, owner)
	if err != nil || state != InboundPending {
		t.Fatalf("unclaimed replay state = %q, %v", state, err)
	}
	claimed, err := store.ClaimInbound(ctx, "telegram", "update-1")
	if err != nil || !claimed {
		t.Fatalf("claim = %t, %v", claimed, err)
	}
	// Once the gateway may have run, replay must preserve the original bytes
	// and refuse a second claim from another caller or process.
	state, err = store.BeginInbound(ctx, "telegram", "update-1", payload, owner)
	if err != nil || state != InboundDispatching {
		t.Fatalf("uncertain replay state = %q, %v", state, err)
	}
	claimed, err = store.ClaimInbound(ctx, "telegram", "update-1")
	if err != nil || claimed {
		t.Fatalf("second claim = %t, %v", claimed, err)
	}
	state, saved, err := store.InboundReceipt(ctx, "telegram", "update-1")
	if err != nil || state != InboundDispatching || string(saved) != string(payload) {
		t.Fatalf("saved receipt = %q %q, %v", state, saved, err)
	}
	if _, err := store.BeginInbound(ctx, "telegram", "update-1", []byte(`{"content":"changed"}`), owner); err == nil || !strings.Contains(err.Error(), "changed payload") {
		t.Fatalf("changed redelivery was accepted: %v", err)
	}
	if err := store.AcceptInbound(ctx, "telegram", "update-1"); err != nil {
		t.Fatal(err)
	}
	state, err = store.BeginInbound(ctx, "telegram", "update-1", payload, owner)
	if err != nil || state != InboundAccepted {
		t.Fatalf("completed replay state = %q, %v", state, err)
	}
	if _, err := store.BeginInbound(ctx, "telegram", "update-1", payload,
		InboundOwner{TenantID: "default", PersonID: "person-b"}); err == nil || !strings.Contains(err.Error(), "another identity") {
		t.Fatalf("cross-person redelivery accepted: %v", err)
	}
}

func TestInboundPrunesOnlyAcceptedRows(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	old := time.Now().Add(-inboundDedupRetention - time.Hour).Unix()
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO inbound_dedup(platform, message_id, created_at) VALUES('feishu','stale',?)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO inbound_dedup
		(platform, message_id, created_at, state, payload, tenant_id, person_id)
		VALUES('feishu','uncertain',?,'dispatching','{}','default','person-a')`, old); err != nil {
		t.Fatal(err)
	}
	owner := InboundOwner{TenantID: "default", PersonID: "person-a"}
	if _, err := store.BeginInbound(ctx, "feishu", "fresh", []byte(`{}`), owner); err != nil {
		t.Fatal(err)
	}
	state, err := store.BeginInbound(ctx, "feishu", "stale", []byte(`{}`), owner)
	if err != nil || state != InboundPending {
		t.Fatalf("stale accepted row was not pruned: %q %v", state, err)
	}
	state, _, err = store.InboundReceipt(ctx, "feishu", "uncertain")
	if err != nil || state != InboundDispatching {
		t.Fatalf("unresolved row was pruned: %q %v", state, err)
	}
}
