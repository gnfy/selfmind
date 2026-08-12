package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestAuthMiddleware(t *testing.T) {
	// Middleware chain: TenantIsolationMiddleware -> AuthMiddleware -> inner executor
	// Applied in reverse: TenantIsolationMiddleware wraps AuthMiddleware, which wraps the inner ToolExecutor
	inner := func(args map[string]interface{}) (string, error) {
		return "ok", nil
	}
	exec := TenantIsolationMiddleware("test-tenant")(
		AuthMiddleware(&mockPermStorage{})(inner),
	)

	result, err := exec(map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Errorf("expected 'ok', got %q", result)
	}
}

// mockPermStorage implements the permission getter for AuthMiddleware
type mockPermStorage struct{}

func (m *mockPermStorage) GetPermission(ctx context.Context, tenantID, toolName string) (bool, error) {
	return true, nil
}

func TestApprovalMiddleware_DryRun(t *testing.T) {
	// dryRun=true should block execution
	mw := ApprovalMiddleware(true)
	exec := mw(func(args map[string]interface{}) (string, error) {
		return "should not reach", nil
	})
	_, err := exec(map[string]interface{}{})
	if err == nil {
		t.Error("expected error when dry_run=true")
	}
}

func TestApprovalMiddleware_Allow(t *testing.T) {
	// dryRun=false should allow
	mw := ApprovalMiddleware(false)
	exec := mw(func(args map[string]interface{}) (string, error) {
		return "executed", nil
	})
	result, err := exec(map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "executed" {
		t.Errorf("expected 'executed', got %q", result)
	}
}

func TestSmartApprovalMiddlewareUsesExecutionScopeApproval(t *testing.T) {
	cleanup := SetExecutionScope("person-a", ExecutionScope{
		TenantID: "tenant-a",
		PersonID: "person-a",
		TaskID:   "task-a",
		RunID:    "run-a",
		Approval: func(ctx context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			if req.ToolName != "terminal" {
				t.Fatalf("tool name = %q", req.ToolName)
			}
			if req.TaskID != "task-a" || req.RunID != "run-a" {
				t.Fatalf("approval request scope = %+v", req)
			}
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr-a"}, nil
		},
	})
	defer cleanup()

	called := false
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		called = true
		return "executed", nil
	})
	result, err := exec(map[string]interface{}{
		"_tenant_id": "person-a",
		"_tool_name": "terminal",
		"command":    "rm -rf tmp",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "executed" || !called {
		t.Fatalf("tool was not executed after approval: result=%q called=%v", result, called)
	}
}

func TestExplicitHostSandboxRequiresApproval(t *testing.T) {
	approvalCalled := false
	cleanup := SetExecutionScope("person-host", ExecutionScope{
		TenantID: "tenant-host",
		PersonID: "person-host",
		Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			approvalCalled = true
			if !strings.Contains(req.Reason, "host") {
				t.Fatalf("approval reason = %q", req.Reason)
			}
			return ToolApprovalDecision{Approved: true}, nil
		},
	})
	defer cleanup()

	executed := false
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		executed = true
		return "executed", nil
	})
	result, err := exec(map[string]interface{}{
		"_tenant_id": "person-host",
		"_tool_name": "terminal",
		"command":    "gcloud builds list",
		"sandbox":    "host",
	})
	if err != nil || result != "executed" || !approvalCalled || !executed {
		t.Fatalf("result=%q err=%v approval=%v executed=%v", result, err, approvalCalled, executed)
	}
}

func TestAutoSandboxHostFallbackIsApprovedAsHost(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	approvalCalled := false
	cleanup := SetExecutionScope("person-host-fallback", ExecutionScope{
		TenantID:     "tenant-host",
		PersonID:     "person-host-fallback",
		WorkspaceID:  "workspace-host-fallback",
		ApprovalMode: ApprovalOnRequest,
		Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			approvalCalled = true
			if !strings.Contains(req.Reason, "host") {
				t.Fatalf("fallback approval reason = %q", req.Reason)
			}
			if got := req.Args["effective_sandbox"]; got != "host (isolated sandbox unavailable or disabled)" {
				t.Fatalf("effective sandbox = %#v", got)
			}
			if req.ResourceFingerprint == "" {
				t.Fatalf("host fallback must have a workspace-scoped fingerprint")
			}
			return ToolApprovalDecision{Approved: true}, nil
		},
	})
	defer cleanup()

	executed := false
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		executed = true
		return "executed", nil
	})
	result, err := exec(map[string]interface{}{
		"_tenant_id": "person-host-fallback",
		"_tool_name": "terminal",
		"command":    "printf ok",
	})
	if err != nil || result != "executed" || !approvalCalled || !executed {
		t.Fatalf("result=%q err=%v approval=%v executed=%v", result, err, approvalCalled, executed)
	}
}

func TestWatchFinalizationProfileNeverWaitsForShellApproval(t *testing.T) {
	approvalCalled := false
	cleanup := SetExecutionScope("person-watch", ExecutionScope{
		TenantID:         "tenant-watch",
		PersonID:         "person-watch",
		ExecutionProfile: ExecutionProfileWatchFinalization,
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			approvalCalled = true
			return ToolApprovalDecision{Approved: true}, nil
		},
	})
	defer cleanup()

	executed := false
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		executed = true
		return "executed", nil
	})
	_, err := exec(map[string]interface{}{
		"_tenant_id": "person-watch",
		"_tool_name": "terminal",
		"command":    "grep SUCCESS release.md",
	})
	if err == nil || !strings.Contains(err.Error(), "operation rejected: unattended watcher finalization") {
		t.Fatalf("terminal rejection = %v", err)
	}
	if approvalCalled || executed {
		t.Fatalf("unattended shell reached approval/executor: approval=%v executed=%v", approvalCalled, executed)
	}
}

func TestWatchFinalizationProfileAllowsSafeWorkspaceFileTool(t *testing.T) {
	cleanup := SetExecutionScope("person-watch-file", ExecutionScope{
		TenantID:         "tenant-watch",
		PersonID:         "person-watch-file",
		WorkspaceRoot:    t.TempDir(),
		ExecutionProfile: ExecutionProfileWatchFinalization,
	})
	defer cleanup()

	executed := false
	exec := SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		executed = true
		return "executed", nil
	})
	result, err := exec(map[string]interface{}{
		"_tenant_id": "person-watch-file",
		"_tool_name": "write_file",
		"path":       "release.md",
		"content":    "completed",
	})
	if err != nil || result != "executed" || !executed {
		t.Fatalf("safe file tool result=%q err=%v executed=%v", result, err, executed)
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	rl := RateLimit(2)
	mw := rl.Middleware
	exec := mw(func(args map[string]interface{}) (string, error) {
		return "ok", nil
	})

	_, err := exec(map[string]interface{}{})
	if err != nil {
		t.Fatalf("call 1 failed: %v", err)
	}
	_, err = exec(map[string]interface{}{})
	if err != nil {
		t.Fatalf("call 2 failed: %v", err)
	}
	_, err = exec(map[string]interface{}{})
	if err == nil {
		t.Error("expected rate limit error on call 3")
	}
}

func TestLoggingMiddleware(t *testing.T) {
	exec := LoggingMiddleware(func(args map[string]interface{}) (string, error) {
		return "result", nil
	})
	result, err := exec(map[string]interface{}{"key": "value"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "result" {
		t.Errorf("expected 'result', got %q", result)
	}
}

func TestTenantIsolationMiddleware(t *testing.T) {
	mw := TenantIsolationMiddleware("tenant-abc")
	exec := mw(func(args map[string]interface{}) (string, error) {
		if args["_tenant_id"] != "tenant-abc" {
			return "", fmt.Errorf("tenant mismatch: got %v", args["_tenant_id"])
		}
		return "ok", nil
	})
	_, err := exec(map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnvVarMiddleware_Missing(t *testing.T) {
	t.Setenv("TEST_VAR", "")
	mw := EnvVarMiddleware("TEST_VAR")
	exec := mw(func(args map[string]interface{}) (string, error) {
		return "should not reach", nil
	})
	_, err := exec(map[string]interface{}{})
	if err == nil {
		t.Error("expected error for missing env var")
	}
}

func TestEnvVarMiddleware_Set(t *testing.T) {
	t.Setenv("TEST_VAR", "exists")
	mw := EnvVarMiddleware("TEST_VAR")
	exec := mw(func(args map[string]interface{}) (string, error) {
		return "ok", nil
	})
	result, err := exec(map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ok" {
		t.Errorf("expected 'ok', got %q", result)
	}
}

func TestToolGuardrailsBlockRepeatedFailure(t *testing.T) {
	guard := NewToolGuardrails()
	exec := guard.Middleware(func(args map[string]interface{}) (string, error) {
		return "", fmt.Errorf("same failure")
	})
	args := map[string]interface{}{"_tool_name": "read_file", "_tenant_id": "tenant-a", "path": "missing.txt"}
	for i := 0; i < 2; i++ {
		if _, err := exec(args); err == nil {
			t.Fatalf("call %d should fail", i+1)
		}
	}
	if _, err := exec(args); err == nil || !strings.Contains(err.Error(), "guardrail blocked") {
		t.Fatalf("expected guardrail block, got %v", err)
	}
}

func TestDangerousToolCallDetection(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    bool
	}{
		{"plain rm", "rm -rf build", true},
		{"absolute path rm bypass", "/bin/rm -rf /tmp/x", true},
		{"piped killall", "ps aux | killall -9 node", true},
		{"chained shutdown", "echo hi && shutdown now", true},
		{"env-prefixed dd", "FOO=bar dd if=/dev/zero of=disk", true},
		{"fork bomb", ":(){ :|:& };:", true},
		{"redirect to device", "echo x > /dev/sda", true},
		{"safe build", "go build ./...", false},
		{"safe list", "ls -la && cat README.md", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := dangerousToolCall("", "terminal", map[string]interface{}{"command": tc.command})
			if got != tc.want {
				t.Fatalf("dangerousToolCall(%q) = %v (reason %q), want %v", tc.command, got, reason, tc.want)
			}
		})
	}
}

func TestRedactSensitive(t *testing.T) {
	input := `Authorization: Bearer abcdefghijklmnop token=secret-value api_key: sk-testsecret123456789`
	out := RedactSensitive(input)
	if strings.Contains(out, "secret-value") || strings.Contains(out, "abcdefghijklmnop") || strings.Contains(out, "sk-testsecret") {
		t.Fatalf("sensitive data was not redacted: %s", out)
	}
}

func TestRedactSensitivePreservesCredentialReferences(t *testing.T) {
	input := `TOKEN="$(gcloud auth print-access-token)" curl -H "Authorization: Bearer ${TOKEN}"`
	if got := RedactSensitive(input); got != input {
		t.Fatalf("credential reference structure changed:\n got: %s\nwant: %s", got, input)
	}
	literal := `GH_TOKEN="literal-secret-value" api_key=another-secret-value`
	got := RedactSensitive(literal)
	if strings.Contains(got, "literal-secret-value") || strings.Contains(got, "another-secret-value") {
		t.Fatalf("literal credentials leaked: %s", got)
	}
}

func TestApprovalDisplayArgsRedactsNestedSecrets(t *testing.T) {
	got := approvalDisplayArgs(map[string]interface{}{
		"command": `curl -H "Authorization: Bearer literal-secret-token"`,
		"env": map[string]interface{}{
			"API_TOKEN": "plain-secret-value",
		},
	})
	command, _ := got["command"].(string)
	nested, _ := got["env"].(map[string]interface{})
	if strings.Contains(command, "literal-secret-token") || strings.Contains(fmt.Sprintf("%v", nested), "plain-secret-value") {
		t.Fatalf("approval args leaked a credential: %#v", got)
	}
}
