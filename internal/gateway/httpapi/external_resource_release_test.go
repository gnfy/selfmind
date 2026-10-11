package httpapi

import (
	"context"
	"testing"

	"selfmind/internal/control"
)

// Item 8: a Run parked behind another Run's effect that was then left
// uncertain is blocked with resource_observation_required. Once every
// conflicting claim is released (by the person's settlement, a bound
// observation, or a successful return), the existing idempotent continuation
// path wakes it exactly once. Nothing wakes it before the release.
func TestReleasedClaimWakesBlockedResourceWait(t *testing.T) {
	for _, release := range []string{"settlement", "observation"} {
		t.Run(release, func(t *testing.T) {
			daemon, store, identity, taskA, _ := newApprovalTestServer(t)
			ctx := context.Background()
			owner, err := store.StartRunWithOptions(ctx, taskA, "cli", "publish", control.StartRunOptions{MaxActiveRuns: 3})
			if err != nil {
				t.Fatal(err)
			}
			taskB, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "push", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			waiter, err := store.StartRunWithOptions(ctx, taskB, "cli", "push", control.StartRunOptions{MaxActiveRuns: 3})
			if err != nil {
				t.Fatal(err)
			}
			held, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: identity.TenantID, PersonID: identity.PersonID,
				RunID: owner.ID, EffectID: "publish", TargetKeys: []string{control.UnknownExternalTarget}})
			if err != nil || !held.Granted {
				t.Fatalf("claim: %+v %v", held, err)
			}
			if err := store.RecordExternalEffectDispatch(ctx, control.ExternalEffectDispatch{TenantID: identity.TenantID, PersonID: identity.PersonID,
				RunID: owner.ID, EffectID: "publish", ToolCallID: "publish", ToolName: "terminal", Summary: "npm publish"}); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkExternalEffectPossible(ctx, identity.TenantID, owner.ID, "publish"); err != nil {
				t.Fatal(err)
			}
			// The owner's call is in flight: the waiter parks behind it.
			if wait, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: identity.TenantID, PersonID: identity.PersonID,
				RunID: waiter.ID, EffectID: "push", TargetKeys: []string{"repo:origin"}}); err != nil || wait.Granted || wait.NeedsApproval || wait.NeedsObservation {
				t.Fatalf("waiter did not park behind an in-flight call: %+v %v", wait, err)
			}
			if err := store.FinishRun(ctx, identity.TenantID, waiter.ID, "waiting_external"); err != nil {
				t.Fatal(err)
			}
			// The call fails: its claim is left uncertain and nothing will
			// release it by itself.
			exit := 1
			if err := store.RecordExternalEffectReturn(ctx, control.ExternalEffectReturn{TenantID: identity.TenantID, PersonID: identity.PersonID,
				RunID: owner.ID, EffectID: "publish", ToolCallID: "publish", Outcome: "exit 1", ExitCode: &exit}); err != nil {
				t.Fatal(err)
			}
			daemon.runExternalResourceWaitPass(ctx)
			parked, err := store.GetRun(ctx, identity.TenantID, waiter.ID)
			if err != nil || parked.Status != "blocked" {
				t.Fatalf("waiter behind a left-uncertain effect: %+v %v", parked, err)
			}
			outcome, ok := daemon.coordinator().latestStructuredRunOutcome(ctx, taskB.ID, waiter.ID)
			if !ok || outcome.CompletionReason != "external_effect_unresolved" {
				t.Fatalf("blocked outcome: %+v", outcome)
			}
			key := "external-resource:" + waiter.ID + ":push"
			daemon.runExternalResourceWaitPass(ctx)
			if row, err := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, key); err != nil || row != nil {
				t.Fatalf("blocked waiter woke before the release: %+v %v", row, err)
			}

			switch release {
			case "settlement":
				if _, err := store.SettleExternalEffect(ctx, control.ExternalEffectSettlementRequest{TenantID: identity.TenantID, PersonID: identity.PersonID,
					ClaimID: held.Claims[0].ID, ActorAccountID: identity.AccountID, ActorChannel: "cli", Reason: "published by hand"}); err != nil {
					t.Fatal(err)
				}
			case "observation":
				if err := store.ObserveExternalEffectGroup(ctx, identity.TenantID, identity.PersonID, owner.ID, "publish", "observation:"+owner.ID+":check"); err != nil {
					t.Fatal(err)
				}
			}
			// Hold the foreground slot so the queued wakeup stays inspectable.
			active := &activeRun{PersonID: identity.PersonID, RunID: owner.ID, TaskID: taskA.ID, Channel: "cli"}
			if !daemon.coordinator().beginActive(identity.PersonID, active) {
				t.Fatal("could not hold foreground slot")
			}
			defer daemon.coordinator().endActiveRun(identity.PersonID, active)
			daemon.runExternalResourceWaitPass(ctx)
			daemon.runExternalResourceWaitPass(ctx)
			row, err := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, key)
			if err != nil || row == nil || row.TaskID != taskB.ID || row.ReplyToRunID != waiter.ID {
				t.Fatalf("released claim did not wake the blocked Run's exact continuation: %+v %v", row, err)
			}
			queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
			if err != nil {
				t.Fatal(err)
			}
			wakes := 0
			for _, item := range queued {
				if item.IdempotencyKey == key {
					wakes++
				}
			}
			if wakes != 1 {
				t.Fatalf("wakeups for one release = %d", wakes)
			}
		})
	}
}

// A Run blocked before this change was told to /resume itself; releasing its
// claim later does not start it on its own.
func TestHistoricalBlockedResourceWaitStaysWithThePerson(t *testing.T) {
	daemon, store, identity, taskA, _ := newApprovalTestServer(t)
	ctx := context.Background()
	owner, err := store.StartRunWithOptions(ctx, taskA, "cli", "publish", control.StartRunOptions{MaxActiveRuns: 3})
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "push", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	waiter, err := store.StartRunWithOptions(ctx, taskB, "cli", "push", control.StartRunOptions{MaxActiveRuns: 3})
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: owner.ID, EffectID: "publish", TargetKeys: []string{"service:legacy"}}); err != nil || !decision.Granted {
		t.Fatalf("claim: %+v %v", decision, err)
	}
	if err := store.MarkExternalEffectPossible(ctx, identity.TenantID, owner.ID, "publish"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: waiter.ID, EffectID: "push", TargetKeys: []string{"service:legacy"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, waiter.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, owner.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	waits, err := store.ListExternalResourceWaits(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil || len(waits) != 1 {
		t.Fatalf("waits: %+v %v", waits, err)
	}
	// The correction an older daemon wrote carries no wake marker.
	if _, err := store.MaterializeRunFinalization(ctx, control.RunFinalization{Identity: *identity, RunID: waiter.ID, TaskID: taskB.ID,
		RunStatus: "blocked", ExpectedRunStatus: "waiting_external", ResourceWait: &waits[0].ExternalResourceWait, Summary: "Observe the effect",
		Event: control.Event{Type: "run.finished", IdempotencyKey: "resource-observation:" + waiter.ID + ":push",
			Payload: []byte(`{"outcome":{"status":"blocked","completion_reason":"external_effect_unresolved","resumable":true},"resource_observation_required":true}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveExternalEffectGroup(ctx, identity.TenantID, identity.PersonID, owner.ID, "publish", "observer:terminal"); err != nil {
		t.Fatal(err)
	}
	daemon.runExternalResourceWaitPass(ctx)
	if row, err := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, "external-resource:"+waiter.ID+":push"); err != nil || row != nil {
		t.Fatalf("a historical blocked Run woke on its own: %+v %v", row, err)
	}
}
