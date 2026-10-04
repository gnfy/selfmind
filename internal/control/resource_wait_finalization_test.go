package control

import (
	"context"
	"testing"
)

func TestResourceWaitCorrectionRollbackAndReplay(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, first, waiting, _ := externalEffectRuns(t, store)
	if claim, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: first.ID, EffectID: "effect", TargetKeys: []string{"service:test"}}); err != nil || !claim.Granted {
		t.Fatalf("claim %+v %v", claim, err)
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, first.ID, "effect"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: waiting.ID, EffectID: "blocked", TargetKeys: []string{"service:test"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, waiting.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, first.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	waits, err := store.ListExternalResourceWaits(ctx, owner.TenantID, owner.PersonID, 100)
	if err != nil || len(waits) != 1 || !waits[0].NeedsObservation {
		t.Fatalf("wait %+v %v", waits, err)
	}
	input := RunFinalization{Identity: *owner, RunID: waiting.ID, TaskID: waiting.TaskID, RunStatus: "blocked", ExpectedRunStatus: "waiting_external", ResourceWait: &waits[0].ExternalResourceWait, Summary: "Observe the effect", NextSteps: []string{"read-only observation"},
		Event: Event{Type: "run.finished", IdempotencyKey: "resource-correction", Payload: []byte(`{"outcome":{"status":"blocked","completion_reason":"external_effect_unresolved","resumable":true},"resource_observation_required":true}`)}}
	changed := input
	changedWait := waits[0].ExternalResourceWait
	changedWait.TargetKeys = []string{"service:wrong"}
	changed.ResourceWait = &changedWait
	if _, err := store.MaterializeRunFinalization(ctx, changed); err == nil {
		t.Fatal("a stale target snapshot changed the parked Run")
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_resource_result BEFORE INSERT ON task_events WHEN NEW.type='run.finished' BEGIN SELECT RAISE(ABORT,'injected result failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, input); err == nil {
		t.Fatal("injected commit failure was ignored")
	}
	run, err := store.GetRun(ctx, owner.TenantID, waiting.ID)
	if err != nil || run.Status != "waiting_external" {
		t.Fatalf("partial state committed %+v %v", run, err)
	}
	if notices, err := store.ListResourceObservationNotices(ctx); err != nil || len(notices) != 0 {
		t.Fatalf("notice before commit %+v %v", notices, err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_resource_result`); err != nil {
		t.Fatal(err)
	}
	firstEvent, err := store.MaterializeRunFinalization(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.MaterializeRunFinalization(ctx, input)
	if err != nil || replayed.ID != firstEvent.ID {
		t.Fatalf("commit replay changed result %+v %v", replayed, err)
	}
	if notices, err := store.ListResourceObservationNotices(ctx); err != nil || len(notices) != 1 {
		t.Fatalf("post-commit notice lost/duplicated %+v %v", notices, err)
	}
	claims, err := store.ListUnresolvedExternalEffects(ctx, owner.TenantID, owner.PersonID, 100)
	if err != nil || len(claims) != 1 || claims[0].State != ExternalClaimUncertain {
		t.Fatalf("failed correction cleared effects %+v %v", claims, err)
	}
}

func TestBoundWatchKeepsSelfConflictObservableUntilFinalized(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, run, _, _ := externalEffectRuns(t, store)
	if _, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID, EffectID: "effect", TargetKeys: []string{"service:beta"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, run.ID, "effect"); err != nil {
		t.Fatal(err)
	}
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: run.TaskID, RunID: run.ID, Channel: "cli", CWD: t.TempDir(), Command: "printf READY", SuccessPattern: "^READY$", PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchContinuationReceiptVersion, EffectID: "effect", EffectRuleKey: "reviewed-rule"}})
	if err != nil {
		t.Fatal(err)
	}
	request := ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID, EffectID: "next", TargetKeys: []string{"service:beta"}}
	decision, err := store.ClaimExternalEffects(ctx, request)
	if err != nil || decision.Granted || decision.NeedsObservation {
		t.Fatalf("bound observer was ignored: %+v %v", decision, err)
	}
	if _, err := store.FinishExternalWatch(ctx, owner.TenantID, watch.ID, ExternalWatchSucceeded, "READY", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkExternalWatchFinalized(ctx, owner.TenantID, watch.ID); err != nil {
		t.Fatal(err)
	}
	decision, err = store.ClaimExternalEffects(ctx, request)
	if err != nil || decision.Granted || !decision.NeedsObservation {
		t.Fatalf("a finalized observer falsely promised release: %+v %v", decision, err)
	}
}

func TestSelfConflictRequiresObservationAndUnboundWatchIsInsufficient(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, run, _, _ := externalEffectRuns(t, store)
	if _, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID, EffectID: "first", TargetKeys: []string{UnknownExternalTarget}}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, run.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateExternalWatch(ctx, ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: run.TaskID, RunID: run.ID, Channel: "cli", CWD: t.TempDir(), Command: "printf READY", SuccessPattern: "^READY$"}); err != nil {
		t.Fatal(err)
	}
	decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID, EffectID: "second", TargetKeys: []string{"service:changed-target"}})
	if err != nil || !decision.NeedsObservation || decision.Granted || len(decision.BlockingClaims) != 1 {
		t.Fatalf("unbound watch falsely guaranteed release %+v %v", decision, err)
	}
	if pending, err := store.IsRunExternalResourceWaitPending(ctx, owner.TenantID, run.ID); err != nil || pending {
		t.Fatalf("self wait was parked %+v %v", pending, err)
	}
}
