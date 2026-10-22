package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

func TestExternalEffectResultFactsDoNotTrustModelReleaseProse(t *testing.T) {
	ctx := context.Background()
	s := controltest.NewStore(t)
	owner, err := s.ResolveOrCreateAccount(ctx, "default", "cli", "effect-facts", "User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "Publish", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.StartRun(ctx, task, "cli", "publish")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID,
		EffectID: "publish", TargetKeys: []string{"registry:alpha"}})
	if err != nil || !d.Granted {
		t.Fatalf("claim: %+v %v", d, err)
	}
	if err := s.MarkExternalEffectPossible(ctx, owner.TenantID, run.ID, "publish"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordExternalEffectReturn(ctx, control.ExternalEffectReturn{TenantID: owner.TenantID, PersonID: owner.PersonID,
		RunID: run.ID, EffectID: "publish", ToolCallID: "publish", Outcome: "exit 1"}); err != nil {
		t.Fatal(err)
	}
	c := (&Server{Control: s}).coordinator()
	selected := c.externalEffectContext(ctx, owner.TenantID, owner.PersonID, run.ID)
	if len(selected) != 1 || selected[0].ClaimID != d.Claims[0].ID || !selected[0].SameChain {
		t.Fatalf("context=%+v", selected)
	}
	var outcome api.RunOutcome
	body := c.withExternalEffectFacts(ctx, owner.TenantID, owner.PersonID, run.ID, "I released all claims.", &outcome)
	for _, want := range []string{"I released all claims.", "Unresolved external effects", d.Claims[0].ID, "remain unresolved in the runtime", "/effects"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q: %s", want, body)
		}
	}
	if len(outcome.UnresolvedEffects) != 1 || outcome.UnresolvedEffects[0].State != control.ExternalClaimLeftUncertain {
		t.Fatalf("outcome=%+v", outcome)
	}
	if _, err := s.SettleExternalEffect(ctx, control.ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
		ClaimID: d.Claims[0].ID, ActorAccountID: owner.AccountID, ActorChannel: "cli", Reason: "Verified revision"}); err != nil {
		t.Fatal(err)
	}
	var settled api.RunOutcome
	if got := c.withExternalEffectFacts(ctx, owner.TenantID, owner.PersonID, run.ID, "Verified.", &settled); got != "Verified." || len(settled.UnresolvedEffects) != 0 {
		t.Fatalf("settled result=%s %+v", got, settled)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var unavailable api.RunOutcome
	if got := c.withExternalEffectFacts(ctx, owner.TenantID, owner.PersonID, run.ID, "All effects settled.", &unavailable); !strings.Contains(got, "state was unavailable") || len(unavailable.Risks) != 1 {
		t.Fatalf("missing facts were hidden: %s %+v", got, unavailable)
	}
}
