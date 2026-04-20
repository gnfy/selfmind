package tools

import (
	"context"
	"fmt"
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
