package control

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// uncertainEffect claims targets for run, crosses the dispatch boundary, and
// records that the call returned without proving success, so the effect may
// have happened and only an observation or the person can resolve it.
func uncertainEffect(t *testing.T, store *Store, owner *IdentityContext, run *Run, effectID string, targets ...string) []ExternalEffectClaim {
	t.Helper()
	ctx := context.Background()
	decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID,
		PersonID: owner.PersonID, RunID: run.ID, EffectID: effectID, TargetKeys: targets})
	if err != nil || !decision.Granted {
		t.Fatalf("claim %s: %+v %v", effectID, decision, err)
	}
	if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, run.ID, effectID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordExternalEffectReturn(ctx, ExternalEffectReturn{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: run.ID, EffectID: effectID, ToolCallID: "call-" + effectID, Outcome: "exit 1"}); err != nil {
		t.Fatal(err)
	}
	return decision.Claims
}

func settlementCount(t *testing.T, store *Store) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM external_effect_settlements`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func claimStates(t *testing.T, store *Store, runID, effectID string) map[string]string {
	t.Helper()
	rows, err := store.db.Query(`SELECT target_key, state || '|' || observation_ref FROM external_effect_claims
		WHERE run_id = ? AND effect_id = ?`, runID, effectID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var target, state string
		if err := rows.Scan(&target, &state); err != nil {
			t.Fatal(err)
		}
		out[target] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPersonSettlementObservesWholeEffectAndAudits(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, runB, _ := externalEffectRuns(t, store)
	claims := uncertainEffect(t, store, owner, runA, "effect-sync", "cluster:east", "cluster:west")
	request := ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, ClaimID: claims[0].ID,
		ActorAccountID: owner.AccountID, ActorChannel: "cli", Reason: "  checked both clusters by hand  "}

	result, err := store.SettleExternalEffect(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	settlement := result.Settlement
	if result.Replayed || settlement.ID == "" || settlement.PersonID != owner.PersonID || settlement.ClaimID != claims[0].ID ||
		settlement.RunID != runA.ID || settlement.EffectID != "effect-sync" || settlement.ActorAccountID != owner.AccountID ||
		settlement.ActorChannel != "cli" || settlement.Reason != "checked both clusters by hand" || settlement.CreatedAt.IsZero() {
		t.Fatalf("settlement = %+v replayed=%v", settlement, result.Replayed)
	}
	ref := settlement.ObservationRef()
	if ref != "settlement:"+settlement.ID {
		t.Fatalf("observation ref %q does not identify the settlement", ref)
	}
	if got := claimStates(t, store, runA.ID, "effect-sync"); got["cluster:east"] != "observed|"+ref || got["cluster:west"] != "observed|"+ref {
		t.Fatalf("effect group after settlement = %v", got)
	}
	if len(result.Claims) != 2 || result.Claims[0].State != ExternalClaimObserved || result.Claims[1].ObservationRef != ref {
		t.Fatalf("returned claims = %+v", result.Claims)
	}
	var auditPerson, auditClaim, auditRun, auditEffect, auditAccount, auditChannel, auditReason string
	if err := store.db.QueryRowContext(ctx, `SELECT person_id, claim_id, run_id, effect_id, actor_account_id, actor_channel, reason
		FROM external_effect_settlements WHERE id = ?`, settlement.ID).Scan(&auditPerson, &auditClaim, &auditRun,
		&auditEffect, &auditAccount, &auditChannel, &auditReason); err != nil {
		t.Fatal(err)
	}
	if auditPerson != owner.PersonID || auditClaim != claims[0].ID || auditRun != runA.ID || auditEffect != "effect-sync" ||
		auditAccount != owner.AccountID || auditChannel != "cli" || auditReason != "checked both clusters by hand" {
		t.Fatalf("audit row = %s %s %s %s %s %s %q", auditPerson, auditClaim, auditRun, auditEffect, auditAccount, auditChannel, auditReason)
	}

	// The released targets are claimable again: another Run is not blocked.
	if decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID,
		PersonID: owner.PersonID, RunID: runB.ID, EffectID: "effect-next", TargetKeys: []string{"cluster:east"}}); err != nil || !decision.Granted {
		t.Fatalf("settled target still blocks: %+v %v", decision, err)
	}

	// A retry of the same command, and a settle naming the sibling target, both
	// return the one recorded settlement without a second audit row.
	for _, claimID := range []string{claims[0].ID, claims[1].ID} {
		request.ClaimID = claimID
		replay, err := store.SettleExternalEffect(ctx, request)
		if err != nil || !replay.Replayed || replay.Settlement.ID != settlement.ID || replay.Settlement.ClaimID != claims[0].ID {
			t.Fatalf("replay for %s = %+v err=%v", claimID, replay, err)
		}
	}
	if count := settlementCount(t, store); count != 1 {
		t.Fatalf("settlement rows = %d, want 1", count)
	}
}

func TestOnlyTheOwnerCanSettleAnEffect(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, _, _ := externalEffectRuns(t, store)
	stranger, err := store.ResolveOrCreateAccount(ctx, owner.TenantID, "telegram", "424242", "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	claims := uncertainEffect(t, store, owner, runA, "effect-publish", UnknownExternalTarget)
	before := claimStates(t, store, runA.ID, "effect-publish")
	for name, request := range map[string]ExternalEffectSettlementRequest{
		"another person": {TenantID: owner.TenantID, PersonID: stranger.PersonID, ClaimID: claims[0].ID,
			ActorAccountID: stranger.AccountID, ActorChannel: "telegram"},
		"another tenant": {TenantID: "elsewhere", PersonID: owner.PersonID, ClaimID: claims[0].ID,
			ActorAccountID: owner.AccountID, ActorChannel: "cli"},
		"unknown claim": {TenantID: owner.TenantID, PersonID: owner.PersonID, ClaimID: "effectclaim_missing",
			ActorAccountID: owner.AccountID, ActorChannel: "cli"},
	} {
		if _, err := store.SettleExternalEffect(ctx, request); err == nil {
			t.Fatalf("%s settled the owner's effect", name)
		}
	}
	// The owner's person with someone else's account is not an attributable
	// actor either: the audit would name an endpoint the owner does not hold.
	for name, request := range map[string]ExternalEffectSettlementRequest{
		"foreign account": {TenantID: owner.TenantID, PersonID: owner.PersonID, ClaimID: claims[0].ID,
			ActorAccountID: stranger.AccountID, ActorChannel: "telegram"},
		"no account": {TenantID: owner.TenantID, PersonID: owner.PersonID, ClaimID: claims[0].ID, ActorChannel: "cli"},
		"no channel": {TenantID: owner.TenantID, PersonID: owner.PersonID, ClaimID: claims[0].ID, ActorAccountID: owner.AccountID},
	} {
		if _, err := store.SettleExternalEffect(ctx, request); err == nil {
			t.Fatalf("%s settled the effect", name)
		}
	}
	if _, err := store.SettleExternalEffect(ctx, ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: stranger.PersonID,
		ClaimID: claims[0].ID, ActorAccountID: stranger.AccountID, ActorChannel: "telegram"}); !errors.Is(err, ErrExternalEffectClaimNotFound) {
		t.Fatalf("another person's settle error = %v, want ErrExternalEffectClaimNotFound", err)
	}
	if after := claimStates(t, store, runA.ID, "effect-publish"); after[UnknownExternalTarget] != before[UnknownExternalTarget] {
		t.Fatalf("rejected settlements changed the claim: %v -> %v", before, after)
	}
	if count := settlementCount(t, store); count != 0 {
		t.Fatalf("rejected settlements wrote %d audit rows", count)
	}
}

func TestSettlementRequiresAnUnresolvedDispatchedEffect(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, runA, runB, _ := externalEffectRuns(t, store)
	observed := uncertainEffect(t, store, owner, runA, "effect-shell", "git:origin")
	if err := store.ObserveExternalEffectGroup(ctx, owner.TenantID, owner.PersonID, runA.ID, "effect-shell", "watch:trusted:r1"); err != nil {
		t.Fatal(err)
	}
	reserved, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID,
		PersonID: owner.PersonID, RunID: runB.ID, EffectID: "effect-reserved", TargetKeys: []string{"registry:npm"}})
	if err != nil || !reserved.Granted {
		t.Fatalf("reserve: %+v %v", reserved, err)
	}
	for name, claim := range map[string]ExternalEffectClaim{"observed": observed[0], "never dispatched": reserved.Claims[0]} {
		_, err := store.SettleExternalEffect(ctx, ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
			ClaimID: claim.ID, ActorAccountID: owner.AccountID, ActorChannel: "cli"})
		if !errors.Is(err, ErrExternalEffectNotSettleable) {
			t.Fatalf("%s claim settle error = %v, want ErrExternalEffectNotSettleable", name, err)
		}
	}
	if got := claimStates(t, store, runA.ID, "effect-shell"); got["git:origin"] != "observed|watch:trusted:r1" {
		t.Fatalf("settlement overwrote a trusted observation: %v", got)
	}
	if got := claimStates(t, store, runB.ID, "effect-reserved"); got["registry:npm"] != "reserved|" {
		t.Fatalf("settlement released a reservation: %v", got)
	}
	if count := settlementCount(t, store); count != 0 {
		t.Fatalf("refused settlements wrote %d audit rows", count)
	}
}

func TestConcurrentSettlementsRecordOneAudit(t *testing.T) {
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
	owner, runA, _, _ := externalEffectRuns(t, first)
	claims := uncertainEffect(t, first, owner, runA, "effect-deploy", "deploy:production", "deploy:staging")
	type outcome struct {
		result ExternalEffectSettlementResult
		err    error
	}
	outcomes := make([]outcome, 8)
	var wg sync.WaitGroup
	for i := range outcomes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := []*Store{first, second}[i%2]
			result, err := store.SettleExternalEffect(ctx, ExternalEffectSettlementRequest{TenantID: owner.TenantID,
				PersonID: owner.PersonID, ClaimID: claims[i%2].ID, ActorAccountID: owner.AccountID, ActorChannel: "cli"})
			outcomes[i] = outcome{result, err}
		}(i)
	}
	wg.Wait()
	winners := 0
	id := ""
	for _, outcome := range outcomes {
		if outcome.err != nil {
			t.Fatalf("concurrent settle failed: %v", outcome.err)
		}
		if !outcome.result.Replayed {
			winners++
		}
		if id == "" {
			id = outcome.result.Settlement.ID
		}
		if outcome.result.Settlement.ID != id {
			t.Fatalf("concurrent settles disagree: %s vs %s", outcome.result.Settlement.ID, id)
		}
	}
	if winners != 1 || settlementCount(t, first) != 1 {
		t.Fatalf("winners=%d audit rows=%d, want exactly one", winners, settlementCount(t, first))
	}
}
