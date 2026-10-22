package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	claim, err := store.ClaimInbound(ctx, "telegram", "update-1")
	if err != nil || claim == 0 {
		t.Fatalf("claim = %d, %v", claim, err)
	}
	// Once the gateway may have run, replay must preserve the original bytes
	// and refuse a second claim from another caller or process.
	state, err = store.BeginInbound(ctx, "telegram", "update-1", payload, owner)
	if err != nil || state != InboundDispatching {
		t.Fatalf("uncertain replay state = %q, %v", state, err)
	}
	if second, err := store.ClaimInbound(ctx, "telegram", "update-1"); err != nil || second != 0 {
		t.Fatalf("second claim = %d, %v", second, err)
	}
	state, saved, err := store.InboundReceipt(ctx, "telegram", "update-1")
	if err != nil || state != InboundDispatching || string(saved) != string(payload) {
		t.Fatalf("saved receipt = %q %q, %v", state, saved, err)
	}
	// A possibly effectful receipt answers every redelivery the same way,
	// whatever the copy carries; it never becomes dispatchable again.
	if state, err := store.BeginInbound(ctx, "telegram", "update-1", []byte(`{"content":"changed"}`), owner); err != nil || state != InboundDispatching {
		t.Fatalf("changed redelivery of an uncertain receipt = %q, %v", state, err)
	}
	if err := store.AcceptInbound(ctx, "telegram", "update-1", claim); err != nil {
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

// Only a claimed receipt can return to pending, and the next redelivery is
// claimed exactly once. Accepted and never-claimed receipts are unaffected.
func TestReleaseInboundReturnsOnlyClaimedReceiptToPending(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	owner := InboundOwner{TenantID: "default", PersonID: "person-a", Preview: "start the release"}
	payload := []byte(`{"content":"start the release"}`)
	if _, err := store.BeginInbound(ctx, "weixin", "m-1", payload, owner); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseInbound(ctx, "weixin", "m-1", 1, "not dispatched: SelfMind is shutting down"); err == nil {
		t.Fatal("a receipt that was never claimed was released")
	}
	first, err := store.ClaimInbound(ctx, "weixin", "m-1")
	if err != nil || first == 0 {
		t.Fatalf("first claim = %d, %v", first, err)
	}
	if err := store.ReleaseInbound(ctx, "weixin", "m-1", first, "not dispatched: SelfMind is shutting down"); err != nil {
		t.Fatal(err)
	}
	state, err := store.BeginInbound(ctx, "weixin", "m-1", payload, owner)
	if err != nil || state != InboundPending {
		t.Fatalf("released receipt redelivery = %q, %v; want pending", state, err)
	}
	rows, err := store.ListUncertainInboundForPerson(ctx, "default", "person-a", 20)
	if err != nil || len(rows) != 1 || rows[0].State != InboundPending || !strings.Contains(rows[0].LastError, "shutting down") {
		t.Fatalf("released receipt diagnostic = %+v, %v", rows, err)
	}
	redelivery, err := store.ClaimInbound(ctx, "weixin", "m-1")
	if err != nil || redelivery <= first {
		t.Fatalf("redelivery claim = %d after claim %d, %v", redelivery, first, err)
	}
	if second, err := store.ClaimInbound(ctx, "weixin", "m-1"); err != nil || second != 0 {
		t.Fatalf("second redelivery claim = %d, %v; want exactly one claimer", second, err)
	}
	if err := store.AcceptInbound(ctx, "weixin", "m-1", redelivery); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseInbound(ctx, "weixin", "m-1", redelivery, "late refusal"); err == nil {
		t.Fatal("an accepted receipt was released")
	}
	if state, _, err := store.InboundReceipt(ctx, "weixin", "m-1"); err != nil || state != InboundAccepted {
		t.Fatalf("accepted receipt = %q, %v", state, err)
	}
}

// A never-started receipt whose redelivery no longer matches it is a typed
// conflict, recorded on that receipt. The adapter's essential fields decide
// what matching means. A receipt with a settled or uncertain outcome answers
// by its state and keeps its own record.
func TestConflictingRedeliveryIsTypedAndNotedOnPendingReceipt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	owner := InboundOwner{TenantID: "default", PersonID: "person-a"}
	if _, err := store.BeginInbound(ctx, "weixin", "m-pending", []byte(`{"text":"a","token":"1"}`), owner); err != nil {
		t.Fatal(err)
	}
	textOnly := func(raw []byte) ([]byte, error) {
		var fields map[string]string
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		return []byte(fields["text"]), nil
	}
	if state, err := store.BeginInboundMatching(ctx, "weixin", "m-pending", []byte(`{"text":"a","token":"2"}`), owner, textOnly); err != nil || state != InboundPending {
		t.Fatalf("redelivery with only a new token = %q, %v; want pending", state, err)
	}
	if _, err := store.BeginInbound(ctx, "weixin", "m-pending", []byte(`{"text":"a","token":"2"}`), owner); !errors.Is(err, ErrInboundConflict) {
		t.Fatalf("byte-compared redelivery error = %v", err)
	}
	if _, err := store.BeginInboundMatching(ctx, "weixin", "m-pending", []byte(`{"text":"b","token":"1"}`), owner, textOnly); !errors.Is(err, ErrInboundConflict) {
		t.Fatalf("changed redelivery error = %v", err)
	}
	if _, err := store.BeginInbound(ctx, "weixin", "m-pending", []byte(`{"text":"a"}`),
		InboundOwner{TenantID: "default", PersonID: "person-b"}); !errors.Is(err, ErrInboundConflict) {
		t.Fatalf("foreign redelivery error = %v", err)
	}
	rows, err := store.ListUncertainInboundForPerson(ctx, "default", "person-a", 20)
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].LastError, "content changed") {
		t.Fatalf("pending receipt does not record the rejected copy: %+v, %v", rows, err)
	}
	if _, err := store.BeginInbound(ctx, "weixin", "m-uncertain", []byte(`{"text":"a"}`), owner); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimInbound(ctx, "weixin", "m-uncertain")
	if err != nil || claim == 0 {
		t.Fatalf("claim = %d, %v", claim, err)
	}
	if err := store.NoteInboundFailure(ctx, "weixin", "m-uncertain", claim, errors.New("gateway returned HTTP 500")); err != nil {
		t.Fatal(err)
	}
	if state, err := store.BeginInbound(ctx, "weixin", "m-uncertain", []byte(`{"text":"b"}`), owner); err != nil || state != InboundDispatching {
		t.Fatalf("changed redelivery of an uncertain receipt = %q, %v; want dispatching", state, err)
	}
	rows, err = store.ListUncertainInboundForPerson(ctx, "default", "person-a", 20)
	for _, row := range rows {
		if row.MessageID == "m-uncertain" && row.LastError != "gateway returned HTTP 500" {
			t.Fatalf("uncertain receipt lost its own record: %+v", row)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
}

// Concurrent redeliveries may race from separate processes. Only one
// dispatcher may move a pending receipt to dispatching, and only it receives
// the claim's generation; every other claim reports none without an error.
func TestClaimInboundAdmitsOneDispatcherAcrossConnections(t *testing.T) {
	dir := t.TempDir()
	seedStoreDir(t, dir)
	stores := make([]*Store, 3)
	for i := range stores {
		store, err := OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		stores[i] = store
	}
	ctx := context.Background()
	owner := InboundOwner{TenantID: "default", PersonID: "person-a"}
	for round := 0; round < 5; round++ {
		messageID := fmt.Sprintf("m-race-%d", round)
		if _, err := stores[0].BeginInbound(ctx, "weixin", messageID, []byte(`{"content":"race"}`), owner); err != nil {
			t.Fatal(err)
		}
		const claimers = 9
		results := make(chan int64, claimers)
		errs := make(chan error, claimers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < claimers; i++ {
			wg.Add(1)
			go func(store *Store) {
				defer wg.Done()
				<-start
				generation, err := store.ClaimInbound(ctx, "weixin", messageID)
				if err != nil {
					errs <- err
					return
				}
				results <- generation
			}(stores[i%len(stores)])
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			t.Fatalf("round %d claim error: %v", round, err)
		}
		var winners []int64
		for generation := range results {
			if generation != 0 {
				winners = append(winners, generation)
			}
		}
		if len(winners) != 1 || winners[0] != 1 {
			t.Fatalf("round %d admitted claims %v, want one dispatcher holding generation 1", round, winners)
		}
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
