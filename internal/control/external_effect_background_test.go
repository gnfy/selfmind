package control

import (
	"context"
	"errors"
	"testing"
)

// Review round 2, item 2: a background start stays in flight while the daemon
// that started it lives, even after its owner Run stops running. Nothing can
// release it before its exit is recorded, and that exit can still be recorded.
func TestBackgroundDispatchStaysInFlightAfterItsOwnerFinishes(t *testing.T) {
	for _, finish := range []string{"done", "blocked"} {
		for _, exit := range []string{"non-zero exit", "exit zero"} {
			t.Run(finish+"/"+exit, func(t *testing.T) {
				ctx := context.Background()
				store := newTestStore(t)
				owner, runA, runB, _ := externalEffectRuns(t, store)
				claims := dispatchedEffect(t, store, owner, runA, "serve", true, UnknownExternalTarget)
				// The owner finishes first while its process still runs.
				if err := store.FinishRun(ctx, owner.TenantID, runA.ID, finish); err != nil {
					t.Fatal(err)
				}
				if got := effectDisposition(t, store, owner, runA, "serve"); got != ExternalClaimInFlight {
					t.Fatalf("background dispatch after its owner finished = %q", got)
				}
				start, err := store.AllocateEffectSequence(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.BindExternalEffectObservation(ctx, ExternalEffectObservation{TenantID: owner.TenantID, PersonID: owner.PersonID,
					ObserverRunID: runB.ID, ClaimID: claims[0].ID, ToolCallID: "check", StartSequence: start}); !errors.Is(err, ErrExternalEffectInFlight) {
					t.Fatalf("an observation was bound to a running background process: %v", err)
				}
				if _, err := store.SettleExternalEffect(ctx, ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
					ClaimID: claims[0].ID, ActorAccountID: owner.AccountID, ActorChannel: "cli"}); !errors.Is(err, ErrExternalEffectInFlight) {
					t.Fatalf("a running background process was settled: %v", err)
				}
				wait, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
					RunID: runB.ID, EffectID: "push", TargetKeys: []string{"repo:origin"}, DeferWait: true})
				if err != nil || wait.Granted || wait.NeedsApproval || wait.NeedsObservation {
					t.Fatalf("another Run did not wait for the running process: %+v %v", wait, err)
				}
				// The process exits; its result is recorded either way.
				if exit == "exit zero" {
					if err := store.ObserveExternalEffectGroup(ctx, owner.TenantID, owner.PersonID, runA.ID, "serve", "process:exited:0:p1:call-serve"); err != nil {
						t.Fatalf("exit 0 was not recorded: %v", err)
					}
					return
				}
				code := 3
				if err := store.RecordExternalEffectReturn(ctx, ExternalEffectReturn{TenantID: owner.TenantID, PersonID: owner.PersonID,
					RunID: runA.ID, EffectID: "serve", ToolCallID: "call-serve", Outcome: "background process exit 3", ExitCode: &code}); err != nil {
					t.Fatalf("the later non-zero exit was not recorded: %v", err)
				}
				if got := effectDisposition(t, store, owner, runA, "serve"); got != ExternalClaimLeftUncertain {
					t.Fatalf("background dispatch after a non-zero exit = %q", got)
				}
			})
		}
	}
}

// A later daemon process cannot observe the exit of a process the earlier one
// started: its background effect is no longer in flight, and boot recovery
// records it returned with an unknown exit, once. A foreground call of a
// crashed owner gets no such record; it is uncertain by its owner's state.
func TestBootRecoveryRecordsAbandonedBackgroundEffects(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, runB, _ := externalEffectRuns(t, store)
	dispatchedEffect(t, store, owner, runA, "serve", true, "service:dev")
	dispatchedEffect(t, store, owner, runB, "deploy", false, "service:prod")
	if n, err := store.RecordAbandonedBackgroundEffects(ctx); err != nil || n != 0 {
		t.Fatalf("this daemon's own running process was recorded abandoned: %d %v", n, err)
	}
	// The next daemon process opens the same database.
	restarted := &Store{db: store.db, dataDir: store.dataDir, events: store.events, instance: "daemon_after_restart"}
	if _, err := restarted.MarkInterruptedRuns(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := effectDisposition(t, restarted, owner, runA, "serve"); got != ExternalClaimLeftUncertain {
		t.Fatalf("an earlier daemon's background effect after restart = %q", got)
	}
	n, err := restarted.RecordAbandonedBackgroundEffects(ctx)
	if err != nil || n != 1 {
		t.Fatalf("abandoned background effects recorded = %d %v", n, err)
	}
	claims, err := restarted.ExternalEffectClaimsForEffect(ctx, owner.TenantID, owner.PersonID, runA.ID, "serve")
	if err != nil || claims[0].Disposition != ExternalClaimLeftUncertain || claims[0].Outcome != abandonedBackgroundOutcome {
		t.Fatalf("abandoned background claim: %+v %v", claims, err)
	}
	if again, err := restarted.RecordAbandonedBackgroundEffects(ctx); err != nil || again != 0 {
		t.Fatalf("boot recovery recorded twice: %d %v", again, err)
	}
	foreground, err := restarted.ExternalEffectClaimsForEffect(ctx, owner.TenantID, owner.PersonID, runB.ID, "deploy")
	if err != nil || foreground[0].Outcome != "" || foreground[0].Disposition != ExternalClaimLeftUncertain {
		t.Fatalf("a foreground call of a crashed owner: %+v %v", foreground, err)
	}
}
