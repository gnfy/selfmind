package tools

import (
	"context"
	"errors"
	"fmt"
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
		RunID:           "run-network",
		WorkspaceID:     "workspace-network",
		TrustLevel:      executionenv.TrustUntrusted,
		CapabilityStore: store,
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			approvals++
			return ToolApprovalDecision{Approved: true, Scope: "run"}, nil
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
	if output != "connected" || calls != 1 || approvals != 1 || store.writes != 0 {
		t.Fatalf("output=%q calls=%d approvals=%d writes=%d", output, calls, approvals, store.writes)
	}
	if _, err := executor(args); err != nil || approvals != 1 {
		t.Fatalf("run-local network grant was not reused: err=%v approvals=%d", err, approvals)
	}
}

func TestExecutionCapabilityMiddlewareNeverReplaysUnknownCommand(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	store := &capabilityStoreStub{}
	cleanup := SetExecutionScope("person-network-unknown", ExecutionScope{
		TenantID:        "tenant-network",
		PersonID:        "person-network-unknown",
		RunID:           "run-network-unknown",
		WorkspaceID:     "workspace-network",
		TrustLevel:      executionenv.TrustUntrusted,
		CapabilityStore: store,
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			return ToolApprovalDecision{Approved: true, Scope: "run"}, nil
		},
	})
	defer cleanup()

	calls := 0
	executor := ExecutionCapabilityMiddleware()(func(args map[string]interface{}) (string, error) {
		calls++
		if shared, _ := args["_network_shared"].(bool); shared {
			return "connected", nil
		}
		return "local side effect completed", errors.New("network is disabled")
	})
	args := map[string]interface{}{
		"_tenant_id": "person-network-unknown",
		"_tool_name": "terminal",
		"command":    "custom-agent sync",
	}
	_, err := executor(args)
	if err == nil || calls != 1 || store.writes != 0 {
		t.Fatalf("err=%v calls=%d writes=%d", err, calls, store.writes)
	}
	if output, retryErr := executor(args); retryErr != nil || output != "connected" || calls != 2 {
		t.Fatalf("explicit retry did not use run grant: output=%q err=%v calls=%d", output, retryErr, calls)
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

func TestTrustedWorkspaceSelectsCredentialsOnlyForMatchingTools(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	cleanup := SetExecutionScope("person-trusted-credentials", ExecutionScope{
		TenantID:    "tenant-trusted",
		PersonID:    "person-trusted-credentials",
		WorkspaceID: "workspace-trusted",
		TrustLevel:  executionenv.TrustTrusted,
	})
	defer cleanup()

	executor := ExecutionCapabilityMiddleware()(func(args map[string]interface{}) (string, error) {
		allowed, _ := args[credentialReadArgKey].(bool)
		return fmt.Sprintf("credentials=%t", allowed), nil
	})
	local, err := executor(map[string]interface{}{
		"_tenant_id": "person-trusted-credentials",
		"_tool_name": "terminal",
		"command":    "git diff --stat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if local != "credentials=false" {
		t.Fatalf("local observation unexpectedly received operator credentials: %s", local)
	}

	cloud, err := executor(map[string]interface{}{
		"_tenant_id": "person-trusted-credentials",
		"_tool_name": "terminal",
		"command":    "gcloud projects get-iam-policy demo-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cloud != "credentials=true" {
		t.Fatalf("credential-bearing CLI did not receive operator credentials: %s", cloud)
	}
}

// The network twin of TestTrustedWorkspaceSelectsCredentialsOnlyForMatchingTools.
//
// An operator policy that allows egress used to hand it to EVERY trusted
// command. A network-shared call is not contained, so the sandbox's own
// "isolated, no egress, no credentials" release could never fire and purely
// local work queued for approval behind commands that genuinely reach a remote
// service. Measured over 666 real approvals, 267 needed neither egress nor
// credentials.
func TestTrustedWorkspaceSharesNetworkOnlyWithCommandsThatReachIt(t *testing.T) {
	// enabled, required=false, allow_network=TRUE — the permissive operator
	// policy that made this blanket.
	withExecSandboxPolicy(t, true, false, true)
	cleanup := SetExecutionScope("person-trusted-network", ExecutionScope{
		TenantID:    "tenant-trusted",
		PersonID:    "person-trusted-network",
		WorkspaceID: "workspace-trusted",
		TrustLevel:  executionenv.TrustTrusted,
	})
	defer cleanup()

	var captured map[string]interface{}
	executor := ExecutionCapabilityMiddleware()(func(args map[string]interface{}) (string, error) {
		captured = args
		shared, _ := args["_network_shared"].(bool)
		return fmt.Sprintf("network=%t", shared), nil
	})
	run := func(t *testing.T, command string) string {
		t.Helper()
		out, err := executor(map[string]interface{}{
			"_tenant_id":              "person-trusted-network",
			"_tool_name":              "terminal",
			"_effective_sandbox_mode": string(SandboxIsolated),
			"command":                 command,
		})
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		return out
	}

	for _, command := range []string{"git diff --stat", "rg pattern .", "cat notes.md | wc -l"} {
		if got := run(t, command); got != "network=false" {
			t.Fatalf("a command that never leaves the host must not be given egress: %s -> %s", command, got)
		}
	}
	// The payoff: with no egress and no credentials, an enforced sandbox
	// contains this call, so it never reaches the approval queue at all.
	if ExecSandboxAvailable() {
		assessment := assessExecContainment("terminal", captured)
		if assessment.Network != containmentNetworkNone || !assessment.AutoApprove() {
			t.Fatalf("a contained local command must auto-approve: %+v", assessment)
		}
	}

	// The constraint that must change the result, in both of its forms: a
	// credential-bearing CLI exists to talk to a remote service, and an
	// explicit egress program says so outright.
	for _, command := range []string{
		"gcloud projects get-iam-policy demo-project",
		"aws sts get-caller-identity --profile prod",
		"curl https://example.com/health",
	} {
		if got := run(t, command); got != "network=true" {
			t.Fatalf("a command that reaches a remote service must keep egress: %s -> %s", command, got)
		}
	}
	// ...and such a call is NOT contained, so it stays gated on proving itself
	// an observation rather than riding the sandbox release.
	if got := assessExecContainment("terminal", captured); got.Network != containmentNetworkShared {
		t.Fatalf("an egress command must report shared network: %+v", got)
	}
}
