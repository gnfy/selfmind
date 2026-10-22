package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/control"
)

// Review round 2, item 4: a Run parked on an effect of its own chain that a
// bound watcher observes is not told about another Run's call or a question
// the conflicting call would ask: while the watcher runs, the status names the
// watcher; once it ends without releasing the effect, /resume means the Run
// observes its own effect read-only first. A Run parked on another Run's call
// keeps those hints.
func TestParkedOnOwnWatchedEffectHintsTheRunsOwnObservation(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		name := map[bool]string{false: "own watched effect", true: "another run's call"}[foreign]
		t.Run(name, func(t *testing.T) {
			daemon, store, owner, task, _ := newApprovalTestServer(t)
			ctx := context.Background()
			first, err := store.StartRunWithOptions(ctx, task, "cli", "first effect", control.StartRunOptions{MaxActiveRuns: 2})
			if err != nil {
				t.Fatal(err)
			}
			waiter := first
			if foreign {
				otherTask, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "second", Channel: "cli"})
				if err != nil {
					t.Fatal(err)
				}
				if waiter, err = store.StartRunWithOptions(ctx, otherTask, "cli", "second effect", control.StartRunOptions{MaxActiveRuns: 2}); err != nil {
					t.Fatal(err)
				}
			}
			claim, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
				RunID: first.ID, EffectID: "first-effect", TargetKeys: []string{"service:alpha"}})
			if err != nil || !claim.Granted {
				t.Fatalf("claim %+v %v", claim, err)
			}
			if err := store.RecordExternalEffectDispatch(ctx, control.ExternalEffectDispatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
				RunID: first.ID, EffectID: "first-effect", ToolCallID: "deploy", ToolName: "terminal", Summary: "./deploy.sh alpha"}); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, first.ID, "first-effect"); err != nil {
				t.Fatal(err)
			}
			var watch *control.ExternalWatch
			if !foreign {
				exit := 1
				if err := store.RecordExternalEffectReturn(ctx, control.ExternalEffectReturn{TenantID: owner.TenantID, PersonID: owner.PersonID,
					RunID: first.ID, EffectID: "first-effect", ToolCallID: "deploy", Outcome: "exit 1", ExitCode: &exit}); err != nil {
					t.Fatal(err)
				}
				if watch, err = store.CreateExternalWatch(ctx, control.ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
					TaskID: first.TaskID, RunID: first.ID, Channel: "cli", CWD: t.TempDir(), Command: "./status.sh alpha", SuccessPattern: "^deployed$",
					PreflightReceipt: control.ExternalWatchPreflightReceipt{Version: control.ExternalWatchRecoveryReceiptVersion,
						RecoveryClaimID: claim.Claims[0].ID}}); err != nil {
					t.Fatal(err)
				}
			}
			decision, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
				RunID: waiter.ID, EffectID: "second-effect", TargetKeys: []string{"service:alpha"}})
			if err != nil || decision.Granted || decision.NeedsObservation || decision.NeedsApproval {
				t.Fatalf("the conflicting effect did not park: %+v %v", decision, err)
			}
			if err := store.FinishRun(ctx, owner.TenantID, waiter.ID, "waiting_external"); err != nil {
				t.Fatal(err)
			}
			detail := waitDetail(t, store, owner, waiter.ID)
			if foreign != strings.Contains(detail, "another run") || !foreign && !strings.Contains(detail, "watcher") {
				t.Fatalf("status of the parked wait: %s", detail)
			}

			// The producer ends without releasing the effect.
			if foreign {
				err = store.FinishRun(ctx, owner.TenantID, first.ID, "done")
			} else {
				_, err = store.FinishExternalWatch(ctx, owner.TenantID, watch.ID, control.ExternalWatchFailed, "pending", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			daemon.runExternalResourceWaitPass(ctx)
			outcome, ok := daemon.coordinator().latestStructuredRunOutcome(ctx, waiter.TaskID, waiter.ID)
			if !ok || outcome.CompletionReason != "external_effect_unresolved" {
				t.Fatalf("correction outcome: %+v", outcome)
			}
			for _, told := range []string{strings.Join(outcome.NextSteps, "\n"), waitDetail(t, store, owner, waiter.ID)} {
				if !strings.Contains(told, "/effects settle "+claim.Claims[0].ID) || !strings.Contains(told, "/resume "+waiter.ID) {
					t.Fatalf("blocked wait does not offer settle and resume: %s", told)
				}
				if asks := strings.Contains(told, "asks you once"); asks != foreign {
					t.Fatalf("blocked wait says the call asks you once = %v: %s", asks, told)
				}
				if !foreign && !strings.Contains(told, "observes its own earlier effect read-only") {
					t.Fatalf("blocked wait on its own effect does not say the run observes it first: %s", told)
				}
			}
		})
	}
}

// uncertainEffectAt dispatches run's effect on target and records that it
// returned without proof of success. tolerated names other Runs' claims the
// person let it pass once.
func uncertainEffectAt(t *testing.T, store *control.Store, owner *control.IdentityContext, run *control.Run, effectID, target string, tolerated ...string) string {
	t.Helper()
	ctx := context.Background()
	claim, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: run.ID, EffectID: effectID, TargetKeys: []string{target}, ToleratedClaimIDs: tolerated})
	if err != nil || !claim.Granted {
		t.Fatalf("claim %s: %+v %v", effectID, claim, err)
	}
	if err := store.RecordExternalEffectDispatch(ctx, control.ExternalEffectDispatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: run.ID, EffectID: effectID, ToolCallID: effectID, ToolName: "terminal", Summary: "./deploy.sh " + effectID}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, run.ID, effectID); err != nil {
		t.Fatal(err)
	}
	exit := 1
	if err := store.RecordExternalEffectReturn(ctx, control.ExternalEffectReturn{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: run.ID, EffectID: effectID, ToolCallID: effectID, Outcome: "exit 1", ExitCode: &exit}); err != nil {
		t.Fatal(err)
	}
	return claim.Claims[0].ID
}

// Review round 3, item 1: a Run parks on its own effect that a bound watcher
// still observes while another Run's effect on the same target was left
// uncertain. It waits for the watcher as the claim gate does: the wait pass
// leaves it waiting, and the status names the watcher and the other Run's
// effect the person must then decide about. When the watcher ends, with or
// without releasing the Run's own effect, the work is blocked with the
// decisions that remain, and it continues by itself once the person settles
// them.
func TestOwnWatcherBesideAnotherRunsUnresolvedEffect(t *testing.T) {
	for _, ending := range []string{"releases", "fails"} {
		t.Run("watcher "+ending, func(t *testing.T) {
			daemon, store, owner, task, _ := newApprovalTestServer(t)
			ctx := context.Background()
			run, err := store.StartRunWithOptions(ctx, task, "cli", "deploy", control.StartRunOptions{MaxActiveRuns: 3})
			if err != nil {
				t.Fatal(err)
			}
			otherTask, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "other", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			other, err := store.StartRunWithOptions(ctx, otherTask, "cli", "other deploy", control.StartRunOptions{MaxActiveRuns: 3})
			if err != nil {
				t.Fatal(err)
			}
			otherClaim := uncertainEffectAt(t, store, owner, other, "other-effect", "service:alpha")
			// The person let this Run's own effect pass the other one once; it
			// failed too, and a watcher bound to it still observes it.
			ownClaim := uncertainEffectAt(t, store, owner, run, "own-effect", "service:alpha", otherClaim)
			command := "./status.sh alpha"
			watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
				TaskID: run.TaskID, RunID: run.ID, Channel: "cli", CWD: t.TempDir(), Command: command, SuccessPattern: "^deployed$",
				PreflightReceipt: control.ExternalWatchPreflightReceipt{Version: control.ExternalWatchRecoveryReceiptVersion,
					RecoveryClaimID: ownClaim, CommandHash: fmt.Sprintf("%x", sha256.Sum256([]byte(command)))}})
			if err != nil {
				t.Fatal(err)
			}
			decision, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
				RunID: run.ID, EffectID: "second-effect", TargetKeys: []string{"service:alpha"}})
			if err != nil || decision.Granted || decision.NeedsObservation || decision.NeedsApproval {
				t.Fatalf("the conflicting effect did not park on the watcher: %+v %v", decision, err)
			}
			if err := store.FinishRun(ctx, owner.TenantID, run.ID, "waiting_external"); err != nil {
				t.Fatal(err)
			}

			daemon.runExternalResourceWaitPass(ctx)
			if got, err := store.GetRun(ctx, owner.TenantID, run.ID); err != nil || got.Status != "waiting_external" {
				t.Fatalf("a Run still waiting for its watcher was moved on: %+v %v", got, err)
			}
			detail := waitDetail(t, store, owner, run.ID)
			for _, want := range []string{"watcher", ownClaim, otherClaim, "/effects settle " + otherClaim, "your decision"} {
				if !strings.Contains(detail, want) {
					t.Fatalf("status while the watcher runs does not mention %q: %s", want, detail)
				}
			}
			if strings.Contains(detail, "another run's in-flight call") {
				t.Fatalf("status describes the wrong holder: %s", detail)
			}

			if ending == "releases" {
				if ok, err := store.FinishExternalWatch(ctx, owner.TenantID, watch.ID, control.ExternalWatchSucceeded, "deployed", ""); err != nil || !ok {
					t.Fatalf("finish: %v %v", ok, err)
				}
				if ok, err := store.MarkExternalWatchFinalized(ctx, owner.TenantID, watch.ID); err != nil || !ok {
					t.Fatalf("finalize: %v %v", ok, err)
				}
				if _, released, err := store.ReleaseExternalEffectForWatch(ctx, owner.TenantID, watch.ID); err != nil || !released {
					t.Fatalf("the watcher did not release the Run's own effect: %v %v", released, err)
				}
			} else if _, err := store.FinishExternalWatch(ctx, owner.TenantID, watch.ID, control.ExternalWatchFailed, "pending", ""); err != nil {
				t.Fatal(err)
			}
			daemon.runExternalResourceWaitPass(ctx)
			if got, err := store.GetRun(ctx, owner.TenantID, run.ID); err != nil || got.Status != "blocked" {
				t.Fatalf("a Run left with a decision after its watcher ended: %+v %v", got, err)
			}
			outcome, ok := daemon.coordinator().latestStructuredRunOutcome(ctx, run.TaskID, run.ID)
			if !ok || outcome.CompletionReason != "external_effect_unresolved" {
				t.Fatalf("blocked outcome: %+v", outcome)
			}
			steps := strings.Join(outcome.NextSteps, "\n")
			wantStep := "the conflicting call asks you once"
			if ending == "fails" {
				wantStep = "observes its own earlier effect read-only; another run's effect then asks you once"
			}
			if !strings.Contains(steps, wantStep) || !strings.Contains(steps, "/effects settle ") {
				t.Fatalf("blocked next steps: %s", steps)
			}

			// The person settles what remains; the work continues once.
			remaining := []string{otherClaim}
			if ending == "fails" {
				remaining = append(remaining, ownClaim)
			}
			for _, claimID := range remaining {
				if _, err := store.SettleExternalEffect(ctx, control.ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
					ClaimID: claimID, ActorAccountID: owner.AccountID, ActorChannel: "cli", Reason: "checked by hand"}); err != nil {
					t.Fatal(err)
				}
			}
			active := &activeRun{PersonID: owner.PersonID, RunID: other.ID, TaskID: otherTask.ID, Channel: "cli"}
			if !daemon.coordinator().beginActive(owner.PersonID, active) {
				t.Fatal("could not hold foreground slot")
			}
			defer daemon.coordinator().endActiveRun(owner.PersonID, active)
			daemon.runExternalResourceWaitPass(ctx)
			row, err := store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, "external-resource:"+run.ID+":second-effect")
			if err != nil || row == nil || row.ReplyToRunID != run.ID {
				t.Fatalf("the settled work did not continue: %+v %v", row, err)
			}
		})
	}
}

func waitDetail(t *testing.T, store *control.Store, owner *control.IdentityContext, runID string) string {
	t.Helper()
	waits, err := store.ListExternalResourceWaits(context.Background(), owner.TenantID, owner.PersonID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, wait := range waits {
		if wait.RunID == runID {
			return formatResourceWaitDetail(wait)
		}
	}
	t.Fatalf("no resource wait of %s in %+v", runID, waits)
	return ""
}
