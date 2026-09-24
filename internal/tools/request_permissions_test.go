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

func TestRequestPermissionsBatchesExactCommandsWithoutBypassingLivePolicy(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	resetTriageTelemetryForTest(t)
	root := t.TempDir()
	asks := 0
	capturedEffects := 0
	cleanup := SetExecutionScope("person-phase", ExecutionScope{
		TenantID: "tenant-phase", PersonID: "person-phase", TaskID: "task-phase", RunID: "run-phase",
		WorkspaceID: "ws-phase", WorkspaceRoot: root, AllowedRoots: []string{root},
		ApprovalMode: ApprovalSmart,
		IntentSnapshot: func() RunIntentSnapshot {
			return RunIntentSnapshot{ModelAuthorization: true, RawUserText: "publish the release"}
		},
		Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			asks++
			if req.ToolName == "request_permissions" {
				effects, _ := req.Args["effects"].([]interface{})
				capturedEffects = len(effects)
				return ToolApprovalDecision{Approved: true, ApprovalID: "apr-phase", Scope: "run", Outcome: ApprovalOutcomeApproved}, nil
			}
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr-single", Outcome: ApprovalOutcomeApproved}, nil
		},
	})
	defer cleanup()

	registry := NewRegistry()
	registry.Register(NewRequestPermissionsTool())
	registry.Register(NewExecuteCommandTool())
	command := "aws codebuild start-build --project-name site --profile release"
	out, err := registry.Dispatch("request_permissions", map[string]interface{}{
		"_tenant_id": "person-phase",
		"effects": []interface{}{
			map[string]interface{}{"tool": "terminal", "arguments_json": `{"command":"aws codebuild start-build --project-name site --profile release"}`},
			map[string]interface{}{"tool": "terminal", "arguments_json": `{"command":"gh run watch 123 --exit-status"}`},
		},
		"reason": "dispatch and observe this release phase",
	})
	if err != nil {
		t.Fatalf("request exact command phase: %v", err)
	}
	if asks != 1 || capturedEffects != 2 || !strings.Contains(out, "Granted (run)") {
		t.Fatalf("bundle result asks=%d effects=%d out=%q", asks, capturedEffects, out)
	}

	ran := 0
	exec := SmartApprovalMiddleware(root)(func(map[string]interface{}) (string, error) {
		ran++
		return "ok", nil
	})
	call := func(value string) error {
		_, callErr := exec(map[string]interface{}{
			"_tenant_id": "person-phase", "_tool_name": "terminal", "command": value,
		})
		return callErr
	}
	if err := call(command); err != nil {
		t.Fatalf("declared command: %v", err)
	}
	if asks != 1 || ran != 1 {
		t.Fatalf("declared command re-asked: asks=%d ran=%d", asks, ran)
	}
	if stats := TriageDiagnostics("tenant-phase", "person-phase"); stats.BundleHits != 1 {
		t.Fatalf("bundle reuse was not observable: %+v", stats)
	}

	if err := call("aws codebuild start-build --project-name other --profile release"); err != nil {
		t.Fatalf("changed command: %v", err)
	}
	if asks != 2 || ran != 2 {
		t.Fatalf("changed arguments reused the declaration: asks=%d ran=%d", asks, ran)
	}
}

func TestRequestPermissionsExactCommandsFailClosed(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	root := t.TempDir()
	asks := 0
	cleanup := SetExecutionScope("person-phase-closed", ExecutionScope{
		TenantID: "tenant-phase", PersonID: "person-phase-closed", TaskID: "task-phase", RunID: "run-phase-closed",
		WorkspaceID: "ws-phase", WorkspaceRoot: root, AllowedRoots: []string{root},
		ApprovalMode: ApprovalSmart,
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			asks++
			return ToolApprovalDecision{Approved: true, Scope: "run"}, nil
		},
	})
	defer cleanup()
	registry := NewRegistry()
	registry.Register(NewRequestPermissionsTool())
	registry.Register(NewExecuteCommandTool())
	registry.Register(NewExecuteCodeTool())

	for _, tc := range []struct {
		name string
		tool string
		args map[string]interface{}
	}{
		{name: "hard floor", tool: "terminal", args: map[string]interface{}{"command": "shutdown -h now"}},
		{name: "opaque script", tool: "terminal", args: map[string]interface{}{"command": "bash deploy.sh"}},
		{name: "arbitrary network client", tool: "terminal", args: map[string]interface{}{"command": "curl https://example.com/release"}},
		{name: "arbitrary code", tool: "execute_code", args: map[string]interface{}{"code": "print('release')", "language": "python"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := registry.Dispatch("request_permissions", map[string]interface{}{
				"_tenant_id": "person-phase-closed",
				"effects":    []interface{}{map[string]interface{}{"tool": tc.tool, "arguments_json": MarshalArgs(tc.args)}},
				"reason":     "run one phase",
			})
			if err == nil {
				t.Fatalf("%s must stay on single-call approval", tc.name)
			}
		})
	}
	if asks != 0 {
		t.Fatalf("invalid exact declarations reached the person %d times", asks)
	}
}

func TestDeclaredEffectKeyChangesWithExecutionIdentity(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	args := map[string]interface{}{"_tool_name": "terminal", "command": "aws codebuild start-build --project-name site"}
	annotateEffectiveSandboxMode(args)
	scope := ExecutionScope{
		TenantID: "tenant", PersonID: "person", TaskID: "task", RunID: "run", WorkspaceID: "workspace",
		EnvironmentSnapshotID: "snapshot", EnvironmentGeneration: 1, EnvironmentFingerprint: "environment",
		PrincipalFingerprint: "principal", CredentialSourceHash: "credentials",
	}
	first := approvalDeclaredEffectKey("terminal", args, scope, true)
	scope.EnvironmentGeneration++
	if second := approvalDeclaredEffectKey("terminal", args, scope, true); first == "" || first == second {
		t.Fatalf("environment change did not invalidate exact phase grant: %q / %q", first, second)
	}
	scope.EnvironmentGeneration--
	scope.PrincipalFingerprint = "other-principal"
	if second := approvalDeclaredEffectKey("terminal", args, scope, true); first == second {
		t.Fatal("principal change did not invalidate exact phase grant")
	}
}

func TestDeclaredEffectDoesNotOverrideCurrentExplicitDeny(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	root := t.TempDir()
	asks := 0
	scope := ExecutionScope{
		TenantID: "tenant-deny", PersonID: "person-deny", TaskID: "task-deny", RunID: "run-deny",
		WorkspaceID: "ws-deny", WorkspaceRoot: root, AllowedRoots: []string{root}, ApprovalMode: ApprovalSmart,
		IntentSnapshot: func() RunIntentSnapshot {
			return RunIntentSnapshot{ModelAuthorization: true, RawUserText: "prepare the release"}
		},
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			asks++
			if asks == 1 {
				return ToolApprovalDecision{Approved: true, Scope: "run"}, nil
			}
			return ToolApprovalDecision{Approved: true}, nil
		},
	}
	cleanup := SetExecutionScope("person-deny", scope)
	registry := NewRegistry()
	registry.Register(NewRequestPermissionsTool())
	registry.Register(NewExecuteCommandTool())
	command := "aws codebuild start-build --project-name site"
	if _, err := registry.Dispatch("request_permissions", map[string]interface{}{
		"_tenant_id": "person-deny",
		"effects": []interface{}{map[string]interface{}{
			"tool": "terminal", "arguments_json": MarshalArgs(map[string]interface{}{"command": command}),
		}},
		"reason": "release phase",
	}); err != nil {
		cleanup()
		t.Fatalf("request phase: %v", err)
	}
	installed, ok := currentExecutionScopeAny(map[string]interface{}{"_tenant_id": "person-deny"})
	cleanup()
	if !ok || installed.runGrants == nil {
		t.Fatal("missing live run grant set")
	}
	installed.IntentSnapshot = func() RunIntentSnapshot {
		return RunIntentSnapshot{
			ModelAuthorization: true,
			RawUserText:        "do not run commands yet",
			ExplicitDeny:       []string{"do not run commands yet"},
			DenyScopes: []DenyScope{{
				Marker: "do not", Clause: "do not run commands yet", Classes: []OperationClass{OpClassExec}, Resolved: true,
			}},
		}
	}
	cleanup = SetExecutionScope("person-deny", installed)
	defer cleanup()
	ran := false
	exec := SmartApprovalMiddleware(root)(func(map[string]interface{}) (string, error) {
		ran = true
		return "ok", nil
	})
	if _, err := exec(map[string]interface{}{
		"_tenant_id": "person-deny", "_tool_name": "terminal", "command": command,
	}); err != nil {
		t.Fatalf("human-confirmed deny override: %v", err)
	}
	if asks != 2 || !ran {
		t.Fatalf("declared effect bypassed current deny: asks=%d ran=%v", asks, ran)
	}
}
