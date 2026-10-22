package control

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLegacyEffectWithoutLifecycleDetailsStaysUncertain(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, run, other, _ := externalEffectRuns(t, store)
	decision, err := store.ClaimExternalEffects(ctx, ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: run.ID, EffectID: "legacy", TargetKeys: []string{UnknownExternalTarget}})
	if err != nil || !decision.Granted {
		t.Fatalf("claim: %+v %v", decision, err)
	}
	// Released versions may have an uncertain claim without the newer
	// lifecycle records. Neither its age nor absence of metadata proves safety.
	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	if _, err := store.db.Exec(`UPDATE external_effect_claims SET state='uncertain',sequence=0,created_at=? WHERE id=?`, old, decision.Claims[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, run.ID, "blocked"); err != nil {
		t.Fatal(err)
	}
	claims, err := store.ListUnresolvedExternalEffects(ctx, owner.TenantID, owner.PersonID, 20)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims=%+v %v", claims, err)
	}
	claim := claims[0]
	if claim.CreatedAt.Unix() != old || claim.DispatchRecorded || claim.Disposition != ExternalClaimLeftUncertain ||
		!strings.Contains(claim.Description(), "dispatch details unavailable") || strings.Contains(claim.Description(), "ended before") {
		t.Fatalf("missing evidence gained a fabricated result: %+v %s", claim, claim.Description())
	}
	conflicts, err := store.InspectExternalEffectConflicts(ctx, owner.TenantID, owner.PersonID, other.ID, []string{"repo:independent"})
	if err != nil || len(conflicts.Foreign) != 1 {
		t.Fatalf("legacy unknown target was released: %+v %v", conflicts, err)
	}
	if claims, err := store.ListUnresolvedExternalEffects(ctx, owner.TenantID, "stranger", 20); err != nil || len(claims) != 0 {
		t.Fatalf("stranger: %+v %v", claims, err)
	}
}

func TestToolLedgerRetentionPreservesUnresolvedEffectEvidence(t *testing.T) {
	for _, status := range []string{ToolLedgerCompleted, ToolLedgerFailed} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			owner, run, other, _ := externalEffectRuns(t, store)
			claims := dispatchedEffect(t, store, owner, run, "pending-effect", false, "repo:alpha")
			for _, entry := range []ToolLedgerEntry{
				{RunID: run.ID, ToolCallID: "held", ToolName: "terminal", ArgsHash: "a", EffectID: "pending-effect", RetryClass: "side_effect"},
				{RunID: other.ID, ToolCallID: "unrelated", ToolName: "terminal", ArgsHash: "b", EffectID: "pending-effect", RetryClass: "side_effect"},
			} {
				if _, err := store.ClaimToolDispatch(ctx, owner.TenantID, entry); err != nil {
					t.Fatal(err)
				}
				if err := store.RecordToolOutcome(ctx, owner.TenantID, entry.RunID, entry.ToolCallID, status == ToolLedgerCompleted); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.db.Exec(`UPDATE tool_ledger SET updated_at=?`, time.Now().Add(-30*24*time.Hour).Unix()); err != nil {
				t.Fatal(err)
			}
			pruned, err := store.PruneToolLedger(ctx, 0)
			if err != nil || pruned != 1 {
				t.Fatalf("prune unresolved effect: %d %v", pruned, err)
			}
			var call string
			if err := store.db.QueryRow(`SELECT tool_call_id FROM tool_ledger`).Scan(&call); err != nil || call != "held" {
				t.Fatalf("wrong evidence retained: %s %v", call, err)
			}
			if err := store.ObserveExternalEffectGroup(ctx, owner.TenantID, owner.PersonID, run.ID, "pending-effect", "test:observed"); err != nil {
				t.Fatal(err)
			}
			pruned, err = store.PruneToolLedger(ctx, 0)
			if err != nil || pruned != 1 {
				t.Fatalf("prune settled effect: %d %v; claim=%s", pruned, err, claims[0].ID)
			}
		})
	}
}
