package tools

import (
	"context"
	"strings"
	"testing"
)

func newPermissionScope(t *testing.T, grants *fakeGrantStore, handler ToolApprovalHandler) func() {
	t.Helper()
	return SetExecutionScope("person-perm", ExecutionScope{
		TenantID: "tenant-perm", PersonID: "person-perm", TaskID: "task-perm", RunID: "run-perm",
		WorkspaceID: "ws-perm", WorkspaceRoot: "/workspace/app",
		ApprovalMode: ApprovalSmart, Grants: grants, Approval: handler,
	})
}

// TestRequestPermissionsAsksOnceForTheWholeBundle is batch C3's point: work whose
// shape is known up front should cost ONE decision, not one per operation
// discovered by failing.
func TestRequestPermissionsAsksOnceForTheWholeBundle(t *testing.T) {
	grants := newFakeGrantStore()
	asks := 0
	var policy string
	cleanup := newPermissionScope(t, grants, func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
		asks++
		policy = req.DecisionPolicy
		return ToolApprovalDecision{Approved: true, ApprovalID: "apr_perm", Scope: "run", Outcome: ApprovalOutcomeApproved}, nil
	})
	defer cleanup()

	out, err := requestPermissionsExecutor(map[string]interface{}{
		"_tenant_id": "person-perm",
		"_tool_name": "request_permissions",
		"paths":      []interface{}{"/srv/site", "/workspace/app/build"},
		"hosts":      []interface{}{"api.github.com"},
		"reason":     "publish the built site and read the release API",
	})
	if err != nil {
		t.Fatalf("request_permissions: %v", err)
	}
	if asks != 1 {
		t.Fatalf("the bundle must cost exactly one ask, got %d", asks)
	}
	if policy != ApprovalDecisionPolicyRunBundle {
		t.Fatalf("decision policy = %q, want run bundle", policy)
	}
	if !strings.Contains(out, "Granted (run)") || !strings.Contains(out, "api.github.com") {
		t.Fatalf("result should report what was granted: %q", out)
	}
	for _, key := range []string{
		approvalRuleKey(ApprovalRuleKindPathRoot, "/srv/site"),
		approvalRuleKey(ApprovalRuleKindNetworkHost, "api.github.com"),
	} {
		scope, ok := currentExecutionScope(map[string]interface{}{"_tenant_id": "person-perm"})
		if !ok || scope.runGrants == nil || !scope.runGrants.has(key) {
			t.Fatalf("expected a run grant for %q", key)
		}
	}

	// A second identical request must NOT re-ask: otherwise a retrying agent turns
	// this tool into an approval-spamming loop.
	out, err = requestPermissionsExecutor(map[string]interface{}{
		"_tenant_id": "person-perm", "_tool_name": "request_permissions",
		"paths": []interface{}{"/srv/site"}, "hosts": []interface{}{"api.github.com"},
		"reason": "same work",
	})
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	if asks != 1 {
		t.Fatalf("already-granted permissions must not ask again, asks = %d", asks)
	}
	if !strings.Contains(out, "Already granted") {
		t.Fatalf("result should say the permissions are already held: %q", out)
	}
}

// TestRequestPermissionsRefusalIsADecision keeps the model from "helpfully" falling
// back to per-command asks after the person said no.
func TestRequestPermissionsRefusalIsADecision(t *testing.T) {
	cleanup := newPermissionScope(t, newFakeGrantStore(), func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
		return ToolApprovalDecision{Approved: false, ApprovalID: "apr_no", Outcome: ApprovalOutcomeDenied, Reason: "use the staging bucket"}, nil
	})
	defer cleanup()

	_, err := requestPermissionsExecutor(map[string]interface{}{
		"_tenant_id": "person-perm", "_tool_name": "request_permissions",
		"paths": []interface{}{"/srv/site"}, "reason": "publish",
	})
	if err == nil {
		t.Fatal("a refused bundle must be an error")
	}
	if !strings.HasPrefix(err.Error(), "operation rejected:") {
		t.Fatalf("refusal must use the user-decision contract, got %v", err)
	}
	if !strings.Contains(err.Error(), "staging bucket") {
		t.Fatalf("the person's guidance must reach the model: %v", err)
	}
}

// TestRequestPermissionsRefusesOverbroadRequests: a permission nobody could reason
// about later must not be requestable at all.
func TestRequestPermissionsRefusesOverbroadRequests(t *testing.T) {
	cleanup := newPermissionScope(t, newFakeGrantStore(), func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
		t.Fatal("an invalid request must never reach the person")
		return ToolApprovalDecision{}, nil
	})
	defer cleanup()
	t.Setenv("HOME", "/home/tester")

	cases := []struct {
		name string
		args map[string]interface{}
	}{
		{"filesystem root", map[string]interface{}{"paths": []interface{}{"/"}, "reason": "everything"}},
		{"whole home", map[string]interface{}{"paths": []interface{}{"/home/tester"}, "reason": "everything"}},
		{"relative path", map[string]interface{}{"paths": []interface{}{"../elsewhere"}, "reason": "x"}},
		{"not a host", map[string]interface{}{"hosts": []interface{}{"deploy.sh"}, "reason": "x"}},
		{"no reason", map[string]interface{}{"paths": []interface{}{"/srv/site"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]interface{}{"_tenant_id": "person-perm", "_tool_name": "request_permissions"}
			for k, v := range tc.args {
				args[k] = v
			}
			if _, err := requestPermissionsExecutor(args); err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
		})
	}
}

// TestRequestPermissionsWorkspaceOnlyNeedsNothing: the honest answer to "may I
// write inside my own workspace" is that no grant is involved.
func TestRequestPermissionsWorkspaceOnlyNeedsNothing(t *testing.T) {
	cleanup := newPermissionScope(t, newFakeGrantStore(), func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
		t.Fatal("workspace-only work must not ask")
		return ToolApprovalDecision{}, nil
	})
	defer cleanup()

	out, err := requestPermissionsExecutor(map[string]interface{}{
		"_tenant_id": "person-perm", "_tool_name": "request_permissions",
		"paths": []interface{}{"/workspace/app/dist"}, "reason": "write build output",
	})
	if err != nil {
		t.Fatalf("workspace-only request: %v", err)
	}
	if !strings.Contains(out, "No permissions requested") {
		t.Fatalf("result = %q", out)
	}
}
