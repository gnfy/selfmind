package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

// TestBuildApprovalDecisionsIsTheSingleAnswerSet pins batch B1: the daemon computes
// the option list once, from facts only it has, and every surface renders THAT.
// Before this, the TUI offered four options and Telegram offered two, so what a
// person could choose depended on which device they were holding.
func TestBuildApprovalDecisionsIsTheSingleAnswerSet(t *testing.T) {
	options := buildApprovalDecisions(tools.ToolApprovalRequest{
		ToolName:   "terminal",
		GrantClass: `"git" commands`,
		RuleCandidates: []tools.ApprovalRuleCandidate{
			{Kind: tools.ApprovalRuleKindCommandPrefix, Key: "rule:exec_prefix:git status", Label: "commands that start with `git status`"},
		},
	})
	if len(options) < 4 {
		t.Fatalf("expected once + rule + class + deny, got %+v", options)
	}
	foundRun := false
	for _, option := range options {
		if option.ID == "run" && option.Scope == "run" && option.Key == "r" {
			foundRun = true
		}
	}
	if !foundRun {
		t.Fatalf("run-scoped decision is missing: %+v", options)
	}
	if options[0].ID != "once" || options[0].Key != "y" {
		t.Fatalf("the narrowest answer must come first: %+v", options[0])
	}
	last := options[len(options)-1]
	if last.Decision != "rejected" {
		t.Fatalf("refusal must be last so it is never the default landing spot: %+v", last)
	}
	var rule approvalDecisionOption
	for _, option := range options {
		if strings.HasPrefix(option.ID, "rule:") {
			rule = option
		}
	}
	if rule.GrantKey != "rule:exec_prefix:git status" || rule.Key != "p" {
		t.Fatalf("rule option lost its key or shortcut: %+v", rule)
	}
	if !strings.Contains(rule.Label, "git status") {
		t.Fatalf("rule label must state the rule: %q", rule.Label)
	}

	// No grant class → no class-memory options, because the grant floor would
	// discard them. Offering them would promise memory that never happens.
	noClass := buildApprovalDecisions(tools.ToolApprovalRequest{ToolName: "execute_code"})
	for _, option := range noClass {
		if option.ID == "task" || option.ID == "person" {
			t.Fatalf("class options must not be offered without a grant class: %+v", noClass)
		}
	}
	if len(noClass) != 2 {
		t.Fatalf("an unrememberable action offers only once + deny, got %+v", noClass)
	}

	exact := buildApprovalDecisions(tools.ToolApprovalRequest{
		ToolName: "execute_code", RunGrantClass: "this exact action for this run",
	})
	foundExact := false
	for _, option := range exact {
		if option.ID == "run_exact" && option.Scope == "run" && option.Key == "r" {
			foundExact = true
		}
		if option.Scope == "task" || option.Scope == "person" {
			t.Fatalf("an exact script grant must never become durable: %+v", exact)
		}
	}
	if !foundExact {
		t.Fatalf("exact run option missing: %+v", exact)
	}
}

// TestApprovalDecisionsRoundTripAndShortcutResolution proves the list survives the
// payload and that a one-letter IM answer resolves against the SAME list — the
// mechanism that gives WeChat the terminal's options.
func TestApprovalDecisionsRoundTripAndShortcutResolution(t *testing.T) {
	options := buildApprovalDecisions(tools.ToolApprovalRequest{
		ToolName:   "terminal",
		GrantClass: `"git" commands`,
		RuleCandidates: []tools.ApprovalRuleCandidate{
			{Kind: tools.ApprovalRuleKindNetworkHost, Key: "rule:net_host:api.github.com", Label: "network access to api.github.com"},
		},
	})
	payload, err := json.Marshal(map[string]interface{}{"decisions": options})
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeApprovalDecisions(payload)
	if len(decoded) != len(options) {
		t.Fatalf("round trip lost options: %d vs %d", len(decoded), len(options))
	}
	host, ok := approvalOptionByShortcut(decoded, "h")
	if !ok || host.GrantKey != "rule:net_host:api.github.com" {
		t.Fatalf("shortcut h must resolve to the host rule, got %+v", host)
	}
	if _, ok := approvalOptionByShortcut(decoded, "z"); ok {
		t.Fatal("an unknown letter must not resolve to any option")
	}
	// A row from before this batch decodes to nothing, so surfaces fall back to
	// their built-in options instead of rendering an empty menu.
	if got := decodeApprovalDecisions([]byte(`{"tool":"terminal"}`)); got != nil {
		t.Fatalf("legacy payload should carry no options, got %+v", got)
	}
	if lines := approvalOptionLines(decoded); !strings.Contains(lines, "h = ") {
		t.Fatalf("text surfaces must list the same menu: %q", lines)
	}
}

// TestBareReplyRuleShortcutPersistsTheOfferedRule is the IM half of B1/B2: "yp"
// stores the rule that ask offered, not a guess.
func TestBareReplyRuleShortcutPersistsTheOfferedRule(t *testing.T) {
	srv, store, identity, task, fixture := newApprovalTestServer(t)
	ctx := context.Background()
	// The fixture leaves one pending approval; a second pending row makes a bare
	// one-letter answer ambiguous by design, so retire it first.
	if err := store.ExpireApprovalRequest(ctx, identity.TenantID, fixture.ID, "test setup"); err != nil {
		t.Fatal(err)
	}

	options := buildApprovalDecisions(tools.ToolApprovalRequest{
		ToolName:   "terminal",
		GrantClass: `"git" commands`,
		RuleCandidates: []tools.ApprovalRuleCandidate{
			{Kind: tools.ApprovalRuleKindCommandPrefix, Key: "rule:exec_prefix:git status", Label: "commands that start with `git status`"},
		},
	})
	payload, _ := json.Marshal(map[string]interface{}{
		"tool": "terminal", "reason": "terminal requires approval in smart mode", "decisions": options,
	})
	approval, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		ActionType: "tool_call", Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}

	handled, reply, err := srv.tryHandleBareApprovalReply(ctx, identity, "yp", "weixin")
	if err != nil {
		t.Fatalf("bare reply: %v", err)
	}
	if !handled {
		t.Fatalf("yp must be claimed as an approval answer, reply = %q", reply)
	}
	stored, err := store.GetApprovalRequest(ctx, identity.TenantID, approval.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "approved" {
		t.Fatalf("status = %q", stored.Status)
	}
	if stored.DecisionGrantKey != "rule:exec_prefix:git status" {
		t.Fatalf("the offered rule must be recorded, got %q", stored.DecisionGrantKey)
	}
	if stored.DecisionScope != "person" {
		t.Fatalf("rule decisions carry their own scope, got %q", stored.DecisionScope)
	}
	if stored.DecisionID != "rule:exec_prefix" {
		t.Fatalf("decision id = %q, want the exact server-issued option", stored.DecisionID)
	}
}

// TestRejectionNoteIsStoredNotOnlySteered: a refusal with guidance is worth far
// more to the model than a bare no, and it must survive on the row.
func TestRejectionNoteIsStoredNotOnlySteered(t *testing.T) {
	_, store, identity, _, approval := newApprovalTestServer(t)
	ctx := context.Background()

	updated, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID,
		"rejected", "cli", control.ApprovalDecisionInput{Note: "use the staging bucket instead"})
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	if updated.DecisionNote != "use the staging bucket instead" {
		t.Fatalf("note = %q", updated.DecisionNote)
	}
	if updated.DecisionID != "deny" {
		t.Fatalf("decision id = %q, want deny", updated.DecisionID)
	}
	if got := fallbackApprovalReason(updated.DecisionNote, "rejected"); got != "use the staging bucket instead" {
		t.Fatalf("the model should receive the guidance, got %q", got)
	}

	// A rule must never be stored alongside a rejection.
	_, store2, identity2, _, approval2 := newApprovalTestServer(t)
	rejected, err := store2.RespondApprovalRequest(ctx, identity2.TenantID, identity2.PersonID, approval2.ID,
		"rejected", "cli", control.ApprovalDecisionInput{GrantScope: "person", GrantKey: "rule:exec_prefix:git status"})
	if err != nil {
		t.Fatalf("respond: %v", err)
	}
	if rejected.DecisionGrantKey != "" || rejected.DecisionScope != "" {
		t.Fatalf("a refusal must not carry an authorization: %+v", rejected)
	}
}

func TestRunIntentSnapshotSeparatesTaskContextFromAuthorization(t *testing.T) {
	task := &control.Task{Title: "Deploy RUQX-500", CurrentSummary: "Ready for production"}
	run := &control.Run{WorkKey: "RUQX-500"}
	workspace := &control.Workspace{ID: "ws-1"}
	snapshot := runIntentSnapshot(api.MessageRequest{Content: "开始执行"}, task, run, workspace)
	if snapshot.RawUserText != "开始执行" || snapshot.GoalSummary == "" || snapshot.WorkKey != "RUQX-500" || snapshot.WorkspaceID != "ws-1" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.Source != "continuation" || len(snapshot.ExplicitAllow) != 1 {
		t.Fatalf("continuation evidence = %+v", snapshot)
	}
	if len(snapshot.ExplicitDeny) != 0 {
		t.Fatalf("unexpected deny evidence = %+v", snapshot.ExplicitDeny)
	}
}

func TestRunIntentSnapshotSystemContinuationIsNotAuthorization(t *testing.T) {
	snapshot := runIntentSnapshot(api.MessageRequest{
		Content: "continue",
		Origin:  "external-watch-finalization",
	}, &control.Task{Title: "Deploy RUQX-500"}, &control.Run{WorkKey: "RUQX-500"}, nil)
	if snapshot.Source != "system:external-watch-finalization" {
		t.Fatalf("source = %q", snapshot.Source)
	}
	if len(snapshot.ExplicitAllow) != 0 || snapshot.UserAuthored() {
		t.Fatalf("system continuation became human authorization: %+v", snapshot)
	}
}
