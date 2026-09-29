package control

import (
	"context"
	"sync"
	"testing"
)

func externalEffectRuns(t *testing.T, store *Store) (*IdentityContext, *Run, *Run, *Run) {
	t.Helper()
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	var runs []*Run
	for _, title := range []string{"A", "B", "C"} {
		task, err := store.CreateTask(ctx, TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: title, Channel: title})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRunWithOptions(ctx, task, title, title, StartRunOptions{MaxActiveRuns: 3})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
	}
	return owner, runs[0], runs[1], runs[2]
}

func TestExternalEffectClaimsAreAtomicAcrossConnectionsAndSurviveRunFinish(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	first, err := OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	owner, runA, runB, runC := externalEffectRuns(t, first)
	requests := []ExternalEffectClaimRequest{
		{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: runA.ID, EffectID: "effect-a", TargetKeys: []string{"deploy:production"}},
		{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: runB.ID, EffectID: "effect-b", TargetKeys: []string{"deploy:production"}},
	}
	stores := []*Store{first, second}
	type outcome struct {
		decision ExternalEffectClaimDecision
		err      error
	}
	results := make([]outcome, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range requests {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index].decision, results[index].err = stores[index].ClaimExternalEffects(ctx, requests[index])
		}(index)
	}
	close(start)
	wait.Wait()
	var winner, loser int
	if results[0].decision.Granted {
		winner, loser = 0, 1
	} else {
		winner, loser = 1, 0
	}
	if results[winner].err != nil || !results[winner].decision.Granted ||
		results[loser].err != nil || results[loser].decision.Granted || results[loser].decision.BlockedByRun != requests[winner].RunID {
		t.Fatalf("same-target race was not serialized: %+v", results)
	}
	claim := results[winner].decision.Claims[0]
	if listed, err := second.ListUnresolvedExternalEffects(ctx, owner.TenantID, owner.PersonID, 10); err != nil || len(listed) != 1 || listed[0].ID != claim.ID {
		t.Fatalf("owner cannot inspect exact unresolved claim: %+v, %v", listed, err)
	}
	if listed, err := second.ListUnresolvedExternalEffects(ctx, owner.TenantID, "other-person", 10); err != nil || len(listed) != 0 {
		t.Fatalf("claim leaked to another person: %+v, %v", listed, err)
	}
	if err := stores[winner].MarkExternalEffectPossible(ctx, owner.TenantID, requests[winner].RunID, requests[winner].EffectID); err != nil {
		t.Fatal(err)
	}
	if err := stores[winner].FinishRun(ctx, owner.TenantID, requests[winner].RunID, "done"); err != nil {
		t.Fatal(err)
	}
	if again, err := stores[loser].ClaimExternalEffects(ctx, requests[loser]); err != nil || again.Granted {
		t.Fatalf("Run finish incorrectly released an unresolved external effect: %+v, %v", again, err)
	}
	if err := second.ObserveExternalEffect(ctx, owner.TenantID, "another-person", claim.ID, "watch:terminal"); err == nil {
		t.Fatal("another person resolved the owner's claim")
	}
	if err := second.ObserveExternalEffect(ctx, owner.TenantID, owner.PersonID, claim.ID, "watch:terminal"); err != nil {
		t.Fatal(err)
	}
	if released, err := stores[loser].ClaimExternalEffects(ctx, requests[loser]); err != nil || !released.Granted {
		t.Fatalf("observed target did not unblock an independent Run: %+v, %v", released, err)
	}
	if repeated, err := stores[loser].ClaimExternalEffects(ctx, requests[loser]); err != nil || repeated.Granted || !repeated.AlreadyKnown {
		t.Fatalf("same effect id could dispatch twice: %+v, %v", repeated, err)
	}
	// A different target is independent even though the first effect remains
	// in the immutable audit trail.
	if independent, err := first.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{
		TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: runC.ID,
		EffectID: "effect-c", TargetKeys: []string{"deploy:staging"},
	}); err != nil || !independent.Granted {
		t.Fatalf("disjoint target was unnecessarily serialized: %+v, %v", independent, err)
	}
}

func TestExternalEffectUnknownTargetAndMultiTargetClaimFailClosed(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, runB, runC := externalEffectRuns(t, store)
	claim := func(run *Run, effect string, targets ...string) (ExternalEffectClaimDecision, error) {
		return store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID,
			PersonID: owner.PersonID, RunID: run.ID, EffectID: effect, TargetKeys: targets})
	}
	if first, err := claim(runA, "a", "deploy:staging"); err != nil || !first.Granted {
		t.Fatalf("initial claim: %+v, %v", first, err)
	}
	if blocked, err := claim(runB, "b", "deploy:production", "deploy:staging"); err != nil || blocked.Granted || blocked.BlockedByRun != runA.ID {
		t.Fatalf("overlapping group was not rejected atomically: %+v, %v", blocked, err)
	}
	if independent, err := claim(runC, "c", "deploy:production"); err != nil || !independent.Granted {
		t.Fatalf("failed group left a partial target claim: %+v, %v", independent, err)
	}
	if blocked, err := claim(runB, "unknown", UnknownExternalTarget); err != nil || blocked.Granted {
		t.Fatalf("unknown target bypassed known claims: %+v, %v", blocked, err)
	}
	if _, err := claim(runA, "a", "deploy:another"); err == nil {
		t.Fatal("same effect id was allowed to change target after the claim")
	}
	if _, err := claim(runA, "invalid", "not-canonical"); err == nil {
		t.Fatal("unstructured target key was accepted")
	}
	if _, err := claim(runA, "secret-url", "https://token@host/path"); err == nil {
		t.Fatal("raw URL or credential-shaped target key was persisted")
	}
}

func TestVersionTwentyThreeUpgradeDoesNotInventHistoricalEffectClaims(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	store, err := OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	owner, run, _, _ := externalEffectRuns(t, store)
	if _, err := store.db.ExecContext(ctx, `DROP TABLE external_effect_claims`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE external_resource_waits`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 23`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := OpenStore(data)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if status := upgraded.SchemaStatus(); status.Version != CurrentControlSchemaVersion || status.MigrationBackup == "" {
		t.Fatalf("upgrade did not back up and migrate v22: %+v", status)
	}
	var claims int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_effect_claims`).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("v22 historical runs acquired inferred claims: count=%d err=%v", claims, err)
	}
	if err := upgraded.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_resource_waits`).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("v22 historical runs acquired inferred waits: count=%d err=%v", claims, err)
	}
	if old, err := upgraded.GetRun(ctx, owner.TenantID, run.ID); err != nil || old == nil || old.PersonID != owner.PersonID {
		t.Fatalf("upgrade lost the historical Run: %+v, %v", old, err)
	}
}

func TestExternalEffectGroupObservationReleasesAllTargetsTogether(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, runB, _ := externalEffectRuns(t, store)
	request := ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: runA.ID, EffectID: "group-effect", TargetKeys: []string{"deploy:alpha", "deploy:beta"}}
	decision, err := store.ClaimExternalEffects(ctx, request)
	if err != nil || !decision.Granted || len(decision.Claims) != 2 {
		t.Fatalf("group claim: %+v, %v", decision, err)
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, runA.ID, request.EffectID); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveExternalEffectGroup(ctx, owner.TenantID, "other-person", runA.ID, request.EffectID, "observation:exact"); err == nil {
		t.Fatal("another person released the group")
	}
	if err := store.ObserveExternalEffectGroup(ctx, owner.TenantID, owner.PersonID, runA.ID, request.EffectID, "observation:exact"); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveExternalEffectGroup(ctx, owner.TenantID, owner.PersonID, runA.ID, request.EffectID, "observation:exact"); err != nil {
		t.Fatalf("same evidence was not idempotent: %v", err)
	}
	if err := store.ObserveExternalEffectGroup(ctx, owner.TenantID, owner.PersonID, runA.ID, request.EffectID, "observation:changed"); err == nil {
		t.Fatal("a later observation rewrote immutable resolution evidence")
	}
	for index, target := range []string{"deploy:alpha", "deploy:beta"} {
		later, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID,
			PersonID: owner.PersonID, RunID: runB.ID, EffectID: "later-" + target, TargetKeys: []string{target}})
		if err != nil || !later.Granted {
			t.Fatalf("target %d stayed occupied after group observation: %+v, %v", index, later, err)
		}
	}
}

func TestExternalResourceWaitRequiresObservationAndExactParent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, runB, _ := externalEffectRuns(t, store)
	claim := func(run *Run, effect, target string) ExternalEffectClaimDecision {
		t.Helper()
		result, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID,
			PersonID: owner.PersonID, RunID: run.ID, EffectID: effect, TargetKeys: []string{target}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if !claim(runA, "first", "deploy:one").Granted {
		t.Fatal("first claimant did not acquire target")
	}
	if blocked := claim(runA, "second", "deploy:one"); blocked.Granted || blocked.BlockedByRun != runA.ID {
		t.Fatalf("same Run bypassed unresolved effect: %+v", blocked)
	}
	if blocked := claim(runB, "third", "deploy:one"); blocked.Granted || blocked.BlockedByRun != runA.ID {
		t.Fatalf("other Run did not wait: %+v", blocked)
	}
	if err := store.FinishRun(ctx, owner.TenantID, runB.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	if ready, err := store.ListReadyExternalResourceWaits(ctx, 10); err != nil || len(ready) != 0 {
		t.Fatalf("occupied target woke waiter: %+v %v", ready, err)
	}
	parent, err := store.GetRun(ctx, owner.TenantID, runB.ID)
	if err != nil {
		t.Fatal(err)
	}
	child := &Run{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: parent.TaskID, ResumesRunID: runB.ID}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateResumeClaimTx(ctx, tx, child); err != ErrResumeTargetNotResumable {
		t.Fatalf("pending resource wait yielded parent: %v", err)
	}
	_ = tx.Rollback()
	if err := store.ObserveExternalEffectGroup(ctx, owner.TenantID, owner.PersonID, runA.ID, "first", "observer:terminal"); err != nil {
		t.Fatal(err)
	}
	ready, err := store.ListReadyExternalResourceWaits(ctx, 10)
	if err != nil || len(ready) != 1 || ready[0].RunID != runB.ID || ready[0].Status != "pending" {
		t.Fatalf("observed target did not wake exact parent: %+v %v", ready, err)
	}
	if err := store.MarkExternalResourceWaitQueued(ctx, ready[0]); err != nil {
		t.Fatal(err)
	}
	ready, err = store.ListReadyExternalResourceWaits(ctx, 10)
	if err != nil || len(ready) != 1 || ready[0].Status != "queued" {
		t.Fatalf("crash after readiness lost wakeup: %+v %v", ready, err)
	}
	tx, err = store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateResumeClaimTx(ctx, tx, child); err != nil {
		t.Fatalf("ready resource parent was not claimable: %v", err)
	}
	_ = tx.Rollback()
}

func TestExternalEffectRecoveryReleasesOnlyUndispatchedReservation(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, runB, _ := externalEffectRuns(t, store)
	for _, entry := range []struct {
		run            *Run
		effect, target string
	}{
		{runA, "reserved", "remote:before"},
		{runB, "uncertain", "remote:after"},
	} {
		decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID,
			PersonID: owner.PersonID, RunID: entry.run.ID, EffectID: entry.effect, TargetKeys: []string{entry.target}})
		if err != nil || !decision.Granted {
			t.Fatalf("claim %s: %+v %v", entry.effect, decision, err)
		}
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, runB.ID, "uncertain"); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ObserveUndispatchedExternalEffects(ctx); err != nil || n != 0 {
		t.Fatalf("recovery released an active reservation: count=%d err=%v", n, err)
	}
	for _, run := range []*Run{runA, runB} {
		if err := store.FinishRun(ctx, owner.TenantID, run.ID, "interrupted"); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := store.ObserveUndispatchedExternalEffects(ctx); err != nil || n != 1 {
		t.Fatalf("recovery did not distinguish dispatch certainty: count=%d err=%v", n, err)
	}
	left, err := store.ListUnresolvedExternalEffects(ctx, owner.TenantID, owner.PersonID, 10)
	if err != nil || len(left) != 1 || left[0].EffectID != "uncertain" {
		t.Fatalf("uncertain effect was released or reserved effect remained: %+v %v", left, err)
	}
}
