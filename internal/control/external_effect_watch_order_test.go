package control

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func finishedWatch(t *testing.T, store *Store, owner *IdentityContext, run *Run, receipt ExternalWatchPreflightReceipt) *ExternalWatch {
	t.Helper()
	ctx := context.Background()
	command := "printf SUCCEEDED"
	receipt.CommandHash = fmt.Sprintf("%x", sha256.Sum256([]byte(command)))
	if receipt.Version == 0 {
		receipt.Version = ExternalWatchContinuationReceiptVersion
	}
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
		TaskID: run.TaskID, RunID: run.ID, Channel: "cli", CWD: t.TempDir(), Command: command, SuccessPattern: "^SUCCEEDED$",
		PreflightReceipt: receipt})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.FinishExternalWatch(ctx, owner.TenantID, watch.ID, ExternalWatchSucceeded, "SUCCEEDED", ""); err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	if ok, err := store.MarkExternalWatchFinalized(ctx, owner.TenantID, watch.ID); err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
	return watch
}

// Item 7: a watcher counts as an observation of a claim only when it was
// registered after the claim by the shared sequence. Rows from before schema
// v26 (sequence 0) fall back to strictly later timestamps, and only when both
// rows are that old.
func TestWatcherObservationCountsOnlyAfterTheClaimBySequence(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, _, _ := externalEffectRuns(t, store)
	// Registered in the same second as the claim, but before it.
	early := finishedWatch(t, store, owner, runA, ExternalWatchPreflightReceipt{})
	claims := uncertainEffect(t, store, owner, runA, "deploy", UnknownExternalTarget)
	if _, err := store.db.ExecContext(ctx, `UPDATE external_watches SET created_at=(SELECT created_at FROM external_effect_claims WHERE id=?) WHERE id=?`, claims[0].ID, early.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ObserveExternalEffectWithWatch(ctx, owner.TenantID, owner.PersonID, claims[0].ID, early.ID); err == nil {
		t.Fatal("a watcher registered before the claim released it")
	}
	late := finishedWatch(t, store, owner, runA, ExternalWatchPreflightReceipt{})
	if _, err := store.db.ExecContext(ctx, `UPDATE external_watches SET created_at=created_at-60 WHERE id=?`, late.ID); err != nil {
		t.Fatal(err)
	}
	// A clock step back does not matter: the sequence is the order.
	observed, err := store.ObserveExternalEffectWithWatch(ctx, owner.TenantID, owner.PersonID, claims[0].ID, late.ID)
	if err != nil || observed.State != ExternalClaimObserved {
		t.Fatalf("a watcher registered after the claim did not release it: %+v %v", observed, err)
	}

	// Pre-v26 rows have no recorded order: only a strictly later watcher counts.
	legacy := uncertainEffect(t, store, owner, runA, "legacy", "service:legacy")
	same := finishedWatch(t, store, owner, runA, ExternalWatchPreflightReceipt{})
	later := finishedWatch(t, store, owner, runA, ExternalWatchPreflightReceipt{})
	if _, err := store.db.ExecContext(ctx, `UPDATE external_effect_claims SET sequence=0, created_at=1000 WHERE id=?`, legacy[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE external_watches SET sequence=0, created_at=1000 WHERE id=?`, same.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE external_watches SET sequence=0, created_at=1001 WHERE id=?`, later.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ObserveExternalEffectWithWatch(ctx, owner.TenantID, owner.PersonID, legacy[0].ID, same.ID); err == nil {
		t.Fatal("an equally old legacy watcher released a legacy claim")
	}
	if _, err := store.ObserveExternalEffectWithWatch(ctx, owner.TenantID, owner.PersonID, legacy[0].ID, later.ID); err != nil {
		t.Fatalf("a strictly later legacy watcher did not release a legacy claim: %v", err)
	}
}

// Item 7: an observation from a later Run of the claim's exact chain may
// release it, through an explicit claim binding or an owner-reviewed effect
// profile. A Run outside the chain may not.
func TestContinuationChainWatchersCanRelease(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, runB, _ := externalEffectRuns(t, store)
	claims := uncertainEffect(t, store, owner, runA, "deploy", "cluster:east")
	if err := store.FinishRun(ctx, owner.TenantID, runA.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(ctx, owner.TenantID, runA.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{MaxActiveRuns: 3, ResumesRunID: runA.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateExternalWatch(ctx, ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
		TaskID: runB.TaskID, RunID: runB.ID, Channel: "cli", CWD: t.TempDir(), Command: "printf SUCCEEDED", SuccessPattern: "^SUCCEEDED$",
		PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchRecoveryReceiptVersion, RecoveryClaimID: claims[0].ID}}); err == nil {
		t.Fatal("a Run outside the chain bound an observation to the claim")
	}
	bound := finishedWatch(t, store, owner, child, ExternalWatchPreflightReceipt{Version: ExternalWatchRecoveryReceiptVersion, RecoveryClaimID: claims[0].ID})
	released, ok, err := store.ReleaseExternalEffectForWatch(ctx, owner.TenantID, bound.ID)
	if err != nil || !ok || released.ID != claims[0].ID {
		t.Fatalf("bound continuation watcher did not release the claim: %+v %v %v", released, ok, err)
	}
	group, err := store.ExternalEffectClaimsForEffect(ctx, owner.TenantID, owner.PersonID, runA.ID, "deploy")
	if err != nil || group[0].State != ExternalClaimObserved || group[0].ObservationRef != "watch:"+bound.ID+":r1:bound" {
		t.Fatalf("release ref: %+v %v", group, err)
	}
	if _, again, err := store.ReleaseExternalEffectForWatch(ctx, owner.TenantID, bound.ID); err != nil || again {
		t.Fatalf("replay released again: %v %v", again, err)
	}

	// An owner-reviewed effect profile watcher of the continuation finds and
	// releases the effect its parent dispatched.
	parentEffect := uncertainEffectOfFinishedParent(t, store, owner, task, child, "deploy-north", "cluster:north")
	ref, err := store.FindUnresolvedExternalEffectForTargets(ctx, owner.TenantID, owner.PersonID, parentEffect.child.ID, []string{"cluster:north"})
	if err != nil || ref.RunID != parentEffect.parent.ID || ref.EffectID != "deploy-north" {
		t.Fatalf("continuation lookup = %+v, %v", ref, err)
	}
	root := t.TempDir()
	workspace, err := store.EnsureWorkspace(ctx, Workspace{TenantID: owner.TenantID, OwnerPersonID: owner.PersonID, Name: "chain", LocalPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetWorkspaceTrust(ctx, owner.TenantID, owner.PersonID, workspace.ID, "trusted", "local_cli"); err != nil {
		t.Fatal(err)
	}
	command := "./observe.sh north"
	ruleJSON, _ := json.Marshal(map[string]any{"w": workspace.ID, "t": []string{"cluster:north"}, "or": "observe.sh", "od": "digest"})
	ruleKey := "rule:effect_script:v1:" + base64.RawURLEncoding.EncodeToString(ruleJSON)
	readRuleKey := "rule:observation_script:" + workspace.ID + ":script"
	for _, key := range []string{ruleKey, readRuleKey} {
		if err := store.GrantApproval(ctx, "person", owner.TenantID, owner.PersonID, owner.PersonID, key, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	watch, err := store.CreateExternalWatch(ctx, ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
		WorkspaceID: workspace.ID, TaskID: parentEffect.child.TaskID, RunID: parentEffect.child.ID, Channel: "cli", CWD: root,
		Command: command, ObservationAdapter: "status_json.v1", SpecVersion: 3,
		PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchContinuationReceiptVersion,
			CommandHash: fmt.Sprintf("%x", sha256.Sum256([]byte(command))), EffectID: ref.EffectID, EffectRunID: ref.RunID,
			EffectRuleKey: ruleKey, ObservationRuleKey: readRuleKey, EffectTargetKeys: []string{"cluster:north"},
			EffectScriptRoot: root, EffectScriptPath: root + "/observe.sh", EffectScriptDigest: "digest"}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.FinishExternalWatch(ctx, owner.TenantID, watch.ID, ExternalWatchSucceeded, `{"status":"succeeded"}`, ""); err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	if _, err := store.AppendEvent(ctx, Event{TaskID: parentEffect.child.TaskID, RunID: parentEffect.child.ID, Type: "external_watch.completed",
		Visibility: "task", Channel: "cli", IdempotencyKey: fmt.Sprintf("external-watch:%s:r1:completed", watch.ID)}); err != nil {
		t.Fatal(err)
	}
	if done, err := store.ObserveEffectClaimsForWatch(ctx, owner.TenantID, watch.ID); err != nil || !done {
		t.Fatalf("continuation profile watcher did not release the parent's effect: %v %v", done, err)
	}
	north, err := store.ExternalEffectClaimsForEffect(ctx, owner.TenantID, owner.PersonID, parentEffect.parent.ID, "deploy-north")
	if err != nil || north[0].State != ExternalClaimObserved {
		t.Fatalf("parent effect after the profile watcher: %+v %v", north, err)
	}
}

type chainEffect struct{ parent, child *Run }

// uncertainEffectOfFinishedParent gives a fresh Run of task an uncertain
// effect, parks it, and starts its exact continuation.
func uncertainEffectOfFinishedParent(t *testing.T, store *Store, owner *IdentityContext, task *Task, previous *Run, effectID, target string) chainEffect {
	t.Helper()
	ctx := context.Background()
	if err := store.FinishRun(ctx, owner.TenantID, previous.ID, "done"); err != nil {
		t.Fatal(err)
	}
	parent, err := store.StartRunWithOptions(ctx, task, "cli", "parent", StartRunOptions{MaxActiveRuns: 3})
	if err != nil {
		t.Fatal(err)
	}
	uncertainEffect(t, store, owner, parent, effectID, target)
	if err := store.FinishRun(ctx, owner.TenantID, parent.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "child", StartRunOptions{MaxActiveRuns: 3, ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	return chainEffect{parent: parent, child: child}
}
