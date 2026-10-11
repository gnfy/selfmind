package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/tools/sandbox"
)

// awsScalarCacheStub refreshes the SSO token cache when its state allows the
// write and reports one scalar either way, as a watch's check must.
const awsScalarCacheStub = "#!/bin/sh\n" +
	"cache=\"$HOME/.aws/sso/cache\"\n" +
	"mkdir -p \"$cache\" 2>/dev/null\n" +
	"printf '{\"token\":\"watch\"}\\n' 2>/dev/null > \"$cache/watch.json\"\n" +
	"echo PENDING\n"

// registeredAWSWatch registers a watch through the production order —
// capability resolution, approval, the registration tool, its preflight — in a
// trusted workspace whose host holds the given AWS configuration, and returns
// the row read back from the store, which is all a poll has to go on.
func registeredAWSWatch(t *testing.T, name, config string) (*control.ExternalWatch, string) {
	t.Helper()
	ctx := context.Background()
	base := fixtureBase(t)
	home := awsStateHome(t, base, config)
	bin := writeStub(t, base, "aws", awsScalarCacheStub)
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	person, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "register-"+name, "Watch")
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
	tenant, workspace := profileExecScope(t, home, bin, executionenv.TrustTrusted)
	previous, _, _ := lookupExecutionScope(tenant)
	snapshot, ok := executionenv.DefaultRegistry().ForLease(previous.LeaseID)
	if !ok {
		t.Fatal("fixture snapshot missing")
	}
	lease, err := store.MaterializeExecutionLease(ctx, executionenv.Lease{ID: previous.LeaseID, TenantID: person.TenantID, PersonID: person.PersonID,
		RunID: run.ID, WorkspaceID: "ws-register-" + name, EnvironmentSnapshotID: snapshot.ID, EnvironmentGeneration: snapshot.Generation,
		PrincipalFingerprint: snapshot.PrincipalFingerprint, EnvironmentFingerprint: snapshot.EnvironmentFingerprint, CredentialSourceHash: snapshot.CredentialSourceHash})
	if err != nil {
		t.Fatal(err)
	}
	scope := previous
	scope.TenantID, scope.PersonID, scope.TaskID, scope.RunID, scope.WorkspaceID = person.TenantID, person.PersonID, task.ID, run.ID, lease.WorkspaceID
	scope.SandboxPolicy = &ExecSandboxPolicy{Enabled: true, AllowNetwork: true}
	t.Cleanup(SetExecutionScope(tenant, scope))

	register := ExecutionCapabilityMiddleware()(SmartApprovalMiddleware("")(NewExternalWatchTool(store).Execute))
	result, err := register(map[string]interface{}{
		"_tenant_id": tenant, "_tool_name": "watch_external", "command": "aws sts get-caller-identity --profile sso-dev", "cwd": workspace,
		"terminal_success_pattern": "^SUCCEEDED$", "terminal_failure_pattern": "^FAILED$",
	})
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
	return watch, home
}

func pollRegisteredWatch(t *testing.T, watch *control.ExternalWatch) (ExecutionResult, error) {
	t.Helper()
	return RunDurableCheck(WithExecutionRuntime(t.Context(), NewExecutionRuntime("", executionenv.RuntimeRoot(), sandbox.PrivateState{}, nil)), watch.Command, watch.CWD, true, DurableExecutionScope{
		ScratchKey: watch.ID, TenantID: watch.TenantID, PersonID: watch.PersonID,
		WorkspaceID: watch.WorkspaceID, Binding: watch.ExecutionBinding,
	})
}

// The registration and the polls meet in the stored row. A watch registered
// under isolation records that isolation and the in-place writes its approval
// covered; read back from the store, that row alone keeps every later poll
// inside both after the operator turns the sandbox off, and refuses the poll
// once the host's configuration asks for more than the approval covered.
func TestRegisteredWatchPollsUnderWhatItsRegistrationRecorded(t *testing.T) {
	requireInPlaceBackend(t)

	t.Run("registered-under-sso", func(t *testing.T) {
		withExecSandboxPolicy(t, true, false, true)
		watch, home := registeredAWSWatch(t, "sso", ssoProfileConfig)
		root := canonicalPath(t, home)
		sso, cli := filepath.Join(root, ".aws", "sso", "cache"), filepath.Join(root, ".aws", "cli", "cache")
		binding := watch.ExecutionBinding
		if binding.SandboxPolicy == nil || !binding.SandboxPolicy.Enabled {
			t.Fatalf("the registration must freeze the isolation it ran under: %+v", binding.SandboxPolicy)
		}
		if binding.ApprovedBoundary != "isolated" {
			t.Fatalf("the registration must pin the boundary its funnel approved, got %q", binding.ApprovedBoundary)
		}
		if got := slices.Sorted(slices.Values(binding.HostStateWrites)); !slices.Equal(got, []string{cli, sso}) {
			t.Fatalf("the registration must record the writes its approval covered, got %v", got)
		}
		// Registering ran the check once, writing exactly those caches; the poll
		// starts from a clean host.
		for _, path := range []string{sso, cli} {
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
		}

		// The operator turns the sandbox off afterwards.
		withExecSandboxPolicy(t, false, false, true)
		result, err := pollRegisteredWatch(t, watch)
		if err != nil || strings.TrimSpace(result.Output) != "PENDING" {
			t.Fatalf("a registered poll must run: %v (%s)", err, result.Output)
		}
		if _, statErr := os.Stat(filepath.Join(sso, "watch.json")); statErr != nil {
			t.Fatalf("the poll did not refresh the SSO cache it was approved to write: %v", statErr)
		}
		if result.Plan.Mode != SandboxIsolated {
			t.Fatalf("the poll left the isolation its registration recorded: %q", result.Plan.Mode)
		}
		if got := slices.Sorted(slices.Values(result.Plan.HostStateWrites)); !slices.Equal(got, []string{cli, sso}) {
			t.Fatalf("the poll wrote %v in place, want exactly the registered %v", got, []string{cli, sso})
		}
	})

	t.Run("registered-before-the-profile-needed-writes", func(t *testing.T) {
		withExecSandboxPolicy(t, true, false, true)
		watch, home := registeredAWSWatch(t, "static", "[default]\nregion=us-east-1\n[profile sso-dev]\nregion=us-east-1\n")
		if len(watch.ExecutionBinding.HostStateWrites) != 0 {
			t.Fatalf("fixture: a static profile needs no in-place write: %v", watch.ExecutionBinding.HostStateWrites)
		}
		// The profile now authenticates through SSO.
		writeAWSConfig(t, home, ssoProfileConfig)

		withExecSandboxPolicy(t, false, false, true)
		result, err := pollRegisteredWatch(t, watch)
		requireWriteSetRefusal(t, result, err)
		if result.Plan.Mode == SandboxHost {
			t.Fatalf("a poll registered under isolation ran on the host: %+v", result.Plan)
		}
		for _, path := range []string{filepath.Join(canonicalPath(t, home), ".aws", "sso", "cache"), filepath.Join(canonicalPath(t, home), ".aws", "cli", "cache")} {
			if _, statErr := os.Stat(path); statErr == nil {
				t.Fatalf("%s was created for a poll the registration never covered", path)
			}
		}
	})
}
