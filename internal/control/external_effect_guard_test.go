package control

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Review round 2, item 3: each test here pins one guard of the effect claim
// lifecycle and fails when that guard is removed.

func allocateStart(t *testing.T, store *Store) int64 {
	t.Helper()
	start, err := store.AllocateEffectSequence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return start
}

// bindObservation binds a successful observation that observer started at
// sequence start to claimID.
func bindObservation(store *Store, owner *IdentityContext, observer *Run, claimID, call string, start int64) (ExternalEffectObservationResult, error) {
	return store.BindExternalEffectObservation(context.Background(), ExternalEffectObservation{TenantID: owner.TenantID,
		PersonID: owner.PersonID, ObserverRunID: observer.ID, ClaimID: claimID, ToolCallID: call, StartSequence: start})
}

// conflictingClaim asks for run's effect on target without writing a durable
// wait, and returns how it was decided.
func conflictingClaim(t *testing.T, store *Store, owner *IdentityContext, run *Run, effectID, target string) ExternalEffectClaimDecision {
	t.Helper()
	decision, err := store.ClaimExternalEffects(context.Background(), ExternalEffectClaimRequest{TenantID: owner.TenantID,
		PersonID: owner.PersonID, RunID: run.ID, EffectID: effectID, TargetKeys: []string{target}, DeferWait: true})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func claimState(t *testing.T, store *Store, owner *IdentityContext, run *Run, effectID string) ExternalEffectClaim {
	t.Helper()
	claims, err := store.ExternalEffectClaimsForEffect(context.Background(), owner.TenantID, owner.PersonID, run.ID, effectID)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims of %s: %+v %v", effectID, claims, err)
	}
	return claims[0]
}

// shiftWatchClock moves a watcher's recorded creation time, as a clock step
// would; the sequence it was registered with stays.
func shiftWatchClock(t *testing.T, store *Store, watch *ExternalWatch, seconds int) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE external_watches SET created_at=created_at+? WHERE id=?`, seconds, watch.ID); err != nil {
		t.Fatal(err)
	}
}

// M1b: an observation shows an effect's outcome only when it started after the
// effect's call returned; one that started while the call still ran may have
// read the state before the effect. Binding it is refused and changes nothing,
// for the claim's own Run, for another Run, and after a background exit. An
// observation started after the return still binds.
func TestBoundObservationMustStartAfterTheCallReturned(t *testing.T) {
	for _, tc := range []struct {
		name                string
		background, foreign bool
	}{
		{"own run", false, false},
		{"another run", false, true},
		{"own background exit", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			owner, runA, runB, _ := externalEffectRuns(t, store)
			claims := dispatchedEffect(t, store, owner, runA, "deploy", tc.background, "service:prod")
			observer := runA
			if tc.foreign {
				observer = runB
			}
			// The observation starts while the call runs, which then returns
			// without proof of success.
			early := allocateStart(t, store)
			returnedEffect(t, store, owner, runA, "deploy", 1)
			if result, err := bindObservation(store, owner, observer, claims[0].ID, "early", early); err == nil || result.Released || result.Acknowledged {
				t.Fatalf("an observation that started before the call returned was bound: %+v %v", result, err)
			}
			if got := claimState(t, store, owner, runA, "deploy"); got.State != ExternalClaimUncertain || got.Disposition != ExternalClaimLeftUncertain {
				t.Fatalf("a refused observation changed the claim: %+v", got)
			}
			if tc.foreign {
				if decision := conflictingClaim(t, store, owner, runB, "push-1", "service:prod"); decision.Granted || !decision.NeedsApproval {
					t.Fatalf("a refused observation let another Run pass the claim: %+v", decision)
				}
			}
			result, err := bindObservation(store, owner, observer, claims[0].ID, "late", allocateStart(t, store))
			if err != nil || result.Released != !tc.foreign || result.Acknowledged != tc.foreign {
				t.Fatalf("an observation that started after the call returned: %+v %v", result, err)
			}
			if tc.foreign {
				if decision := conflictingClaim(t, store, owner, runB, "push-2", "service:prod"); !decision.Granted {
					t.Fatalf("a later observation did not let another Run pass once: %+v", decision)
				}
			}
		})
	}
}

// M1e: binding checks again, in its own transaction, that the claim's call is
// not in flight, since nothing can show its outcome yet: a running call of
// another Run, a running background process of either Run, or a reservation
// not yet dispatched. The refusal changes nothing and leaves the observer
// nothing to pass the claim with once the call ends.
func TestObservationCannotBindToACallInFlight(t *testing.T) {
	for _, tc := range []struct {
		name                string
		background, foreign bool
	}{
		{"another run's running call", false, true},
		{"another run's running background process", true, true},
		{"own running background process", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			owner, runA, runB, _ := externalEffectRuns(t, store)
			claims := dispatchedEffect(t, store, owner, runA, "deploy", tc.background, "service:prod")
			observer := runA
			if tc.foreign {
				observer = runB
			}
			result, err := bindObservation(store, owner, observer, claims[0].ID, "check", allocateStart(t, store))
			if !errors.Is(err, ErrExternalEffectInFlight) || result.Released || result.Acknowledged {
				t.Fatalf("an observation was bound to a call in flight: %+v %v", result, err)
			}
			if got := claimState(t, store, owner, runA, "deploy"); got.State != ExternalClaimUncertain || got.Disposition != ExternalClaimInFlight {
				t.Fatalf("a refused observation changed the claim: %+v", got)
			}
			returnedEffect(t, store, owner, runA, "deploy", 1)
			if tc.foreign {
				if decision := conflictingClaim(t, store, owner, runB, "push", "service:prod"); decision.Granted || !decision.NeedsApproval {
					t.Fatalf("an observation refused while the call ran let another Run pass it: %+v", decision)
				}
			} else if got := claimState(t, store, owner, runA, "deploy"); got.Disposition != ExternalClaimLeftUncertain {
				t.Fatalf("own claim after a refused observation and a failed exit: %+v", got)
			}
		})
	}
	t.Run("reservation before dispatch", func(t *testing.T) {
		store := newTestStore(t)
		owner, runA, runB, _ := externalEffectRuns(t, store)
		decision, err := store.ClaimExternalEffects(context.Background(), ExternalEffectClaimRequest{TenantID: owner.TenantID,
			PersonID: owner.PersonID, RunID: runA.ID, EffectID: "deploy", TargetKeys: []string{"service:prod"}})
		if err != nil || !decision.Granted {
			t.Fatalf("claim: %+v %v", decision, err)
		}
		for _, observer := range []*Run{runA, runB} {
			result, err := bindObservation(store, owner, observer, decision.Claims[0].ID, "check", allocateStart(t, store))
			if !errors.Is(err, ErrExternalEffectInFlight) || result.Released || result.Acknowledged {
				t.Fatalf("an observation was bound to a reservation: %+v %v", result, err)
			}
		}
		if got := claimState(t, store, owner, runA, "deploy"); got.State != ExternalClaimReserved {
			t.Fatalf("a refused observation changed the reservation: %+v", got)
		}
	})
}

// boundWatch registers run's watcher bound to claimID, as watch_external with
// effect_claim_id does.
func boundWatch(t *testing.T, store *Store, owner *IdentityContext, run *Run, claimID string) *ExternalWatch {
	t.Helper()
	command := "git ls-remote origin refs/tags/v1"
	watch, err := store.CreateExternalWatch(context.Background(), ExternalWatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
		TaskID: run.TaskID, RunID: run.ID, Channel: "cli", CWD: t.TempDir(), Command: command, SuccessPattern: "refs/tags/v1",
		PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchRecoveryReceiptVersion, RecoveryClaimID: claimID,
			CommandHash: fmt.Sprintf("%x", sha256.Sum256([]byte(command)))}})
	if err != nil {
		t.Fatal(err)
	}
	return watch
}

func succeedBoundWatch(t *testing.T, store *Store, owner *IdentityContext, watch *ExternalWatch) {
	t.Helper()
	ctx := context.Background()
	if ok, err := store.FinishExternalWatch(ctx, owner.TenantID, watch.ID, ExternalWatchSucceeded, "abc refs/tags/v1", ""); err != nil || !ok {
		t.Fatalf("finish: %v %v", ok, err)
	}
	if ok, err := store.MarkExternalWatchFinalized(ctx, owner.TenantID, watch.ID); err != nil || !ok {
		t.Fatalf("finalize: %v %v", ok, err)
	}
}

// M1i: a watcher bound to a claim releases it only when it was registered
// after the effect's call returned, by the shared sequence. One registered
// while the call still ran does not, even when it succeeds after the return
// and its clock says it came long after the claim; one registered after the
// return does, even across a clock step back. The person's explicit resolve
// with the early watcher is refused the same way.
func TestBoundWatcherReleasesOnlyWhenRegisteredAfterTheReturn(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, _, _ := externalEffectRuns(t, store)
	claims := dispatchedEffect(t, store, owner, runA, "deploy", false, "service:prod")
	early := boundWatch(t, store, owner, runA, claims[0].ID)
	returnedEffect(t, store, owner, runA, "deploy", 1)
	shiftWatchClock(t, store, early, 60)
	succeedBoundWatch(t, store, owner, early)
	if released, ok, err := store.ReleaseExternalEffectForWatch(ctx, owner.TenantID, early.ID); err != nil || ok {
		t.Fatalf("a watcher registered before the call returned released it: %+v %v %v", released, ok, err)
	}
	if _, err := store.ObserveExternalEffectWithWatch(ctx, owner.TenantID, owner.PersonID, claims[0].ID, early.ID); err == nil {
		t.Fatal("the person resolved the claim with a watcher registered before the call returned")
	}
	if got := claimState(t, store, owner, runA, "deploy"); got.State != ExternalClaimUncertain {
		t.Fatalf("claim after the early watcher: %+v", got)
	}
	late := boundWatch(t, store, owner, runA, claims[0].ID)
	shiftWatchClock(t, store, late, -60)
	succeedBoundWatch(t, store, owner, late)
	released, ok, err := store.ReleaseExternalEffectForWatch(ctx, owner.TenantID, late.ID)
	if err != nil || !ok || released.ID != claims[0].ID || released.ObservationRef != "watch:"+late.ID+":r1:bound" {
		t.Fatalf("a watcher registered after the return did not release it: %+v %v %v", released, ok, err)
	}
}

// profileWatchers registers owner-reviewed effect profile watchers
// (status_json.v1 under an approved effect script rule) in one trusted
// workspace, for one target set.
type profileWatchers struct {
	t                    *testing.T
	store                *Store
	owner                *IdentityContext
	workspaceID, root    string
	ruleKey, readRuleKey string
	targets              []string
}

func newProfileWatchers(t *testing.T, store *Store, owner *IdentityContext, targets ...string) *profileWatchers {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	workspace, err := store.EnsureWorkspace(ctx, Workspace{TenantID: owner.TenantID, OwnerPersonID: owner.PersonID, Name: "profile", LocalPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetWorkspaceTrust(ctx, owner.TenantID, owner.PersonID, workspace.ID, "trusted", "local_cli"); err != nil {
		t.Fatal(err)
	}
	ruleJSON, err := json.Marshal(map[string]any{"w": workspace.ID, "t": targets, "or": "observe.sh", "od": "digest"})
	if err != nil {
		t.Fatal(err)
	}
	p := &profileWatchers{t: t, store: store, owner: owner, workspaceID: workspace.ID, root: root, targets: targets,
		ruleKey:     "rule:effect_script:v1:" + base64.RawURLEncoding.EncodeToString(ruleJSON),
		readRuleKey: "rule:observation_script:" + workspace.ID + ":script"}
	for _, key := range []string{p.ruleKey, p.readRuleKey} {
		if err := store.GrantApproval(ctx, "person", owner.TenantID, owner.PersonID, owner.PersonID, key, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// register records run's profile watcher of the effect effectRun dispatched.
func (p *profileWatchers) register(run, effectRun *Run, effectID string) *ExternalWatch {
	p.t.Helper()
	command := "./observe.sh"
	effectRunID := ""
	if effectRun.ID != run.ID {
		effectRunID = effectRun.ID
	}
	watch, err := p.store.CreateExternalWatch(context.Background(), ExternalWatch{TenantID: p.owner.TenantID, PersonID: p.owner.PersonID,
		WorkspaceID: p.workspaceID, TaskID: run.TaskID, RunID: run.ID, Channel: "cli", CWD: p.root, Command: command,
		ObservationAdapter: "status_json.v1", SpecVersion: 3,
		PreflightReceipt: ExternalWatchPreflightReceipt{Version: ExternalWatchContinuationReceiptVersion,
			CommandHash: fmt.Sprintf("%x", sha256.Sum256([]byte(command))), EffectID: effectID, EffectRunID: effectRunID,
			EffectRuleKey: p.ruleKey, ObservationRuleKey: p.readRuleKey, EffectTargetKeys: p.targets,
			EffectScriptRoot: p.root, EffectScriptPath: p.root + "/observe.sh", EffectScriptDigest: "digest"}})
	if err != nil {
		p.t.Fatal(err)
	}
	return watch
}

// succeed records the watcher's successful observation and its completion
// event, then reports whether that released the effect it names.
func (p *profileWatchers) succeed(watch *ExternalWatch) bool {
	p.t.Helper()
	return p.finish(watch, ExternalWatchSucceeded, `{"status":"succeeded"}`)
}

// finish records the watcher's verdict and output and its completion event,
// then reports whether that released the effect it names.
func (p *profileWatchers) finish(watch *ExternalWatch, status, output string) bool {
	p.t.Helper()
	ctx := context.Background()
	if ok, err := p.store.FinishExternalWatch(ctx, p.owner.TenantID, watch.ID, status, output, ""); err != nil || !ok {
		p.t.Fatalf("finish: %v %v", ok, err)
	}
	if _, err := p.store.AppendEvent(ctx, Event{TaskID: watch.TaskID, RunID: watch.RunID, Type: "external_watch.completed",
		Visibility: "task", Channel: "cli", IdempotencyKey: fmt.Sprintf("external-watch:%s:r1:completed", watch.ID)}); err != nil {
		p.t.Fatal(err)
	}
	released, err := p.store.ObserveEffectClaimsForWatch(ctx, p.owner.TenantID, watch.ID)
	if err != nil {
		p.t.Fatal(err)
	}
	return released
}

// M1k: an owner-reviewed profile watcher releases the effect group it names
// only when it was registered after the effect's call returned, by the shared
// sequence: not when registered before the claim existed or while the call
// ran, whatever its clock says, and still when registered after the return
// across a clock step back.
func TestProfileWatcherReleasesOnlyWhenRegisteredAfterTheReturn(t *testing.T) {
	store := newTestStore(t)
	owner, runA, _, _ := externalEffectRuns(t, store)
	watchers := newProfileWatchers(t, store, owner, "cluster:north")
	beforeClaim := watchers.register(runA, runA, "deploy")
	dispatchedEffect(t, store, owner, runA, "deploy", false, "cluster:north")
	duringCall := watchers.register(runA, runA, "deploy")
	returnedEffect(t, store, owner, runA, "deploy", 1)
	for _, early := range []struct {
		when  string
		watch *ExternalWatch
	}{{"before the claim", beforeClaim}, {"while the call ran", duringCall}} {
		shiftWatchClock(t, store, early.watch, 60)
		if watchers.succeed(early.watch) {
			t.Fatalf("a profile watcher registered %s released the effect", early.when)
		}
		if got := claimState(t, store, owner, runA, "deploy"); got.State != ExternalClaimUncertain {
			t.Fatalf("effect after the watcher registered %s: %+v", early.when, got)
		}
	}
	late := watchers.register(runA, runA, "deploy")
	shiftWatchClock(t, store, late, -60)
	if !watchers.succeed(late) {
		t.Fatal("a profile watcher registered after the return did not release the effect")
	}
	if got := claimState(t, store, owner, runA, "deploy"); got.State != ExternalClaimObserved || got.ObservationRef != "watch:"+late.ID+":r1:owner-bound" {
		t.Fatalf("effect after the late watcher: %+v", got)
	}
}

// M1l: a profile watcher releases an effect of its own Run or of an earlier
// Run on its exact resume chain only. A Run outside the chain cannot, even
// naming the effect's Run, and neither can an earlier Run release a later
// Run's effect: the chain is walked from the watcher's Run upward.
func TestProfileWatcherReleasesOnlyItsOwnChainsEffect(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, runB, _ := externalEffectRuns(t, store)
	north := newProfileWatchers(t, store, owner, "cluster:north")
	uncertainEffect(t, store, owner, runA, "deploy", "cluster:north")
	if err := store.FinishRun(ctx, owner.TenantID, runA.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	if north.succeed(north.register(runB, runA, "deploy")) {
		t.Fatal("a Run outside the effect's chain released it")
	}
	task, err := store.GetTask(ctx, owner.TenantID, runA.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{MaxActiveRuns: 3, ResumesRunID: runA.ID})
	if err != nil {
		t.Fatal(err)
	}
	south := newProfileWatchers(t, store, owner, "cluster:south")
	uncertainEffect(t, store, owner, child, "deploy-south", "cluster:south")
	if south.succeed(south.register(runA, child, "deploy-south")) {
		t.Fatal("an earlier Run of the chain released a later Run's effect")
	}
	if got := claimState(t, store, owner, child, "deploy-south"); got.State != ExternalClaimUncertain {
		t.Fatalf("later Run's effect: %+v", got)
	}
	if got := claimState(t, store, owner, runA, "deploy"); got.State != ExternalClaimUncertain {
		t.Fatalf("effect after the outside watcher: %+v", got)
	}
	if !north.succeed(north.register(child, runA, "deploy")) {
		t.Fatal("the continuation's profile watcher did not release its parent's effect")
	}
}
