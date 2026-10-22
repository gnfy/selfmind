package tools

import (
	"errors"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
)

// newNetworkClaimFixture is the production order at parallel capacity 2: the
// capability middleware decides the network for the call, and the claim
// middleware, below approval, claims external targets from that decision.
func newNetworkClaimFixture(t *testing.T, trust string, store ExecutionCapabilityStore, respond func(string) (kernel.ToolDispatchResult, error)) (*effectLifecycleFixture, *lifecycleExecTool) {
	t.Helper()
	f := newEffectLifecycleFixture(t, 2)
	f.registry = NewRegistry()
	f.registry.UseMiddleware(ExecutionCapabilityMiddleware(ExternalEffectPrecheck(f.store)))
	f.registry.UseResultMiddleware(ExternalEffectClaimMiddleware(f.store))
	shell := newLifecycleExecTool("terminal", respond)
	shell.schema.Properties["network"] = PropertyDef{Type: "boolean"}
	f.registry.Register(shell)
	for _, run := range f.runs {
		cleanup := SetExecutionScope(ExecutionScopeKeyForRun(run.ID), ExecutionScope{TenantID: f.owner.TenantID,
			PersonID: f.owner.PersonID, RunID: run.ID, TaskID: run.TaskID, WorkspaceID: "workspace", WorkspaceRoot: f.roots[run.ID],
			AllowedRoots: []string{f.roots[run.ID]}, ParallelWork: true, TrustLevel: trust, CapabilityStore: store,
			SandboxPolicy: &ExecSandboxPolicy{Enabled: true, AllowNetwork: true}})
		t.Cleanup(cleanup)
	}
	return f, shell
}

func (f *effectLifecycleFixture) dispatchIsolated(run *control.Run, call, command string, extra map[string]interface{}) (kernel.ToolDispatchResult, error) {
	f.t.Helper()
	args := map[string]interface{}{"command": command, "_effective_sandbox_mode": string(SandboxIsolated)}
	for key, value := range extra {
		args[key] = value
	}
	return f.dispatch(run, "terminal", call, args)
}

// A command that does not run with network cannot affect an external system, so
// a failing test run claims nothing at capacity 2; a failing install or push
// still leaves its uncertain claim. Before this, a network grant networked every
// command, and a failing `go test` blocked or asked another Run.
func TestFailingLocalCommandLeavesNoClaimAtParallelCapacity(t *testing.T) {
	failing := func(prefixes ...string) func(string) (kernel.ToolDispatchResult, error) {
		return func(command string) (kernel.ToolDispatchResult, error) {
			for _, prefix := range prefixes {
				if strings.HasPrefix(command, prefix) {
					return exitedResult(1), errors.New("command failed: exit status 1")
				}
			}
			return exitedResult(0), nil
		}
	}
	scenarios := []struct {
		name  string
		trust string
		store ExecutionCapabilityStore
	}{
		{"stored grant in an untrusted workspace", executionenv.TrustUntrusted, networkOnlyCapabilityStore{}},
		{"trusted workspace", executionenv.TrustTrusted, nil},
	}
	for _, sc := range scenarios {
		t.Run(sc.name+"/local failures claim nothing", func(t *testing.T) {
			f, shell := newNetworkClaimFixture(t, sc.trust, sc.store, failing("go test", "npm test", "pytest", "./scripts/check.sh"))
			for i, command := range []string{"go test ./...", "npm test", "pytest -q", "./scripts/check.sh"} {
				if _, err := f.dispatchIsolated(f.runs[0], "local-"+string(rune('a'+i)), command, nil); err == nil {
					t.Fatalf("%s: the failing command reported success", command)
				}
			}
			if left := f.unresolved(); len(left) != 0 {
				t.Fatalf("a failing command that never ran with network left a claim: %+v", left)
			}
			// Another Run's network command is not asked about or parked behind them.
			if _, err := f.dispatchIsolated(f.runs[1], "push", "git push origin main", nil); err != nil {
				t.Fatalf("a networked command was blocked by local failures: %v", err)
			}
			if got := shell.commands(); len(got) != 5 {
				t.Fatalf("commands run = %v", got)
			}
		})
		t.Run(sc.name+"/networked failures still claim", func(t *testing.T) {
			f, shell := newNetworkClaimFixture(t, sc.trust, sc.store, failing("npm install", "git push"))
			if _, err := f.dispatchIsolated(f.runs[0], "install", "npm install", nil); err == nil {
				t.Fatal("the failing install reported success")
			}
			left := f.unresolved()
			if len(left) != 1 || left[0].State != control.ExternalClaimUncertain || left[0].TargetKey != control.UnknownExternalTarget {
				t.Fatalf("a failing networked command must leave an uncertain person-wide claim: %+v", left)
			}
			before := len(shell.commands())
			if _, err := f.dispatchIsolated(f.runs[1], "push", "git push origin main", nil); err == nil || len(shell.commands()) != before {
				t.Fatalf("the other Run's networked command was not stopped by the unresolved claim: err=%v", err)
			}
		})
		t.Run(sc.name+"/an explicit request claims", func(t *testing.T) {
			f, _ := newNetworkClaimFixture(t, sc.trust, sc.store, failing("make verify"))
			if _, err := f.dispatchIsolated(f.runs[0], "verify", "make verify", map[string]interface{}{"network": true}); err == nil {
				t.Fatal("the failing check reported success")
			}
			if left := f.unresolved(); len(left) != 1 {
				t.Fatalf("a call that ran with network on request must claim like any networked call: %+v", left)
			}
		})
	}
}
