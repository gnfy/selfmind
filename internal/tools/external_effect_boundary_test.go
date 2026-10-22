package tools

import (
	"strings"
	"testing"
	"time"
)

// A call refused at the execution boundary before any process launches did
// nothing, so the external-effect claim it took is released instead of being
// left uncertain for work that never ran. The refusal is the pinned boundary:
// host execution under a policy that requires isolation, which no approval can
// widen.
func TestRefusedStartReleasesItsExternalEffectClaim(t *testing.T) {
	for _, tc := range []struct {
		name       string
		background bool
	}{
		{"background start", true},
		// The same invariant through the foreground engine's own refusal.
		{"foreground command", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEffectLifecycleFixture(t, 1)
			f.registry.Register(NewExecuteCommandTool())
			run := f.runs[0]
			key := ExecutionScopeKeyForRun(run.ID)
			scope, ok, _ := lookupExecutionScope(key)
			if !ok {
				t.Fatal("fixture: the run's scope is missing")
			}
			scope.SandboxPolicy = &ExecSandboxPolicy{Enabled: true, Required: true}
			t.Cleanup(SetExecutionScope(key, scope))

			result, err := f.dispatch(run, "terminal", "call-refused", map[string]interface{}{
				"command": "touch never-ran", "background": tc.background, "sandbox": "host", "_network_shared": true,
			})
			if err == nil || !strings.Contains(err.Error(), "exec_sandbox.required") {
				t.Fatalf("host execution under required isolation must be refused: %+v %v", result, err)
			}
			if result.Process == nil || result.Process.Started {
				t.Fatalf("a refused start must report that no process started: %+v", result.Process)
			}
			if left := f.unresolved(); len(left) != 0 {
				t.Fatalf("a call that never started left its claim unresolved: %+v", left)
			}
			if strings.Contains(err.Error(), "External effect claim") {
				t.Fatalf("a call that never started was told it holds a claim: %v", err)
			}
		})
	}

	// The constraint that must change the result: the same call with no
	// isolation requirement starts, and keeps its claim until it is observed.
	f := newEffectLifecycleFixture(t, 1)
	f.registry.Register(NewExecuteCommandTool())
	result, err := f.dispatch(f.runs[0], "terminal", "call-started", map[string]interface{}{
		"command": "touch ran; sleep 2; exit 0", "background": true, "sandbox": "host", "_network_shared": true,
	})
	if err != nil || result.Process == nil || !result.Process.Started {
		t.Fatalf("a permitted background start must run: %+v %v", result, err)
	}
	if left := f.unresolved(); len(left) != 1 {
		t.Fatalf("a started background command holds its claim: %+v", left)
	}
	// This positive control owns a real background process. Wait for its exit
	// observer before closing the store and deleting its workspace; returning
	// while it still runs races TempDir cleanup and loses the observer's write.
	deadline := time.Now().Add(30 * time.Second)
	for len(f.unresolved()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("test process exit was not observed before cleanup")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
