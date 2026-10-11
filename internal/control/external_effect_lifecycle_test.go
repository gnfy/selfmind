package control

import (
	"context"
	"errors"
	"testing"
)

// dispatchedEffect claims targets for run, records the dispatch and crosses
// the dispatch boundary: the call is now in flight until it returns.
func dispatchedEffect(t *testing.T, store *Store, owner *IdentityContext, run *Run, effectID string, background bool, targets ...string) []ExternalEffectClaim {
	t.Helper()
	ctx := context.Background()
	decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID,
		PersonID: owner.PersonID, RunID: run.ID, EffectID: effectID, TargetKeys: targets})
	if err != nil || !decision.Granted {
		t.Fatalf("claim %s: %+v %v", effectID, decision, err)
	}
	if err := store.RecordExternalEffectDispatch(ctx, ExternalEffectDispatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: run.ID, EffectID: effectID, ToolCallID: "call-" + effectID, ToolName: "terminal",
		Summary: "deploy --target " + effectID, Background: background}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, run.ID, effectID); err != nil {
		t.Fatal(err)
	}
	return decision.Claims
}

func returnedEffect(t *testing.T, store *Store, owner *IdentityContext, run *Run, effectID string, exit int) {
	t.Helper()
	if err := store.RecordExternalEffectReturn(context.Background(), ExternalEffectReturn{TenantID: owner.TenantID,
		PersonID: owner.PersonID, RunID: run.ID, EffectID: effectID, ToolCallID: "call-" + effectID,
		Outcome: "exit 1", ExitCode: &exit}); err != nil {
		t.Fatal(err)
	}
}

func effectDisposition(t *testing.T, store *Store, owner *IdentityContext, run *Run, effectID string) string {
	t.Helper()
	claims, err := store.ExternalEffectClaimsForEffect(context.Background(), owner.TenantID, owner.PersonID, run.ID, effectID)
	if err != nil || len(claims) == 0 {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	return claims[0].Disposition
}

// A dispatched call is in flight while it has not returned and its owner Run
// is running. A recorded failed return, or the owner's crash (boot recovery
// marks it interrupted), leaves it uncertain instead. Nothing infers success.
func TestEffectIsInFlightUntilItsCallReturnsOrItsOwnerCrashes(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, store *Store, owner *IdentityContext, run *Run)
	}{
		{"failed return", func(t *testing.T, store *Store, owner *IdentityContext, run *Run) {
			returnedEffect(t, store, owner, run, "deploy", 1)
		}},
		{"crash after dispatch", func(t *testing.T, store *Store, owner *IdentityContext, run *Run) {
			if _, err := store.MarkInterruptedRuns(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			owner, runA, runB, _ := externalEffectRuns(t, store)
			claims := dispatchedEffect(t, store, owner, runA, "deploy", false, UnknownExternalTarget)
			if got := effectDisposition(t, store, owner, runA, "deploy"); got != ExternalClaimInFlight {
				t.Fatalf("dispatched call disposition = %q", got)
			}
			waiting, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
				RunID: runB.ID, EffectID: "push", TargetKeys: []string{"repo:origin"}, DeferWait: true})
			if err != nil || waiting.Granted || waiting.NeedsObservation || waiting.NeedsApproval || waiting.BlockedByRun != runA.ID {
				t.Fatalf("other Run did not wait for an in-flight call: %+v %v", waiting, err)
			}
			if _, err := store.SettleExternalEffect(ctx, ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
				ClaimID: claims[0].ID, ActorAccountID: owner.AccountID, ActorChannel: "cli"}); !errors.Is(err, ErrExternalEffectInFlight) {
				t.Fatalf("settle while in flight: %v", err)
			}
			if resourceWaitRowCount(t, store) != 0 {
				t.Fatal("deferred wait wrote a durable resource wait")
			}

			tc.end(t, store, owner, runA)
			if got := effectDisposition(t, store, owner, runA, "deploy"); got != ExternalClaimLeftUncertain {
				t.Fatalf("ended call disposition = %q", got)
			}
			if released, err := store.ObserveUndispatchedExternalEffects(ctx); err != nil || released != 0 {
				t.Fatalf("recovery released a dispatched effect: %d %v", released, err)
			}
			decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
				RunID: runB.ID, EffectID: "push", TargetKeys: []string{"repo:origin"}})
			if tc.name == "crash after dispatch" {
				// Boot recovery ended every running Run, the waiter included.
				if err == nil {
					t.Fatalf("an interrupted Run claimed an effect: %+v", decision)
				}
			} else if err != nil || decision.Granted || !decision.NeedsApproval || len(decision.ApprovalClaims) != 1 || decision.ApprovalClaims[0].ID != claims[0].ID {
				t.Fatalf("other Run did not need approval for a left-uncertain effect: %+v %v", decision, err)
			}
			settled, err := store.SettleExternalEffect(ctx, ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
				ClaimID: claims[0].ID, ActorAccountID: owner.AccountID, ActorChannel: "cli", Reason: "verified by hand"})
			if err != nil || settled.Replayed || settled.Claims[0].State != ExternalClaimObserved {
				t.Fatalf("settle of a left-uncertain effect: %+v %v", settled, err)
			}
		})
	}
}

// The Run's own chain never waits on itself: its own foreground call has
// returned because it is calling again, while its own running background
// process is still in flight.
func TestOwnChainUnresolvedEffectNeedsObservationNotWait(t *testing.T) {
	for _, background := range []bool{false, true} {
		ctx := context.Background()
		store := newTestStore(t)
		owner, runA, _, _ := externalEffectRuns(t, store)
		dispatchedEffect(t, store, owner, runA, "first", background, "service:alpha")
		decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
			RunID: runA.ID, EffectID: "second", TargetKeys: []string{"service:alpha"}})
		if err != nil || decision.Granted || !decision.NeedsObservation || len(decision.BlockingClaims) != 1 {
			t.Fatalf("background=%v: own effect did not need observation: %+v %v", background, decision, err)
		}
		want := ExternalClaimLeftUncertain
		if background {
			want = ExternalClaimInFlight
		}
		if got := decision.BlockingClaims[0]; !got.SameChain || got.Disposition != want || got.Summary != "deploy --target first" {
			t.Fatalf("background=%v: own blocker facts %+v", background, got)
		}
		if resourceWaitRowCount(t, store) != 0 {
			t.Fatalf("background=%v: own conflict parked the Run on itself", background)
		}
	}
}

// resourceWaitRowCount counts durable resource waits.
func resourceWaitRowCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM external_resource_waits`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
