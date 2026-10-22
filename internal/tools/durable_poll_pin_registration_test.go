package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
)

// The pin is recorded where the approval happened: at registration, from the
// boundary the funnel resolved for that very call.
func TestExternalWatchRegistrationRecordsTheApprovedBoundary(t *testing.T) {
	requireIsolationBackend(t)
	for _, tc := range []struct {
		name     string
		approved string
		reason   string
		policy   *ExecSandboxPolicy
		process  bool
		want     string
	}{
		{name: "approved-isolated", approved: "isolated", policy: &ExecSandboxPolicy{Enabled: true, AllowNetwork: true}, process: true, want: "isolated"},
		{name: "approved-host", approved: "host", reason: "exec sandbox disabled by configuration", policy: &ExecSandboxPolicy{AllowNetwork: true}, want: "host"},
		// A registration no approval funnel resolved records no pin.
		{name: "no-decision", policy: &ExecSandboxPolicy{AllowNetwork: true}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withExecSandboxPolicy(t, tc.process, false, true)
			ctx := context.Background()
			store, err := control.OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			person, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "pin-"+tc.name, "Watch")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "Watch a deploy", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "cli", "Watch a deploy")
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(fixtureBase(t), "home")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			tenant, workspace := profileExecScope(t, home, "", executionenv.TrustTrusted)
			previous, _, _ := lookupExecutionScope(tenant)
			snapshot, ok := executionenv.DefaultRegistry().ForLease(previous.LeaseID)
			if !ok {
				t.Fatal("fixture snapshot missing")
			}
			lease, err := store.MaterializeExecutionLease(ctx, executionenv.Lease{ID: previous.LeaseID, TenantID: person.TenantID, PersonID: person.PersonID,
				RunID: run.ID, WorkspaceID: "ws-pin", EnvironmentSnapshotID: snapshot.ID, EnvironmentGeneration: snapshot.Generation,
				PrincipalFingerprint: snapshot.PrincipalFingerprint, EnvironmentFingerprint: snapshot.EnvironmentFingerprint, CredentialSourceHash: snapshot.CredentialSourceHash})
			if err != nil {
				t.Fatal(err)
			}
			scope := previous
			scope.TenantID, scope.PersonID, scope.TaskID, scope.RunID, scope.WorkspaceID = person.TenantID, person.PersonID, task.ID, run.ID, lease.WorkspaceID
			scope.SandboxPolicy = tc.policy
			defer SetExecutionScope(tenant, scope)()
			if err := os.WriteFile(filepath.Join(workspace, "observed.state"), []byte("RUNNING\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := map[string]interface{}{
				"_tenant_id": tenant, "command": "cat observed.state", "cwd": workspace,
				"terminal_success_pattern": "^SUCCEEDED$", "terminal_failure_pattern": "^FAILED$",
			}
			if tc.approved != "" {
				args["_effective_sandbox_mode"], args[effectiveSandboxReasonArg] = tc.approved, tc.reason
			}
			result, err := NewExternalWatchTool(store).Execute(args)
			var response struct {
				WatchID string `json:"watch_id"`
			}
			if err != nil || json.Unmarshal([]byte(result), &response) != nil || response.WatchID == "" {
				t.Fatalf("registration: %v (%s)", err, result)
			}
			watch, err := store.GetExternalWatch(ctx, person.TenantID, response.WatchID)
			if err != nil || watch == nil {
				t.Fatalf("load watch: %v", err)
			}
			if got := watch.ExecutionBinding.ApprovedBoundary; got != tc.want {
				t.Fatalf("the binding pinned %q, want %q", got, tc.want)
			}
		})
	}
}
