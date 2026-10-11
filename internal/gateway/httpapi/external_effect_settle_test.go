package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

// Item 6: /effects settle <claim_id> [reason] is the person's own release of
// an uncertain effect. Only the claim's person can settle it, the actor
// account, channel, and reason are audited, an in-flight call is refused with
// a clear message, and /effects lists uncertain claims with the settle hint.
func TestEffectsSettleIsPersonScopedAuditedAndRefusedInFlight(t *testing.T) {
	for _, channel := range []string{"cli-session-7", "wx-chat-owner"} {
		t.Run(channel, func(t *testing.T) {
			ctx := context.Background()
			store := controltest.NewStore(t)
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "effects-owner", "Owner")
			if err != nil {
				t.Fatal(err)
			}
			stranger, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "effects-stranger", "Stranger")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "release", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRunWithOptions(ctx, task, "cli", "publish", control.StartRunOptions{MaxActiveRuns: 2})
			if err != nil {
				t.Fatal(err)
			}
			decision, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
				RunID: run.ID, EffectID: "publish", TargetKeys: []string{control.UnknownExternalTarget}})
			if err != nil || !decision.Granted {
				t.Fatalf("claim: %+v %v", decision, err)
			}
			claimID := decision.Claims[0].ID
			if err := store.RecordExternalEffectDispatch(ctx, control.ExternalEffectDispatch{TenantID: owner.TenantID, PersonID: owner.PersonID,
				RunID: run.ID, EffectID: "publish", ToolCallID: "publish", ToolName: "terminal", Summary: "npm publish --tag next"}); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkExternalEffectPossible(ctx, owner.TenantID, run.ID, "publish"); err != nil {
				t.Fatal(err)
			}
			server := &Server{Control: store, DefaultTenantID: "default"}
			reply := func(identity *control.IdentityContext, content string) string {
				t.Helper()
				handled, text, _, err := server.tryHandleControlCommand(ctx, identity, api.MessageRequest{Channel: channel, Content: content})
				if err != nil || !handled {
					t.Fatalf("%s: handled=%v err=%v", content, handled, err)
				}
				return text
			}
			if text := reply(owner, "/effects"); !strings.Contains(text, claimID) || !strings.Contains(text, "in flight") {
				t.Fatalf("in-flight listing: %q", text)
			}
			if text := reply(owner, "/effects settle "+claimID); !strings.Contains(text, "still in flight") || strings.Contains(text, "Settled") {
				t.Fatalf("in-flight settle: %q", text)
			}
			exit := 1
			if err := store.RecordExternalEffectReturn(ctx, control.ExternalEffectReturn{TenantID: owner.TenantID, PersonID: owner.PersonID,
				RunID: run.ID, EffectID: "publish", ToolCallID: "publish", Outcome: "exit 1", ExitCode: &exit}); err != nil {
				t.Fatal(err)
			}
			text := reply(owner, "/effects")
			for _, want := range []string{claimID, "`npm publish --tag next`", "exit 1", "/effects settle <claim_id> [reason]"} {
				if !strings.Contains(text, want) {
					t.Fatalf("listing lacks %q: %q", want, text)
				}
			}
			if text := reply(owner, "/effects settle"); !strings.Contains(text, "Usage: /effects") {
				t.Fatalf("bare settle: %q", text)
			}
			if text := reply(stranger, "/effects settle "+claimID); strings.Contains(text, "Settled") || !strings.Contains(text, "No unresolved external effect") {
				t.Fatalf("stranger settle: %q", text)
			}
			text = reply(owner, "/effects settle "+claimID+" checked the registry: version 2.1.0 is live")
			if !strings.Contains(text, "Settled external effect "+claimID) {
				t.Fatalf("owner settle: %q", text)
			}
			claims, err := store.ExternalEffectClaimsForEffect(ctx, owner.TenantID, owner.PersonID, run.ID, "publish")
			if err != nil || claims[0].State != control.ExternalClaimObserved || !strings.HasPrefix(claims[0].ObservationRef, "settlement:") {
				t.Fatalf("settled claim: %+v %v", claims, err)
			}
			settled, err := store.SettleExternalEffect(ctx, control.ExternalEffectSettlementRequest{TenantID: owner.TenantID, PersonID: owner.PersonID,
				ClaimID: claimID, ActorAccountID: owner.AccountID, ActorChannel: channel})
			if err != nil || !settled.Replayed || settled.Settlement.ActorAccountID != owner.AccountID || settled.Settlement.ActorChannel != channel ||
				settled.Settlement.Reason != "checked the registry: version 2.1.0 is live" {
				t.Fatalf("settlement audit: %+v %v", settled, err)
			}
			if text := reply(owner, "/effects settle "+claimID); !strings.Contains(text, "already settled") {
				t.Fatalf("repeated settle: %q", text)
			}
			if text := reply(owner, "/effects"); !strings.Contains(text, "No unresolved external effects.") {
				t.Fatalf("settled effect still listed: %q", text)
			}
		})
	}
}

// Review round 3, item 3: settling a background process that is still
// running is refused with what the person can do: the refusal names the
// process and says how it stops. While its run is running, that run can stop
// it with the process tool; after the run ends, it stops at its time limit
// (two hours by default) or when SelfMind restarts.
func TestEffectsSettleNamesARunningBackgroundProcess(t *testing.T) {
	for _, owner := range []string{"running", "finished"} {
		t.Run("owner "+owner, func(t *testing.T) {
			ctx := context.Background()
			store := controltest.NewStore(t)
			person, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "effects-owner", "Owner")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "serve", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRunWithOptions(ctx, task, "cli", "serve", control.StartRunOptions{MaxActiveRuns: 2})
			if err != nil {
				t.Fatal(err)
			}
			decision, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{TenantID: person.TenantID, PersonID: person.PersonID,
				RunID: run.ID, EffectID: "serve", TargetKeys: []string{control.UnknownExternalTarget}})
			if err != nil || !decision.Granted {
				t.Fatalf("claim: %+v %v", decision, err)
			}
			claimID := decision.Claims[0].ID
			if err := store.RecordExternalEffectDispatch(ctx, control.ExternalEffectDispatch{TenantID: person.TenantID, PersonID: person.PersonID,
				RunID: run.ID, EffectID: "serve", ToolCallID: "serve", ToolName: "terminal", Summary: "./serve.sh --port 8080", Background: true}); err != nil {
				t.Fatal(err)
			}
			if err := store.MarkExternalEffectPossible(ctx, person.TenantID, run.ID, "serve"); err != nil {
				t.Fatal(err)
			}
			if owner == "finished" {
				if err := store.FinishRun(ctx, person.TenantID, run.ID, "done"); err != nil {
					t.Fatal(err)
				}
			}
			server := &Server{Control: store, DefaultTenantID: "default"}
			handled, text, _, err := server.tryHandleControlCommand(ctx, person, api.MessageRequest{Channel: "cli", Content: "/effects settle " + claimID})
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			want := []string{claimID, "background process", "`./serve.sh --port 8080`", run.ID, "2 hours"}
			if owner == "running" {
				want = append(want, "process tool", "kill")
			} else {
				want = append(want, "restarts")
			}
			for _, w := range want {
				if !strings.Contains(text, w) {
					t.Fatalf("refusal does not mention %q: %q", w, text)
				}
			}
			if strings.Contains(text, "Settled") || owner == "finished" && strings.Contains(text, "process tool") {
				t.Fatalf("refusal: %q", text)
			}
		})
	}
}
