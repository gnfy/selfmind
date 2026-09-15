package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSemanticReviewProtectsInScopeWriteWithoutKeywordGate(t *testing.T) {
	root := t.TempDir()
	judge := &fakeJudge{reply: `{"risk_level":"low","user_authorization":"low","outcome":"deny","rationale":"The person requested preparation only; writing the deliverable is premature."}`}
	scope := ExecutionScope{TenantID: "semantic", PersonID: "semantic", RunID: "semantic", WorkspaceRoot: root, AllowedRoots: []string{root}, ApprovalMode: ApprovalSmart, Judge: judge, IntentSnapshot: func() RunIntentSnapshot {
		return RunIntentSnapshot{ModelAuthorization: true, RawUserText: "Prepare the delivery and wait for my confirmation."}
	}}
	cleanup := SetExecutionScope("semantic", scope)
	defer cleanup()
	ran := false
	exec := SmartApprovalMiddleware(root)(func(map[string]interface{}) (string, error) { ran = true; return "", nil })
	_, err := exec(map[string]interface{}{"_tenant_id": "semantic", "_tool_name": "write_file", "path": root + "/receipt.txt", "content": "VERIFIED"})
	if judge.calls != 1 || ran || err == nil || !strings.Contains(err.Error(), "operation rejected") {
		t.Fatalf("premature write bypassed semantic review: calls=%d ran=%v err=%v", judge.calls, ran, err)
	}
}

func TestSemanticRestrictionCannotBeBypassedByOldGrant(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	judge := &fakeJudge{reply: `{"risk_level":"low","user_authorization":"low","outcome":"deny","rationale":"The script must remain unchanged."}`}
	store := newFakeGrantStore()
	scope := ExecutionScope{TenantID: "semantic-grant", PersonID: "semantic-grant", RunID: "semantic-grant", ApprovalMode: ApprovalSmart, Judge: judge, Grants: store, IntentSnapshot: func() RunIntentSnapshot {
		return RunIntentSnapshot{ModelAuthorization: true, RawUserText: "Leave the script unchanged."}
	}}
	// A previous capability grant does not authorize today's prohibited effect.
	_, reason := dangerousToolCall("", "terminal", map[string]interface{}{"command": "chmod +x script.sh"})
	key := approvalPatternKeyForScope("terminal", map[string]interface{}{"command": "chmod +x script.sh"}, reason, scope, true)
	if key == "" {
		t.Fatal("missing grant fixture")
	}
	_ = store.GrantApproval(context.Background(), "person", "semantic-grant", "semantic-grant", "semantic-grant", key, time.Time{})
	_, _ = runSmart(t, scope, "semantic-grant", "chmod +x script.sh")
	if judge.calls != 1 {
		t.Fatal("old grant bypassed the current human restriction")
	}
}

func TestSemanticReviewRejectsIncompleteJudgment(t *testing.T) {
	for _, reply := range []string{`{"risk_level":"low","user_authorization":"high","rationale":"Looks safe"}`, `APPROVE`, `{"outcome":"approve"}`} {
		judge := &fakeJudge{reply: reply}
		verdict, _, err := triageApprovalWithIntent(context.Background(), judge, "write_file", "receipt.txt", "effect authorization", RunIntentSnapshot{ModelAuthorization: true, RawUserText: "Write receipt.txt"})
		if err == nil || verdict != TriageEscalate || judge.calls != 1 {
			t.Fatalf("invalid judgment inferred permission: reply=%q verdict=%v err=%v", reply, verdict, err)
		}
	}
}

func TestUnansweredApprovalReturnsTypedRunPause(t *testing.T) {
	scope := ExecutionScope{TenantID: "approval-pause", PersonID: "approval-pause", RunID: "approval-pause", ApprovalMode: ApprovalOnRequest,
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			return ToolApprovalDecision{ApprovalID: "apr-pending", Outcome: ApprovalOutcomeTimedOut}, nil
		}}
	ran, err := runSmart(t, scope, "approval-pause", "rm -rf build")
	var pause interface{ ToolRunPause() (string, string, bool) }
	if ran || !errors.As(err, &pause) {
		t.Fatalf("unanswered approval must pause before effects: ran=%v err=%v", ran, err)
	}
	reason, message, approval := pause.ToolRunPause()
	if reason != "waiting_user" || message == "" || !approval {
		t.Fatalf("missing approval handoff: %q %q %v", reason, message, approval)
	}
}
