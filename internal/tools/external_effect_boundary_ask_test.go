package tools

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/control"
)

// foreignClaimFixture is another Run's left-uncertain effect on the target.
func foreignClaimFixture() []control.ExternalEffectClaim {
	return []control.ExternalEffectClaim{{
		ID: "claim-foreign", RunID: "run-other", Summary: "npm run deploy",
		Disposition: control.ExternalClaimLeftUncertain, Outcome: "exit 1",
	}}
}

// askForeignEffect runs the claim boundary's own once-only ask for a call and
// returns the request the person was shown.
func askForeignEffect(t *testing.T, args map[string]interface{}) ToolApprovalRequest {
	t.Helper()
	var shown ToolApprovalRequest
	scope := ExecutionScope{
		TenantID: "tenant-foreign", PersonID: "person-foreign", TaskID: "task-foreign", RunID: "run-foreign",
		Approval: func(_ context.Context, request ToolApprovalRequest) (ToolApprovalDecision, error) {
			shown = request
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr-foreign"}, nil
		},
	}
	if err := approveForeignExternalEffects(context.Background(), scope, args, foreignClaimFixture()); err != nil {
		t.Fatal(err)
	}
	if shown.ToolName == "" {
		t.Fatal("the person was not asked")
	}
	return shown
}

// The funnel's own ask leads with the boundary facts and carries the
// containment summary; the claim boundary's separate ask releases the same
// exact call, so it states them too: an approver of a call that runs on the
// host, or writes a credential cache in place, is told so whichever surface
// asks.
func TestForeignEffectAskStatesTheCallsBoundary(t *testing.T) {
	base := func() map[string]interface{} {
		return map[string]interface{}{"_tool_name": "terminal", "command": "npm run deploy", "cwd": t.TempDir()}
	}
	t.Run("host fallback", func(t *testing.T) {
		args := base()
		args["_effective_sandbox_mode"] = string(SandboxHost)
		args[effectiveSandboxReasonArg] = "exec sandbox disabled by configuration"
		request := askForeignEffect(t, args)
		if !strings.HasPrefix(request.Reason, "runs on the host: exec sandbox disabled by configuration; another run left an external effect") {
			t.Fatalf("the ask must lead with the boundary fact and keep the claim: %q", request.Reason)
		}
		if !strings.Contains(request.Containment, "filesystem=host") {
			t.Fatalf("the ask must carry the containment summary: %q", request.Containment)
		}
	})
	// The constraint that must change the result: a contained call has no
	// boundary fact to lead with, and its ask is the claim's alone.
	t.Run("contained", func(t *testing.T) {
		args := base()
		args["_effective_sandbox_mode"] = string(SandboxIsolated)
		request := askForeignEffect(t, args)
		if !strings.HasPrefix(request.Reason, "another run left an external effect") {
			t.Fatalf("a contained call's ask must be the claim's alone: %q", request.Reason)
		}
		if !strings.Contains(request.Containment, "filesystem=isolated") {
			t.Fatalf("the ask must carry the containment summary: %q", request.Containment)
		}
	})
	// Only an exec call has a boundary to state. A tool nothing has assessed
	// would otherwise be shown as "no network, no credentials".
	t.Run("not an exec tool", func(t *testing.T) {
		request := askForeignEffect(t, map[string]interface{}{"_tool_name": "mcp__deploy__run", "text": "ship it"})
		if request.Containment != "" {
			t.Fatalf("an unassessed tool must carry no containment summary: %q", request.Containment)
		}
		if !strings.HasPrefix(request.Reason, "another run left an external effect") {
			t.Fatalf("an unassessed tool's ask must be the claim's alone: %q", request.Reason)
		}
	})
}
