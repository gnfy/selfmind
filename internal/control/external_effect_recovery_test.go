package control

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestExactEffectRecoveryNeedsExplicitClaimAndFreshObservation(t *testing.T) {
	ctx := context.Background()
	store, owner, task, parent := newRecoveryFixture(t)
	decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: parent.ID, EffectID: "unknown-command", TargetKeys: []string{UnknownExternalTarget}})
	if err != nil || !decision.Granted {
		t.Fatalf("claim: %+v %v", decision, err)
	}
	claim := decision.Claims[0]
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, parent.ID, claim.EffectID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, parent.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "Observe the old effect", StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	command := "printf SUCCEEDED"
	watch := func(version int, claimID string) *ExternalWatch {
		w, err := store.CreateExternalWatch(ctx, ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: child.TaskID, RunID: child.ID, Channel: "cli", CWD: t.TempDir(), Command: command, SuccessPattern: "^SUCCEEDED$", Status: ExternalWatchSucceeded, LastOutput: "SUCCEEDED", PreflightReceipt: ExternalWatchPreflightReceipt{Version: version, RecoveryClaimID: claimID, CommandHash: fmt.Sprintf("%x", sha256.Sum256([]byte(command)))}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.MarkExternalWatchFinalized(ctx, owner.TenantID, w.ID); err != nil {
			t.Fatal(err)
		}
		return w
	}
	legacy := watch(ExternalWatchContinuationReceiptVersion, "")
	if _, err := store.ObserveExternalEffectWithWatch(ctx, owner.TenantID, owner.PersonID, claim.ID, legacy.ID); err == nil {
		t.Fatal("legacy receipt gained cross-Run authority")
	}
	explicit := watch(ExternalWatchRecoveryReceiptVersion, claim.ID)
	if got, err := store.ObserveExternalEffectWithWatch(ctx, owner.TenantID, owner.PersonID, claim.ID, explicit.ID); err != nil || got.State != ExternalClaimObserved {
		t.Fatalf("exact recovery: %+v %v", got, err)
	}
}

func TestRecoveryObservationSaveFailurePreservesEffect(t *testing.T) {
	ctx := context.Background()
	store, owner, _, run := newRecoveryFixture(t)
	claim, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID, EffectID: "uncertain-save", TargetKeys: []string{UnknownExternalTarget}})
	if err != nil || !claim.Granted {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, run.ID, "uncertain-save"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_recovery_save BEFORE UPDATE ON external_watches WHEN NEW.finalized=1 BEGIN SELECT RAISE(ABORT,'injected observation save failure'); END`); err != nil {
		t.Fatal(err)
	}
	command := "printf SUCCEEDED"
	_, err = store.CreateExternalWatch(ctx, ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: run.TaskID, RunID: run.ID, Channel: "cli", CWD: t.TempDir(), Command: command, SuccessPattern: "^SUCCEEDED$", Status: ExternalWatchSucceeded, LastOutput: "SUCCEEDED", PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchRecoveryReceiptVersion, RecoveryClaimID: claim.Claims[0].ID, CommandHash: fmt.Sprintf("%x", sha256.Sum256([]byte(command)))}})
	if err == nil {
		t.Fatal("save fault wasn't surfaced")
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_watches`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("partial observation survived rollback: %d %v", rows, err)
	}
	effects, err := store.ListUnresolvedExternalEffects(ctx, owner.TenantID, owner.PersonID, 10)
	if err != nil || len(effects) != 1 || effects[0].State != ExternalClaimUncertain {
		t.Fatalf("effect erased after failed observation save: %+v %v", effects, err)
	}
}

func TestRecoveryBindingRejectsUnrelatedRunAndInertReceipt(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, parent, peer, _ := externalEffectRuns(t, store)
	claim, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: parent.ID, EffectID: "guarded", TargetKeys: []string{UnknownExternalTarget}})
	if err != nil || !claim.Granted {
		t.Fatal(err)
	}
	if _, err := store.ExternalEffectForObservation(ctx, owner.TenantID, owner.PersonID, peer.ID, claim.Claims[0].ID); err == nil {
		t.Fatal("Thread/person peer became an exact continuation")
	}
	_, err = store.CreateExternalWatch(ctx, ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: parent.TaskID, RunID: parent.ID, CWD: t.TempDir(), Command: "printf SUCCEEDED", SuccessPattern: "SUCCEEDED", PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchContinuationReceiptVersion, RecoveryClaimID: claim.Claims[0].ID}})
	if err == nil {
		t.Fatal("old receipt activated recovery binding")
	}
}

func TestRecoveryObservationSurvivesEventSaveFailure(t *testing.T) {
	ctx := context.Background()
	store, owner, _, run := newRecoveryFixture(t)
	decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID, EffectID: "uncertain-delivery", TargetKeys: []string{UnknownExternalTarget}})
	if err != nil || !decision.Granted {
		t.Fatalf("claim: %+v %v", decision, err)
	}
	claim := decision.Claims[0]
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, run.ID, claim.EffectID); err != nil {
		t.Fatal(err)
	}
	command := "printf SUCCEEDED"
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: run.TaskID, RunID: run.ID, Channel: "cli", CWD: t.TempDir(), Command: command, SuccessPattern: "SUCCEEDED", Status: ExternalWatchSucceeded, LastOutput: "SUCCEEDED", PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchRecoveryReceiptVersion, RecoveryClaimID: claim.ID, CommandHash: fmt.Sprintf("%x", sha256.Sum256([]byte(command)))}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_recovery_event BEFORE INSERT ON task_events BEGIN SELECT RAISE(ABORT,'injected notification save failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: run.TaskID, RunID: run.ID, Channel: "cli", Type: "external_watch.created"}); err == nil {
		t.Fatal("event save failure was swallowed")
	}
	saved, err := store.GetExternalWatch(ctx, owner.TenantID, watch.ID)
	if err != nil || saved.Status != ExternalWatchSucceeded || !saved.Finalized {
		t.Fatalf("committed evidence disappeared: %+v %v", saved, err)
	}
	effects, err := store.ListUnresolvedExternalEffects(ctx, owner.TenantID, owner.PersonID, 10)
	if err != nil || len(effects) != 1 || effects[0].State != ExternalClaimUncertain {
		t.Fatalf("missing notification released an uncertain effect: %+v %v", effects, err)
	}
}
