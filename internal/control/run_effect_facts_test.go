package control

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRunEffectFactsUseOnlyOwnedLineageAndTypedReferences(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner, a, b, c := externalEffectRuns(t, s)
	claim := uncertainEffect(t, s, owner, a, "publish", "registry:alpha")[0]
	if facts, err := s.RunRelevantExternalEffects(ctx, owner.TenantID, owner.PersonID, a.ID); err != nil || len(facts) != 1 || !facts[0].SameChain {
		t.Fatalf("own facts=%+v err=%v", facts, err)
	}
	if facts, err := s.RunRelevantExternalEffects(ctx, owner.TenantID, owner.PersonID, b.ID); err != nil || len(facts) != 0 {
		t.Fatalf("unrelated work acquired a blocker: %+v %v", facts, err)
	}
	// Prose naming a claim is not a runtime association.
	if _, err := s.CreateApprovalRequest(ctx, ApprovalRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		TaskID: c.TaskID, RunID: c.ID, ActionType: "tool_call", Payload: json.RawMessage(`{"reason":"` + claim.ID + `"}`)}); err != nil {
		t.Fatal(err)
	}
	if facts, err := s.RunRelevantExternalEffects(ctx, owner.TenantID, owner.PersonID, c.ID); err != nil || len(facts) != 0 {
		t.Fatalf("prose selected a claim: %+v %v", facts, err)
	}
	raw, _ := json.Marshal(map[string]any{"external_effect_claim_ids": []string{claim.ID}})
	if _, err := s.CreateApprovalRequest(ctx, ApprovalRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		TaskID: b.TaskID, RunID: b.ID, ActionType: "tool_call", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	if facts, err := s.RunRelevantExternalEffects(ctx, owner.TenantID, owner.PersonID, b.ID); err != nil || len(facts) != 1 || facts[0].SameChain {
		t.Fatalf("typed foreign blocker missing: %+v %v", facts, err)
	}
	if _, err := s.RunRelevantExternalEffects(ctx, owner.TenantID, "stranger", b.ID); err == nil {
		t.Fatal("another person read owned facts")
	}
	// A read-only foreign observation acknowledges a conflict, without release.
	if _, err := s.AppendEvent(ctx, Event{RunID: c.ID, Type: ExternalEffectObservedEvent,
		Payload: json.RawMessage(`{"claim_id":"` + claim.ID + `"}`)}); err != nil {
		t.Fatal(err)
	}
	if facts, err := s.RunRelevantExternalEffects(ctx, owner.TenantID, owner.PersonID, c.ID); err != nil || len(facts) != 1 || facts[0].State == ExternalClaimObserved {
		t.Fatalf("acknowledged claim falsely settled: %+v %v", facts, err)
	}
	if _, err := s.SettleExternalEffect(ctx, ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		ClaimID: claim.ID, ActorAccountID: owner.AccountID, ActorChannel: "cli", Reason: "Observed the published revision"}); err != nil {
		t.Fatal(err)
	}
	for _, run := range []*Run{a, b, c} {
		if facts, err := s.RunRelevantExternalEffects(ctx, owner.TenantID, owner.PersonID, run.ID); err != nil || len(facts) != 0 {
			t.Fatalf("settled claim remains in facts: %+v %v", facts, err)
		}
	}
}
