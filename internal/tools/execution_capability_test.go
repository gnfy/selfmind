package tools

import (
	"context"
	"errors"
	"testing"
	"time"

	"selfmind/internal/executionenv"
)

type capabilityStoreStub struct {
	granted bool
	writes  int
}

func (s *capabilityStoreStub) HasExecutionCapability(context.Context, string, string, string, string, string) (bool, error) {
	return s.granted, nil
}

func (s *capabilityStoreStub) GrantExecutionCapability(
	context.Context,
	string,
	string,
	string,
	string,
	string,
	string,
	time.Time,
) error {
	s.granted = true
	s.writes++
	return nil
}

func TestExecutionCapabilityMiddlewareApprovesBeforeKnownNetworkCommand(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	store := &capabilityStoreStub{}
	approvals := 0
	cleanup := SetExecutionScope("person-network", ExecutionScope{
		TenantID:        "tenant-network",
		PersonID:        "person-network",
		WorkspaceID:     "workspace-network",
		TrustLevel:      executionenv.TrustUntrusted,
		CapabilityStore: store,
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			approvals++
			return ToolApprovalDecision{Approved: true, Scope: "task"}, nil
		},
	})
	defer cleanup()

	calls := 0
	executor := ExecutionCapabilityMiddleware()(func(args map[string]interface{}) (string, error) {
		calls++
		if shared, _ := args["_network_shared"].(bool); !shared {
			return "", errors.New("dial tcp: network is unreachable")
		}
		return "connected", nil
	})
	args := map[string]interface{}{
		"_tenant_id": "person-network",
		"_tool_name": "terminal",
		"command":    "curl https://example.test",
	}
	output, err := executor(args)
	if err != nil {
		t.Fatal(err)
	}
	if output != "connected" || calls != 1 || approvals != 1 || store.writes != 1 {
		t.Fatalf("output=%q calls=%d approvals=%d writes=%d", output, calls, approvals, store.writes)
	}
}

func TestExecutionCapabilityMiddlewareNeverReplaysUnknownCommand(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	store := &capabilityStoreStub{}
	cleanup := SetExecutionScope("person-network-unknown", ExecutionScope{
		TenantID:        "tenant-network",
		PersonID:        "person-network-unknown",
		WorkspaceID:     "workspace-network",
		TrustLevel:      executionenv.TrustUntrusted,
		CapabilityStore: store,
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			return ToolApprovalDecision{Approved: true, Scope: "task"}, nil
		},
	})
	defer cleanup()

	calls := 0
	executor := ExecutionCapabilityMiddleware()(func(map[string]interface{}) (string, error) {
		calls++
		return "local side effect completed", errors.New("network is disabled")
	})
	_, err := executor(map[string]interface{}{
		"_tenant_id": "person-network-unknown",
		"_tool_name": "terminal",
		"command":    "custom-agent sync",
	})
	if err == nil || calls != 1 || store.writes != 1 {
		t.Fatalf("err=%v calls=%d writes=%d", err, calls, store.writes)
	}
}

func TestExecutionCapabilityMiddlewareUsesExistingGrantWithoutApproval(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	store := &capabilityStoreStub{granted: true}
	cleanup := SetExecutionScope("person-network-existing", ExecutionScope{
		TenantID:        "tenant-network",
		PersonID:        "person-network-existing",
		WorkspaceID:     "workspace-network",
		TrustLevel:      executionenv.TrustUntrusted,
		CapabilityStore: store,
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			t.Fatal("existing capability must not ask again")
			return ToolApprovalDecision{}, nil
		},
	})
	defer cleanup()

	executor := ExecutionCapabilityMiddleware()(func(args map[string]interface{}) (string, error) {
		if shared, _ := args["_network_shared"].(bool); !shared {
			t.Fatal("existing network capability was not applied")
		}
		return "connected", nil
	})
	if _, err := executor(map[string]interface{}{
		"_tenant_id": "person-network-existing",
		"_tool_name": "terminal",
		"command":    "curl https://example.test",
	}); err != nil {
		t.Fatal(err)
	}
}
