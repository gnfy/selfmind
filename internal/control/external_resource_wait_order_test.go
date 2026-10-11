package control

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
)

// claimFor claims run's effect on target, passing the tolerated claims once,
// and records its dispatch. returned also records a failed return.
func claimFor(t *testing.T, store *Store, owner *IdentityContext, run *Run, effectID, target string, returned bool, tolerated ...string) string {
	t.Helper()
	ctx := context.Background()
	decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: run.ID, EffectID: effectID, TargetKeys: []string{target}, ToleratedClaimIDs: tolerated})
	if err != nil || !decision.Granted {
		t.Fatalf("claim %s: %+v %v", effectID, decision, err)
	}
	if err := store.RecordExternalEffectDispatch(ctx, ExternalEffectDispatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: run.ID, EffectID: effectID, ToolCallID: "call-" + effectID, ToolName: "terminal", Summary: "deploy " + effectID}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, run.ID, effectID); err != nil {
		t.Fatal(err)
	}
	if returned {
		returnedEffect(t, store, owner, run, effectID, 1)
	}
	return decision.Claims[0].ID
}

func parkedWait(t *testing.T, store *Store, owner *IdentityContext, run *Run) ExternalResourceWaitProjection {
	t.Helper()
	waits, err := store.ListExternalResourceWaits(context.Background(), owner.TenantID, owner.PersonID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, wait := range waits {
		if wait.RunID == run.ID {
			return wait
		}
	}
	t.Fatalf("no parked wait of %s: %+v", run.ID, waits)
	return ExternalResourceWaitProjection{}
}

// Review round 3, item 1: a parked wait follows the claim gate's order. While
// a conflicting claim is still waited for (this work's own effect a bound
// watcher observes, or another Run's call in flight), another Run's
// left-uncertain claim does not yet need a decision, so the wait is not
// turned into a blocked Run; the park also names that claim as the decision
// left for after the wait. Once nothing is waited for, it needs one.
func TestParkedWaitNeedsADecisionOnlyAfterWhatItWaitsFor(t *testing.T) {
	for _, waitedFor := range []string{"own watched effect", "another run's call in flight"} {
		t.Run(waitedFor, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			owner, runA, runB, runC := externalEffectRuns(t, store)
			left := claimFor(t, store, owner, runB, "left", "service:alpha", true)
			waiter := runA
			var watch *ExternalWatch
			if waitedFor == "own watched effect" {
				own := claimFor(t, store, owner, runA, "own", "service:alpha", true, left)
				command := "./status.sh alpha"
				var err error
				if watch, err = store.CreateExternalWatch(ctx, ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
					TaskID: runA.TaskID, RunID: runA.ID, Channel: "cli", CWD: t.TempDir(), Command: command, SuccessPattern: "^deployed$",
					PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchRecoveryReceiptVersion, RecoveryClaimID: own,
						CommandHash: fmt.Sprintf("%x", sha256.Sum256([]byte(command)))}}); err != nil {
					t.Fatal(err)
				}
			} else {
				claimFor(t, store, owner, runC, "running", "service:alpha", false, left)
			}
			decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
				RunID: waiter.ID, EffectID: "next", TargetKeys: []string{"service:alpha"}})
			if err != nil || decision.Granted || decision.NeedsObservation || decision.NeedsApproval ||
				len(decision.DecideAfterWait) != 1 || decision.DecideAfterWait[0].ID != left {
				t.Fatalf("the call did not wait and name the decision left after it: %+v %v", decision, err)
			}
			if err := store.FinishRun(ctx, owner.TenantID, waiter.ID, "waiting_external"); err != nil {
				t.Fatal(err)
			}
			if wait := parkedWait(t, store, owner, waiter); wait.NeedsObservation || wait.Ready {
				t.Fatalf("a wait still waiting for a claim needs a decision now: %+v", wait)
			}
			if watch != nil {
				if _, err := store.FinishExternalWatch(ctx, owner.TenantID, watch.ID, ExternalWatchFailed, "pending", ""); err != nil {
					t.Fatal(err)
				}
			} else if err := store.ObserveExternalEffectGroup(ctx, owner.TenantID, owner.PersonID, runC.ID, "running", "tool:completed:call-running"); err != nil {
				t.Fatal(err)
			}
			if wait := parkedWait(t, store, owner, waiter); !wait.NeedsObservation || wait.Ready {
				t.Fatalf("nothing is waited for any more, yet no decision is needed: %+v", wait)
			}
		})
	}
}
