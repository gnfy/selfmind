package httpapi

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/tools"
)

// A durable poll runs under the execution policy its registering Run froze,
// never the one the daemon happens to be configured with when the poll fires.
// An operator who disables the sandbox after registration does not move an
// existing watch onto the host; one who tightens it still narrows the watch.
// A watch registered before the policy was recorded keeps today's behaviour:
// the configured policy.
func TestDurablePollKeepsTheRegisteringRunsSandboxPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux and macOS")
	}
	server, store, identity, _, _ := newClarifyTestServer(t)
	ctx := context.Background()
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	tools.InstallEnvironmentSnapshot(os.Environ(), "inherited")
	configured := tools.CurrentExecSandboxPolicy()
	t.Cleanup(func() { tools.SetExecSandbox(configured.Enabled, configured.Required, configured.AllowNetwork) })

	workspace, err := store.RegisterWorkspace(ctx, control.Workspace{TenantID: identity.TenantID,
		OwnerPersonID: identity.PersonID, Name: "workspace", LocalPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	watch := control.ExternalWatch{ID: "watch_frozen_policy", TenantID: identity.TenantID, PersonID: identity.PersonID,
		WorkspaceID: workspace.ID, CWD: t.TempDir(), Command: "printf PENDING",
		ExecutionBinding: executionenv.Binding{
			Version: executionenv.BindingVersion, TenantID: identity.TenantID, PersonID: identity.PersonID,
			WorkspaceID: workspace.ID, TrustLevel: executionenv.TrustUntrusted,
			ExecutionCapabilities: []string{executionenv.CapabilityNetworkShared},
			CapabilityBindings: []executionenv.CapabilityBinding{{Capability: executionenv.CapabilityNetworkShared,
				Source: executionenv.CapabilitySourceRegistration, ExpiresAt: time.Now().Add(time.Hour)}},
		}}
	poll := func(name string, frozen *executionenv.SandboxPolicy, enabled, required bool) (tools.SandboxMode, error) {
		t.Helper()
		watch.ExecutionBinding.SandboxPolicy = frozen
		tools.SetExecSandbox(enabled, required, true)
		result, err := server.runExternalWatchCommand(ctx, watch)
		t.Logf("%s poll: mode=%q err=%v", name, result.Plan.Mode, err)
		if err != nil {
			return "", err
		}
		if result.Output != "PENDING" {
			t.Fatalf("%s: poll output = %q", name, result.Output)
		}
		return result.Plan.Mode, nil
	}

	// Registered under a required sandbox; the operator later turns it off.
	// The poll is isolated or refused, never run on the host.
	if mode, err := poll("loosened", &executionenv.SandboxPolicy{Enabled: true, Required: true}, false, false); err == nil && mode != tools.SandboxIsolated {
		t.Fatalf("a poll registered under a required sandbox ran as %q after the sandbox was disabled", mode)
	}
	// Registered without isolation; the operator now requires it.
	if mode, err := poll("tightened", &executionenv.SandboxPolicy{}, true, true); err == nil && mode != tools.SandboxIsolated {
		t.Fatalf("a stricter configuration did not narrow the poll: ran as %q", mode)
	}
	// A watch registered before the policy was recorded follows the
	// configured one.
	if mode, err := poll("legacy", nil, false, false); err != nil || mode != tools.SandboxHost {
		t.Fatalf("legacy watch poll = %q, %v; want the configured host policy", mode, err)
	}
}
