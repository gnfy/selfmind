package control

import (
	"context"
	"errors"
	"testing"
)

// A receipt's claim generation is a recorded fact: 0 means it has not been
// claimed since generations were recorded, each claim advances it, and
// whatever a writer records reads back through the receipt list unchanged.
func TestInboundReceiptReadsItsClaimGeneration(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	owner := InboundOwner{TenantID: "default", PersonID: "person-a", Preview: "deploy"}
	other := InboundOwner{TenantID: "default", PersonID: "person-b", Preview: "elsewhere"}
	for _, receipt := range []struct {
		platform, id string
		owner        InboundOwner
	}{{"weixin", "wx-1", owner}, {"feishu", "fs-1", owner}, {"telegram", "tg-1", other}} {
		if _, err := store.BeginInbound(ctx, receipt.platform, receipt.id, []byte(`{"content":"`+receipt.id+`"}`), receipt.owner); err != nil {
			t.Fatal(err)
		}
		if generation, err := store.ClaimInbound(ctx, receipt.platform, receipt.id); err != nil || generation != 1 {
			t.Fatalf("claim %s/%s = %d %v; want generation 1", receipt.platform, receipt.id, generation, err)
		}
	}
	generations := func(tenantID, personID string) map[string]int64 {
		t.Helper()
		listed, err := store.ListUncertainInboundForPerson(ctx, tenantID, personID, 20)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, row := range listed {
			out[row.Platform+"/"+row.MessageID] = row.ClaimGeneration
		}
		return out
	}
	if got := generations("default", "person-a"); len(got) != 2 || got["weixin/wx-1"] != 1 || got["feishu/fs-1"] != 1 {
		t.Fatalf("first-claim generations = %v", got)
	}
	for platform, generation := range map[string]int64{"weixin": 3, "feishu": 9223372036854775807, "telegram": 5} {
		if _, err := store.db.ExecContext(ctx, `UPDATE inbound_dedup SET claim_generation = ? WHERE platform = ?`, generation, platform); err != nil {
			t.Fatal(err)
		}
	}
	if got := generations("default", "person-a"); len(got) != 2 || got["weixin/wx-1"] != 3 || got["feishu/fs-1"] != 9223372036854775807 {
		t.Fatalf("recorded generations = %v", got)
	}
	if got := generations("default", "person-b"); len(got) != 1 || got["telegram/tg-1"] != 5 {
		t.Fatalf("another person's generation = %v", got)
	}
}

// Two dispatchers on separate connections claim one receipt in turn: the
// first claim is released and a redelivery claims the receipt again. A
// transition the first dispatcher sends late — a release, an acceptance, or a
// recorded reason — names a claim the receipt no longer holds, so it changes
// nothing, and the later claim still settles its own receipt.
func TestEarlierClaimCannotTransitionALaterClaimAcrossConnections(t *testing.T) {
	for _, late := range []string{"release", "accept", "note"} {
		t.Run(late, func(t *testing.T) {
			dir := t.TempDir()
			seedStoreDir(t, dir)
			stores := make([]*Store, 2)
			for i := range stores {
				store, err := OpenStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				stores[i] = store
			}
			earlier, later := stores[0], stores[1]
			ctx := context.Background()
			owner := InboundOwner{TenantID: "default", PersonID: "person-a", Preview: "rotate the keys"}
			if _, err := earlier.BeginInbound(ctx, "weixin", "wx-1", []byte(`{"content":"rotate the keys"}`), owner); err != nil {
				t.Fatal(err)
			}
			first, err := earlier.ClaimInbound(ctx, "weixin", "wx-1")
			if err != nil || first == 0 {
				t.Fatalf("first claim = %d, %v", first, err)
			}
			const released = "not dispatched: SelfMind is shutting down"
			if err := earlier.ReleaseInbound(ctx, "weixin", "wx-1", first, released); err != nil {
				t.Fatal(err)
			}
			second, err := later.ClaimInbound(ctx, "weixin", "wx-1")
			if err != nil || second <= first {
				t.Fatalf("redelivery claim = %d after claim %d, %v", second, first, err)
			}
			var lateErr error
			switch late {
			case "release":
				lateErr = earlier.ReleaseInbound(ctx, "weixin", "wx-1", first, "not dispatched: late refusal")
			case "accept":
				lateErr = earlier.AcceptInbound(ctx, "weixin", "wx-1", first)
			case "note":
				lateErr = earlier.NoteInboundFailure(ctx, "weixin", "wx-1", first, errors.New("gateway returned HTTP 500"))
			}
			if !errors.Is(lateErr, ErrInboundNotOwned) {
				t.Fatalf("late %s from the earlier claim = %v; want not owned", late, lateErr)
			}
			if state, generation, err := later.InboundReceiptClaim(ctx, "weixin", "wx-1"); err != nil || state != InboundDispatching || generation != second {
				t.Fatalf("receipt after a late %s = %q claim %d, %v; want dispatching under claim %d", late, state, generation, err, second)
			}
			if rows, err := later.ListUncertainInboundForPerson(ctx, "default", "person-a", 20); err != nil || len(rows) != 1 || rows[0].LastError != released {
				t.Fatalf("a late %s rewrote the later claim's record: %+v, %v", late, rows, err)
			}
			if err := later.AcceptInbound(ctx, "weixin", "wx-1", second); err != nil {
				t.Fatalf("the later claim cannot settle its own receipt: %v", err)
			}
			if state, _, err := earlier.InboundReceipt(ctx, "weixin", "wx-1"); err != nil || state != InboundAccepted {
				t.Fatalf("settled receipt = %q, %v; want accepted", state, err)
			}
		})
	}
}

// Only the claim that holds a receipt settles it. An acceptance naming an
// earlier claim, no claim, a claim never made, or another receipt's claim is
// refused without changing anything: generations count per receipt.
func TestAcceptWithStaleGenerationIsRefused(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	owner := InboundOwner{TenantID: "default", PersonID: "person-a"}
	for _, id := range []string{"tg-1", "tg-2"} {
		if _, err := store.BeginInbound(ctx, "telegram", id, []byte(`{"content":"`+id+`"}`), owner); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ClaimInbound(ctx, "telegram", "tg-1")
	if err != nil || first == 0 {
		t.Fatalf("first claim = %d, %v", first, err)
	}
	if err := store.ReleaseInbound(ctx, "telegram", "tg-1", first, "not dispatched: SelfMind is shutting down"); err != nil {
		t.Fatal(err)
	}
	current, err := store.ClaimInbound(ctx, "telegram", "tg-1")
	if err != nil || current <= first {
		t.Fatalf("second claim = %d after %d, %v", current, first, err)
	}
	neighbour, err := store.ClaimInbound(ctx, "telegram", "tg-2")
	if err != nil || neighbour == 0 || neighbour == current {
		t.Fatalf("other receipt's claim = %d, %v; want a generation of its own", neighbour, err)
	}
	for name, generation := range map[string]int64{"earlier claim": first, "no claim": 0, "claim never made": current + 1} {
		if err := store.AcceptInbound(ctx, "telegram", "tg-1", generation); !errors.Is(err, ErrInboundNotOwned) {
			t.Fatalf("acceptance under %s (%d) = %v; want not owned", name, generation, err)
		}
	}
	if err := store.AcceptInbound(ctx, "telegram", "tg-2", current); !errors.Is(err, ErrInboundNotOwned) {
		t.Fatalf("acceptance under another receipt's claim = %v; want not owned", err)
	}
	for _, id := range []string{"tg-1", "tg-2"} {
		if state, _, err := store.InboundReceipt(ctx, "telegram", id); err != nil || state != InboundDispatching {
			t.Fatalf("%s after refused acceptances = %q, %v; want dispatching", id, state, err)
		}
	}
	if err := store.AcceptInbound(ctx, "telegram", "tg-1", current); err != nil {
		t.Fatalf("acceptance under the holding claim: %v", err)
	}
	if err := store.AcceptInbound(ctx, "telegram", "tg-2", neighbour); err != nil {
		t.Fatalf("acceptance under the other receipt's own claim: %v", err)
	}
	// A settled receipt is held by no claim, its own included.
	if err := store.ReleaseInbound(ctx, "telegram", "tg-1", current, "late refusal"); !errors.Is(err, ErrInboundNotOwned) {
		t.Fatalf("release of an accepted receipt = %v; want not owned", err)
	}
	if state, generation, err := store.InboundReceiptClaim(ctx, "telegram", "tg-1"); err != nil || state != InboundAccepted || generation != current {
		t.Fatalf("accepted receipt = %q claim %d, %v", state, generation, err)
	}
}

// A delivery that does not hold a receipt's claim may explain the receipt only
// where its holder has not: its note lands on an empty reason under the
// holding generation, never under another generation or on a settled receipt,
// and never replaces the holder's reason, while the holder's own note
// replaces it.
func TestUnexplainedNoteNeverReplacesTheHolderReason(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	owner := InboundOwner{TenantID: "default", PersonID: "person-a"}
	if _, err := store.BeginInbound(ctx, "weixin", "wx-1", []byte(`{"content":"answer 2"}`), owner); err != nil {
		t.Fatal(err)
	}
	holder, err := store.ClaimInbound(ctx, "weixin", "wx-1")
	if err != nil || holder == 0 {
		t.Fatalf("claim = %d, %v", holder, err)
	}
	reason := func() string {
		t.Helper()
		var lastError string
		if err := store.db.QueryRowContext(ctx, `SELECT last_error FROM inbound_dedup WHERE platform = 'weixin' AND message_id = 'wx-1'`).Scan(&lastError); err != nil {
			t.Fatal(err)
		}
		return lastError
	}
	const unknown = "claim outcome unknown; this delivery did not dispatch it"
	const held = "choice answered, but its request was not saved"
	for _, generation := range []int64{0, holder + 1} {
		if err := store.NoteUnexplainedInbound(ctx, "weixin", "wx-1", generation, errors.New(unknown)); err != nil {
			t.Fatal(err)
		}
		if got := reason(); got != "" {
			t.Fatalf("a note under claim %d reached the receipt held by claim %d: %q", generation, holder, got)
		}
	}
	if err := store.NoteUnexplainedInbound(ctx, "weixin", "wx-1", holder, errors.New(unknown)); err != nil || reason() != unknown {
		t.Fatalf("an unexplained receipt did not take the note: %q, %v", reason(), err)
	}
	if err := store.NoteInboundFailure(ctx, "weixin", "wx-1", holder, errors.New(held)); err != nil || reason() != held {
		t.Fatalf("the holder's reason did not replace the note: %q, %v", reason(), err)
	}
	if err := store.NoteUnexplainedInbound(ctx, "weixin", "wx-1", holder, errors.New(unknown)); err != nil || reason() != held {
		t.Fatalf("a note replaced the holder's reason: %q, %v", reason(), err)
	}
	if err := store.AcceptInbound(ctx, "weixin", "wx-1", holder); err != nil {
		t.Fatal(err)
	}
	if err := store.NoteUnexplainedInbound(ctx, "weixin", "wx-1", holder, errors.New(unknown)); err != nil || reason() != "" {
		t.Fatalf("a settled receipt took a note: %q, %v", reason(), err)
	}
}

// A receipt saved by a release without claim generations reads generation 0.
// A pending one is claimed from 0 to 1 and settles under that claim like any
// other; an uncertain one stays held, because generation 0 names no claim and
// settles nothing.
func TestHistoricalReceiptClaimsFromGenerationZero(t *testing.T) {
	for _, fixture := range releasedRowFixtures {
		if fixture.version < 22 {
			continue
		}
		t.Run(fixture.file, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			loadRowFixture(t, dir, fixture)
			store, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if state, generation, err := store.InboundReceiptClaim(ctx, "weixin", "fixture-wx-pending"); err != nil || state != InboundPending || generation != 0 {
				t.Fatalf("historical pending receipt = %q claim %d, %v", state, generation, err)
			}
			claim, err := store.ClaimInbound(ctx, "weixin", "fixture-wx-pending")
			if err != nil || claim != 1 {
				t.Fatalf("claim of a historical pending receipt = %d, %v; want 1", claim, err)
			}
			if err := store.AcceptInbound(ctx, "weixin", "fixture-wx-pending", claim); err != nil {
				t.Fatalf("acceptance of a historical receipt under its first claim: %v", err)
			}
			if state, generation, err := store.InboundReceiptClaim(ctx, "weixin", "fixture-wx-pending"); err != nil || state != InboundAccepted || generation != 1 {
				t.Fatalf("settled historical receipt = %q claim %d, %v", state, generation, err)
			}

			const uncertain = "fixture-wx-uncertain"
			for name, transition := range map[string]func() error{
				"release": func() error { return store.ReleaseInbound(ctx, "weixin", uncertain, 0, "not dispatched: retry") },
				"accept":  func() error { return store.AcceptInbound(ctx, "weixin", uncertain, 0) },
				"note": func() error {
					return store.NoteInboundFailure(ctx, "weixin", uncertain, 0, errors.New("gateway returned HTTP 500"))
				},
			} {
				if err := transition(); !errors.Is(err, ErrInboundNotOwned) {
					t.Fatalf("%s of a historical uncertain receipt under generation 0 = %v; want not owned", name, err)
				}
			}
			if claim, err := store.ClaimInbound(ctx, "weixin", uncertain); err != nil || claim != 0 {
				t.Fatalf("claim of a historical uncertain receipt = %d, %v; want none", claim, err)
			}
			owner, err := store.ResolveAccount(ctx, DefaultTenantID, "cli", "fixture-owner")
			if err != nil || owner == nil {
				t.Fatalf("fixture owner: %+v %v", owner, err)
			}
			rows, err := store.ListUncertainInboundForPerson(ctx, owner.TenantID, owner.PersonID, 20)
			if err != nil {
				t.Fatal(err)
			}
			held := false
			for _, row := range rows {
				if row.MessageID == uncertain {
					held = row.State == InboundDispatching && row.ClaimGeneration == 0 &&
						row.LastError == "gateway unavailable: connection reset"
				}
			}
			if !held {
				t.Fatalf("historical uncertain receipt changed: %+v", rows)
			}
		})
	}
}
