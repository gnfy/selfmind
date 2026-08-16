package tools

import (
	"context"
	"testing"
)

func TestRunScopedApprovalAvoidsRepeatWithoutPersisting(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	resetTriageTelemetryForTest(t)
	asks := 0
	executions := 0
	install := func(runID string) func() {
		return SetExecutionScope("person-run-grant", ExecutionScope{
			TenantID: "default", PersonID: "person-run-grant", TaskID: "task-1", RunID: runID, WorkspaceID: "ws-run",
			WorkspaceRoot: t.TempDir(), ApprovalMode: ApprovalOnRequest,
			Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
				asks++
				return ToolApprovalDecision{Approved: true, ApprovalID: "apr-run", Scope: "run"}, nil
			},
		})
	}
	exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) {
		executions++
		return "ok", nil
	})
	call := func(runID string) {
		t.Helper()
		ctx := WithExecutionScopeKey(context.Background(), ExecutionScopeKeyForRun(runID))
		if _, err := exec(map[string]interface{}{
			"_tenant_id": "person-run-grant", "_context": ctx,
			"_tool_name": "terminal", "command": "git status",
		}); err != nil {
			t.Fatalf("terminal call: %v", err)
		}
	}

	cleanup := install("run-1")
	call("run-1")
	scope, ok := currentExecutionScopeAny(map[string]interface{}{
		"_context": WithExecutionScopeKey(context.Background(), ExecutionScopeKeyForRun("run-1")),
	})
	if !ok || scope.runGrants == nil {
		t.Fatal("run-scoped approval memory was not installed")
	}
	scope.runGrants.mu.RLock()
	granted := make([]string, 0, len(scope.runGrants.keys))
	for key := range scope.runGrants.keys {
		granted = append(granted, key)
	}
	scope.runGrants.mu.RUnlock()
	if len(granted) == 0 {
		t.Fatal("approved run did not remember any authorization key")
	}
	call("run-1")
	cleanup()
	if asks != 1 || executions != 2 {
		t.Fatalf("same run asks=%d executions=%d grants=%v, want 1/2", asks, executions, granted)
	}

	cleanup = install("run-2")
	defer cleanup()
	call("run-2")
	if asks != 2 {
		t.Fatalf("run grant leaked into the next run: asks=%d", asks)
	}
	stats := TriageDiagnostics("default", "person-run-grant")
	if stats.ExactRunHits != 1 || stats.HumanAsks != 2 {
		t.Fatalf("approval funnel stats = %+v, want one exact run hit and two human asks", stats)
	}
}

func TestHostScriptApprovalIsOnceOnly(t *testing.T) {
	resetTriageTelemetryForTest(t)
	asks := 0
	executions := 0
	cleanup := SetExecutionScope("person-exact-run", ExecutionScope{
		TenantID: "default", PersonID: "person-exact-run", TaskID: "task-exact", RunID: "run-exact",
		WorkspaceID: "ws-exact", WorkspaceRoot: t.TempDir(), ApprovalMode: ApprovalOnRequest,
		Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			asks++
			if req.DecisionPolicy != ApprovalDecisionPolicyOnceOnly || req.GrantClass != "" || req.RunGrantClass != "" {
				t.Fatalf("host script must offer once-only approval: %+v", req)
			}
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr-exact"}, nil
		},
	})
	defer cleanup()
	exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) {
		executions++
		return "ok", nil
	})
	call := func(code string) {
		t.Helper()
		ctx := WithExecutionScopeKey(context.Background(), ExecutionScopeKeyForRun("run-exact"))
		if _, err := exec(map[string]interface{}{
			"_tenant_id": "person-exact-run", "_context": ctx,
			"_tool_name": "execute_code", "code": code,
		}); err != nil {
			t.Fatalf("execute_code: %v", err)
		}
	}
	call("print('same')")
	call("print('same')")
	call("print('changed')")
	if asks != 3 || executions != 3 {
		t.Fatalf("asks=%d executions=%d, want 3/3", asks, executions)
	}
	stats := TriageDiagnostics("default", "person-exact-run")
	if stats.ExactRunHits != 0 || stats.HumanAsks != 3 {
		t.Fatalf("approval funnel stats = %+v, want no exact-run reuse and three human asks", stats)
	}
}
