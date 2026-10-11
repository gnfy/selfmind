package httpapi

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/executionenv"
	"selfmind/internal/tools"
)

// A durable poll runs from the watch row alone. The row's binding carries what
// the registering Run froze — its sandbox policy — and what its approval
// covered — the in-place credential writes — and the two guarantees only mean
// something together: after the operator turns the sandbox off, a poll that
// followed the configuration would run on the host, where nothing holds it to
// its approved writes. These polls are loaded from the store and run through the
// daemon's own poll path under a configuration that changed after registration.
func TestStoredWatchPollKeepsItsFrozenPolicyAndRegisteredWrites(t *testing.T) {
	host := newAWSHost(t, "stored-poll", awsSSOConfig)
	ctx := context.Background()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Watch a deploy", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "watch a deploy")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.RegisterWorkspace(ctx, control.Workspace{TenantID: identity.TenantID,
		OwnerPersonID: identity.PersonID, Name: "workspace", LocalPath: host.workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetWorkspaceTrust(ctx, identity.TenantID, identity.PersonID, workspace.ID, executionenv.TrustTrusted, "local_cli"); err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store, DefaultTenantID: "default", ExecutionRuntime: newTestExecutionRuntime(t)}

	register := func(frozen *executionenv.SandboxPolicy, writes ...string) control.ExternalWatch {
		t.Helper()
		created, err := store.CreateExternalWatch(ctx, control.ExternalWatch{
			TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
			WorkspaceID: workspace.ID, Channel: "cli", CWD: host.workspace, Command: awsCommand,
			SuccessPattern: "^SUCCESS$", TimeoutAt: time.Now().Add(time.Hour),
			ExecutionBinding: executionenv.Binding{
				Version: executionenv.BindingVersion, TrustLevel: executionenv.TrustTrusted,
				ExecutionCapabilities: []string{executionenv.CapabilityNetworkShared},
				CapabilityBindings: []executionenv.CapabilityBinding{{
					Capability: executionenv.CapabilityNetworkShared, Source: executionenv.CapabilitySourceTrust}},
				SandboxPolicy: frozen, HostStateWrites: writes,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := store.GetExternalWatch(ctx, identity.TenantID, created.ID)
		if err != nil || loaded == nil {
			t.Fatalf("load watch: %v", err)
		}
		return *loaded
	}
	frozen := &executionenv.SandboxPolicy{Enabled: true}

	// The operator turned the sandbox off after these watches registered.
	withSandboxPolicy(t, false, false, true)

	// Its registration covered exactly the plan: the poll keeps the isolation
	// it froze and writes only those caches.
	result, err := server.runExternalWatchCommand(ctx, register(frozen, host.cli, host.sso))
	if err != nil || !strings.Contains(result.Output, "cache write ok") {
		t.Fatalf("a poll within its registered writes must run: %v (%s)", err, result.Output)
	}
	if result.Plan.Mode != tools.SandboxIsolated {
		t.Fatalf("the poll must keep the isolation its registration froze, ran %q", result.Plan.Mode)
	}
	if got := slices.Sorted(slices.Values(result.Plan.HostStateWrites)); !slices.Equal(got, []string{host.cli, host.sso}) {
		t.Fatalf("the poll wrote %v in place, want its registered %v", got, []string{host.cli, host.sso})
	}
	if err := removeCaches(host); err != nil {
		t.Fatal(err)
	}

	// The configuration grew a write the registration never covered: the poll
	// is refused before anything is created, and it did not move to the host.
	result, err = server.runExternalWatchCommand(ctx, register(frozen, host.sso))
	if err == nil || result.Started || notDispatchedCode(t, err) != "approved_host_writes_changed" {
		t.Fatalf("a poll beyond its registered writes must be refused unstarted: started=%v err=%v (%s)", result.Started, err, result.Output)
	}
	if result.Plan.Mode == tools.SandboxHost {
		t.Fatalf("a poll registered under isolation ran on the host: %+v", result.Plan)
	}
	host.requireNoCache(t, "a poll the registration never covered ran")

	// A watch stored before either field existed: the binding column holds the
	// JSON an earlier version wrote. It follows the configured policy, and
	// wherever that isolates it covers no writes.
	legacy := register(nil)
	raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE external_watches SET execution_binding_json = ? WHERE id = ?`,
		`{"version":1,"id":"binding:`+legacy.ID+`","tenant_id":"`+identity.TenantID+`","person_id":"`+identity.PersonID+
			`","workspace_id":"`+workspace.ID+`","trust_level":"trusted","execution_capabilities":["network:shared"],`+
			`"capability_bindings":[{"capability":"network:shared","source":"workspace_trust"}]}`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetExternalWatch(ctx, identity.TenantID, legacy.ID)
	if err != nil || loaded == nil || loaded.ExecutionBinding.SandboxPolicy != nil || len(loaded.ExecutionBinding.HostStateWrites) != 0 {
		t.Fatalf("fixture: the stored row must read as an older one: %+v %v", loaded, err)
	}
	withSandboxPolicy(t, true, false, true)
	result, err = server.runExternalWatchCommand(ctx, *loaded)
	if err == nil || result.Started || notDispatchedCode(t, err) != "approved_host_writes_changed" {
		t.Fatalf("an older row covers no in-place writes: started=%v err=%v (%s)", result.Started, err, result.Output)
	}
	host.requireNoCache(t, "an older row's poll wrote state no approval ever covered")
	withSandboxPolicy(t, false, false, true)
	if result, err = server.runExternalWatchCommand(ctx, *loaded); err != nil || result.Plan.Mode != tools.SandboxHost {
		t.Fatalf("an older row with no frozen policy follows the configured one (host): %v mode=%q (%s)", err, result.Plan.Mode, result.Output)
	}
}

func removeCaches(host *awsHost) error {
	for _, path := range []string{host.sso, host.cli} {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}
