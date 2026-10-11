package control

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// selfmind_test_pause holds a statement open without giving SQLite a point at
// which to honor an interrupt, as a slow disk can, so a receipt write can
// outlive its caller's deadline and still commit.
func init() {
	sqlite.MustRegisterScalarFunction("selfmind_test_pause", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if ms, ok := args[0].(int64); ok {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		return nil, nil
	})
}

// A receipt transition may run longer than its caller's deadline. It runs to
// completion and reports what it did, so an error always means nothing
// changed: a claim that committed returns its generation rather than a
// deadline, and a release, an acceptance, or a recorded reason that committed
// reports success.
func TestSlowReceiptTransitionReportsWhatItCommitted(t *testing.T) {
	for _, step := range []string{"claim", "release", "accept", "note"} {
		t.Run(step, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			owner := InboundOwner{TenantID: "default", PersonID: "person-a"}
			if _, err := store.BeginInbound(ctx, "weixin", "wx-slow", []byte(`{"content":"rotate the keys"}`), owner); err != nil {
				t.Fatal(err)
			}
			var claim int64
			if step != "claim" {
				var err error
				if claim, err = store.ClaimInbound(ctx, "weixin", "wx-slow"); err != nil || claim != 1 {
					t.Fatalf("claim = %d, %v", claim, err)
				}
			}
			if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER slow_receipt AFTER UPDATE ON inbound_dedup
				BEGIN SELECT selfmind_test_pause(300); END`); err != nil {
				t.Fatal(err)
			}
			deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
			var stepErr error
			switch step {
			case "claim":
				claim, stepErr = store.ClaimInbound(deadline, "weixin", "wx-slow")
			case "release":
				stepErr = store.ReleaseInbound(deadline, "weixin", "wx-slow", claim, "not dispatched: SelfMind is shutting down")
			case "accept":
				stepErr = store.AcceptInbound(deadline, "weixin", "wx-slow", claim)
			case "note":
				stepErr = store.NoteInboundFailure(deadline, "weixin", "wx-slow", claim, errors.New("gateway returned HTTP 500"))
			}
			want := map[string]InboundState{"claim": InboundDispatching, "release": InboundPending,
				"accept": InboundAccepted, "note": InboundDispatching}[step]
			state, generation, err := store.InboundReceiptClaim(ctx, "weixin", "wx-slow")
			if err != nil {
				t.Fatal(err)
			}
			if stepErr != nil || claim != 1 || state != want || generation != 1 {
				t.Fatalf("slow %s reported claim %d, %v and left the receipt %q under claim %d; want it reported as the commit it was",
					step, claim, stepErr, state, generation)
			}
			if step == "note" {
				rows, err := store.ListUncertainInboundForPerson(ctx, "default", "person-a", 20)
				if err != nil || len(rows) != 1 || rows[0].LastError != "gateway returned HTTP 500" {
					t.Fatalf("slow note = %+v, %v", rows, err)
				}
			}
		})
	}
}

// The caller's deadline still bounds the wait for the store. A claim that
// cannot start before the deadline reports it and changes nothing, and the
// next claim takes the receipt.
func TestReceiptTransitionDeadlineBoundsOnlyTheWait(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	owner := InboundOwner{TenantID: "default", PersonID: "person-a"}
	if _, err := store.BeginInbound(ctx, "weixin", "wx-wait", []byte(`{"content":"rotate the keys"}`), owner); err != nil {
		t.Fatal(err)
	}
	busy, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	claim, claimErr := store.ClaimInbound(deadline, "weixin", "wx-wait")
	if err := busy.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(claimErr, context.DeadlineExceeded) || claim != 0 {
		t.Fatalf("claim that never reached the store = %d, %v; want the deadline", claim, claimErr)
	}
	if state, generation, err := store.InboundReceiptClaim(ctx, "weixin", "wx-wait"); err != nil || state != InboundPending || generation != 0 {
		t.Fatalf("receipt after a claim that never started = %q claim %d, %v", state, generation, err)
	}
	if claim, err := store.ClaimInbound(ctx, "weixin", "wx-wait"); err != nil || claim != 1 {
		t.Fatalf("next claim = %d, %v", claim, err)
	}
}
