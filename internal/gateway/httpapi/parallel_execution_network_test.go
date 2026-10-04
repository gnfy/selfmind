package httpapi

import (
	"context"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

func TestParallelExecutionScopeRetainsConfiguredNetwork(t *testing.T) {
	prior := tools.CurrentExecSandboxPolicy()
	tools.SetExecSandbox(true, true, true)
	t.Cleanup(func() { tools.SetExecSandbox(prior.Enabled, prior.Required, prior.AllowNetwork) })
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	server := &Server{Control: store, DefaultTenantID: "default"}
	coord := server.coordinator()
	coord.activeLimit = 2
	run := &control.Run{ID: "run-parallel-network", Channel: "cli"}
	workspace := &control.Workspace{ID: "workspace", LocalPath: root, TrustLevel: executionenv.TrustTrusted}
	cleanup := coord.installExecutionScope(ctx, identity, nil, run, workspace, api.MessageRequest{
		Channel: "cli", ExecutionRoots: []executionenv.RootBinding{{
			Path: root, Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite,
			Source: executionenv.RootSourceWorkspace,
		}},
	})
	defer cleanup()
	observed := false
	exec := tools.ExecutionCapabilityMiddleware()(func(args map[string]interface{}) (string, error) {
		observed = true
		if args["_network_shared"] != true || args["_effective_sandbox_mode"] != string(tools.SandboxIsolated) {
			t.Fatalf("parallel run lost configured isolated network: %+v", args)
		}
		return "observed", nil
	})
	_, err = exec(map[string]interface{}{
		"_tenant_id": identity.PersonID, "_tool_name": "terminal", "command": "gcloud builds describe build-123",
		"_invocation_scope": kernel.ToolInvocationScope{ExecutionScopeKey: tools.ExecutionScopeKeyForRun(run.ID)},
	})
	if err != nil || !observed {
		t.Fatalf("parallel run network observation: observed=%v err=%v", observed, err)
	}
}
