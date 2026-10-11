package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
)

// benchmarkApprovalFunnel measures one approval decision for a long payload
// that reaches the human ask, so every key and assessment the funnel derives
// is computed: a 10 KB chain and a 600 KB chain wrapped in `sh -c`.
func benchmarkApprovalFunnel(b *testing.B, command string, leased bool) {
	prevEnabled, prevRequired, prevNetwork := execSandboxPolicy()
	SetExecSandbox(true, true, false)
	b.Cleanup(func() { SetExecSandbox(prevEnabled, prevRequired, prevNetwork) })
	person := "person-funnel-bench"
	scope := ExecutionScope{
		TenantID: "tenant-funnel-bench", PersonID: person, TaskID: "task-funnel-bench", RunID: "run-funnel-bench",
		WorkspaceID: "ws-funnel-bench", WorkspaceRoot: "/workspace/app", ApprovalMode: ApprovalOnRequest,
		Grants: newFakeGrantStore(), StandingGrants: InteractiveStandingGrants(),
		TrustLevel: executionenv.TrustTrusted,
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr-bench"}, nil
		},
	}
	if leased {
		// A lease makes the credential axis consult the profile selection, as
		// in a gateway run.
		home, err := os.UserHomeDir()
		if err != nil {
			b.Skip("no home directory")
		}
		if err := executionenv.SetRuntimeRoot(filepath.Join(home, ".selfmind-test-runtime", "funnel-bench")); err != nil {
			b.Skipf("runtime root unavailable: %v", err)
		}
		registry := executionenv.NewRegistry()
		previous := executionenv.DefaultRegistry()
		executionenv.SetDefaultRegistry(registry)
		b.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
		snapshot := registry.Install([]string{"PATH=/usr/bin:/bin", "HOME=" + home}, "inherited", "principal-bench", nil)
		registry.BindLease("lease-funnel-bench", snapshot.ID)
		scope.LeaseID = "lease-funnel-bench"
	}
	b.Cleanup(SetExecutionScope(person, scope))
	exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { return "ok", nil })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := exec(map[string]interface{}{"_tenant_id": person, "_tool_name": "terminal", "command": command}); err != nil {
			b.Fatal(err)
		}
	}
}

// One approval derives the call's containment once and hands it to every
// decision that needs it, the exact run key included; deriving it again
// doubled the cost of a long command.
func TestApprovalDerivesContainmentOncePerCall(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	person := "person-funnel-once"
	scope := ExecutionScope{
		TenantID: "tenant-funnel-once", PersonID: person, TaskID: "task-funnel-once", RunID: "run-funnel-once",
		WorkspaceID: "ws-funnel-once", WorkspaceRoot: "/workspace/app", ApprovalMode: ApprovalOnRequest,
		Grants: newFakeGrantStore(), StandingGrants: InteractiveStandingGrants(),
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr-once"}, nil
		},
	}
	t.Cleanup(SetExecutionScope(person, scope))
	exec := SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) { return "ok", nil })
	for _, command := range []string{"python3 deploy.py", strings.Repeat("echo x && ", 200) + "echo done"} {
		before := containmentAssessments.Load()
		if _, err := exec(map[string]interface{}{"_tenant_id": person, "_tool_name": "terminal", "command": command}); err != nil {
			t.Fatal(err)
		}
		if got := containmentAssessments.Load() - before; got != 1 {
			t.Errorf("one approval of %.20q derived containment %d times, want 1", command, got)
		}
	}
	// The constraint that must change the result: the run key is a function
	// of the assessment it is handed, so a changed boundary asks again.
	args := map[string]interface{}{"_tool_name": "terminal", "command": "python3 deploy.py"}
	isolated := approvalExactRunKey("terminal", args, scope, true, ContainmentAssessment{Filesystem: containmentFilesystemIsolated})
	host := approvalExactRunKey("terminal", args, scope, true, ContainmentAssessment{Filesystem: containmentFilesystemHost})
	if isolated == "" || isolated == host {
		t.Errorf("run key ignores the assessment: isolated %q, host %q", isolated, host)
	}
}

func BenchmarkApprovalFunnel10KChain(b *testing.B) {
	benchmarkApprovalFunnel(b, strings.Repeat("echo x && ", 1000)+"echo done", false)
}

func BenchmarkApprovalFunnel600KWrappedChain(b *testing.B) {
	benchmarkApprovalFunnel(b, "sh -c '"+strings.Repeat("echo x && ", 60000)+"echo done'", false)
}

func BenchmarkApprovalFunnel10KChainLeased(b *testing.B) {
	benchmarkApprovalFunnel(b, strings.Repeat("echo x && ", 1000)+"echo done", true)
}

func BenchmarkApprovalFunnel600KWrappedChainLeased(b *testing.B) {
	benchmarkApprovalFunnel(b, "sh -c '"+strings.Repeat("echo x && ", 60000)+"echo done'", true)
}
