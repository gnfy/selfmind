package httpapi

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

// Human language remains intact. In particular an exception referring to a
// previous proposal must not become a gateway-created blanket permission rule.
func TestHumanConstraintsReachSmartJudgeWithWholeProposal(t *testing.T) {
	for _, user := range []string{
		"执行，上面的3个问题，好像都不要处理。但这一次可以考虑把这个工单相关的任务状态都改好吧。",
		"Proceed with the release; leave those three optional items alone.",
		"只检查发布状态，不要修改 config.yaml。",
	} {
		t.Run(user, func(t *testing.T) {
			store := controltest.NewStore(t)
			ctx := context.Background()
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			offer := "Prepare release and verify its receipt.\n" + strings.Repeat("Preflight evidence is complete.\n", 100) + "\nThree optional items: update compiler, rerun old worker, commit unrelated files."
			if err := store.RecordChannelMessage(ctx, *identity, "cli", "prior", "assistant", offer); err != nil {
				t.Fatal(err)
			}
			judge := &promptCapturingJudge{reply: `{"risk_level":"low","user_authorization":"high","outcome":"approve","rationale":"Inspect the accepted release."}`}
			server := &Server{Control: store, DefaultTenantID: "default", ApprovalJudge: judge}
			run := &control.Run{ID: "mixed-intent", Channel: "cli"}
			req := api.MessageRequest{Content: user, Channel: "cli", ApprovalMode: "smart"}
			intent := server.coordinator().intentSnapshotWithOffer(ctx, identity, nil, run, nil, req, "cli")
			if intent.HasExplicitDeny() || len(intent.DenyScopes) > 0 {
				t.Fatal("natural language became a deterministic permission rule")
			}
			if intent.PriorAssistantOffer != offer || intent.AuthorizationEvidenceIncomplete {
				t.Fatal("complete proposal was lost")
			}
			cleanup := server.coordinator().installExecutionScope(ctx, identity, nil, run, nil, req)
			defer cleanup()
			ran := false
			exec := tools.SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { ran = true; return "observed", nil })
			_, err = exec(map[string]interface{}{"_tenant_id": identity.PersonID, "_tool_name": "terminal", "command": "cat release-notes.md"})
			if err != nil || !ran {
				t.Fatalf("judge approval did not release observation: %v", err)
			}
			prompt := judge.seen()
			if !strings.Contains(prompt, user) || !strings.Contains(prompt, offer) {
				t.Fatal("judge did not receive the complete request and proposal")
			}
		})
	}
}
