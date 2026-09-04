package httpapi

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/tools"
)

// A trusted workspace whose operator policy allows egress freezes network:shared
// as trust-derived, and its polls must actually run. The live failure was the
// opposite: the binding froze an EMPTY capability set, so every poll refused
// with "does not include network:shared" and four Cloud Build results were left
// unverified. The same watcher must still stop the moment either half of that
// trust decision is withdrawn.
func TestExternalWatchTrustDerivedNetworkCapabilityPollsUntilTrustOrPolicyChanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SelfMind's production daemon runs on Linux")
	}
	server, store, identity, _, _ := newClarifyTestServer(t)
	ctx := context.Background()

	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	tools.InstallEnvironmentSnapshot(os.Environ(), "inherited")

	policy := tools.CurrentExecSandboxPolicy()
	enabled, required := policy.Enabled, policy.Required
	tools.SetExecSandbox(enabled, required, true)
	t.Cleanup(func() { tools.SetExecSandbox(policy.Enabled, policy.Required, policy.AllowNetwork) })

	workspace, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID,
		Name: "workspace", LocalPath: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetWorkspaceTrust(ctx, identity.TenantID, identity.PersonID,
		workspace.ID, executionenv.TrustTrusted, "local_cli"); err != nil {
		t.Fatal(err)
	}
	binding := executionenv.Binding{
		Version:               executionenv.BindingVersion,
		TenantID:              identity.TenantID,
		PersonID:              identity.PersonID,
		WorkspaceID:           workspace.ID,
		TrustLevel:            executionenv.TrustTrusted,
		ExecutionCapabilities: []string{executionenv.CapabilityNetworkShared},
		CapabilityBindings: []executionenv.CapabilityBinding{{
			Capability: executionenv.CapabilityNetworkShared,
			Source:     executionenv.CapabilitySourceTrust,
		}},
	}
	watch := control.ExternalWatch{
		ID: "watch_trusted", TenantID: identity.TenantID, PersonID: identity.PersonID,
		WorkspaceID: workspace.ID, CWD: t.TempDir(), Command: "printf PENDING",
		ExecutionBinding: binding,
	}
	got, err := server.validateExternalWatchCapabilities(ctx, watch, binding)
	if err != nil || len(got) != 1 || got[0] != executionenv.CapabilityNetworkShared {
		t.Fatalf("trust-derived capability was not accepted: %v, %v", got, err)
	}
	result, err := server.runExternalWatchCommand(ctx, watch)
	if err != nil {
		t.Fatalf("trusted watcher poll refused: %v (%s)", err, result.Output)
	}
	if result.Output != "PENDING" {
		t.Fatalf("poll output = %q, want PENDING", result.Output)
	}

	// The operator disables sandbox egress: a trust-derived network capability
	// must stop the watch instead of running without the capability it froze.
	tools.SetExecSandbox(enabled, required, false)
	if _, err := server.runExternalWatchCommand(ctx, watch); err == nil ||
		!strings.Contains(err.Error(), "sandbox policy") {
		t.Fatalf("poll after egress was disabled = %v, want a sandbox policy refusal", err)
	}
	tools.SetExecSandbox(enabled, required, true)

	// Workspace trust is withdrawn: the capability came from that decision, so
	// it disappears with it.
	if _, err := store.SetWorkspaceTrust(ctx, identity.TenantID, identity.PersonID,
		workspace.ID, executionenv.TrustUntrusted, "local_cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.runExternalWatchCommand(ctx, watch); err == nil ||
		!strings.Contains(err.Error(), "trust") {
		t.Fatalf("poll after trust revocation = %v, want a trust refusal", err)
	}
}

func TestExternalWatchFrozenGrantCanBeRevokedButNotExpanded(t *testing.T) {
	server, store, identity, _, _ := newClarifyTestServer(t)
	ctx := context.Background()
	workspace, err := store.RegisterWorkspace(ctx, control.Workspace{
		TenantID: identity.TenantID, OwnerPersonID: identity.PersonID,
		Name: "workspace", LocalPath: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = store.SetWorkspaceTrust(ctx, identity.TenantID, identity.PersonID,
		workspace.ID, executionenv.TrustUntrusted, "local_cli")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.GrantExecutionCapability(ctx, identity.TenantID, identity.PersonID,
		workspace.ID, executionenv.CapabilityNetworkShared, "network-resource", "human:cli",
		time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	grants, err := store.ListActiveExecutionCapabilities(ctx, identity.TenantID, identity.PersonID, workspace.ID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("active grants = %#v, err=%v", grants, err)
	}
	binding := executionenv.Binding{
		Version:               executionenv.BindingVersion,
		TenantID:              identity.TenantID,
		PersonID:              identity.PersonID,
		WorkspaceID:           workspace.ID,
		TrustLevel:            executionenv.TrustUntrusted,
		ExecutionCapabilities: []string{executionenv.CapabilityNetworkShared},
		CapabilityBindings: []executionenv.CapabilityBinding{{
			Capability: executionenv.CapabilityNetworkShared,
			Source:     executionenv.CapabilitySourceGrant,
			GrantID:    grants[0].ID,
		}},
	}
	watch := control.ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		WorkspaceID: workspace.ID, ExecutionBinding: binding,
	}
	got, err := server.validateExternalWatchCapabilities(ctx, watch, binding)
	if err != nil || len(got) != 1 || got[0] != executionenv.CapabilityNetworkShared {
		t.Fatalf("frozen grant was not accepted: %v, %v", got, err)
	}

	// A later unrelated grant must not expand this binding.
	if err := store.GrantExecutionCapability(ctx, identity.TenantID, identity.PersonID,
		workspace.ID, executionenv.CapabilityCredentialRead, "credential-resource", "human:cli",
		time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err = server.validateExternalWatchCapabilities(ctx, watch, binding)
	if err != nil || len(got) != 1 || got[0] != executionenv.CapabilityNetworkShared {
		t.Fatalf("later grant changed frozen capabilities: %v, %v", got, err)
	}

	if err := store.RevokeExecutionCapability(ctx, identity.TenantID, identity.PersonID,
		workspace.ID, executionenv.CapabilityNetworkShared); err != nil {
		t.Fatal(err)
	}
	if _, err := server.validateExternalWatchCapabilities(ctx, watch, binding); err == nil {
		t.Fatal("revoked capability must stop the watcher before its next command")
	}
}

// A durable watch outlives its run and survives restarts, so it is the execution
// path most likely to straddle an environment change — and the failure mode is
// not an error but a check that SUCCEEDS against the wrong account. The identity
// recorded at registration must therefore be verified before every check.
func TestExternalWatchDetectsEnvironmentChange(t *testing.T) {
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })

	accountA := registry.Install([]string{"PATH=/usr/bin", "HOME=/home/u", "CLOUDSDK_CONFIG=/home/u/.config/gcloud"},
		"inherited", "principal-account-A", []string{"environment:GCLOUD_TOKEN"})

	watch := control.ExternalWatch{
		ID:                     "watch_1",
		PrincipalFingerprint:   accountA.PrincipalFingerprint,
		EnvironmentFingerprint: accountA.EnvironmentFingerprint,
		CredentialSourceHash:   accountA.CredentialSourceHash,
	}

	// Same environment: the watch proceeds.
	if changed := externalWatchEnvironmentChange(watch); len(changed) != 0 {
		t.Fatalf("an unchanged environment must not stop a watch: %v", changed)
	}

	// The operator switches account. The daemon restarts and installs a new
	// snapshot; the watch must NOT quietly start reporting on account B.
	registry.Install([]string{"PATH=/usr/bin", "HOME=/home/u", "CLOUDSDK_CONFIG=/home/u/.config/gcloud"},
		"inherited", "principal-account-B", []string{"environment:GCLOUD_TOKEN"})
	changed := externalWatchEnvironmentChange(watch)
	if len(changed) == 0 {
		t.Fatal("an account change must stop the watch instead of checking as someone else")
	}
	if strings.Join(changed, ",") != "account/profile" {
		t.Fatalf("unexpected change description: %v", changed)
	}

	// A credential source moving is also a change: the same account read from a
	// different config location is a different execution identity.
	registry.Install([]string{"PATH=/usr/bin", "HOME=/home/u", "CLOUDSDK_CONFIG=/home/u/.config/gcloud-alt"},
		"inherited", "principal-account-A", []string{"environment:GCLOUD_TOKEN"})
	changed = externalWatchEnvironmentChange(watch)
	if len(changed) == 0 || !strings.Contains(strings.Join(changed, ","), "credential source") {
		t.Fatalf("a credential source change must be detected: %v", changed)
	}
}

// Watches registered before identity existed carry no fingerprints. Parking them
// would strand work the operator already started for a property that was never
// recorded, so they are grandfathered.
func TestExternalWatchWithoutRecordedIdentityIsGrandfathered(t *testing.T) {
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	registry.Install([]string{"PATH=/usr/bin"}, "inherited", "principal-new", nil)

	if changed := externalWatchEnvironmentChange(control.ExternalWatch{ID: "watch_legacy"}); len(changed) != 0 {
		t.Fatalf("a legacy watch must not be parked for an unrecorded property: %v", changed)
	}
}

// The recorded identity must be non-secret: fingerprints and a snapshot id only.
func TestExternalWatchIdentityCarriesNoSecrets(t *testing.T) {
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })

	snapshot := tools.InstallEnvironmentSnapshot([]string{
		"PATH=/usr/bin",
		"GCLOUD_TOKEN=super-secret-value",
		"CLOUDSDK_CONFIG=/home/u/.config/gcloud",
	}, "inherited")

	watch := control.ExternalWatch{
		EnvironmentSnapshotID:  snapshot.ID,
		EnvironmentGeneration:  snapshot.Generation,
		PrincipalFingerprint:   snapshot.PrincipalFingerprint,
		EnvironmentFingerprint: snapshot.EnvironmentFingerprint,
		CredentialSourceHash:   snapshot.CredentialSourceHash,
	}
	rendered := strings.Join([]string{
		watch.EnvironmentSnapshotID, watch.PrincipalFingerprint,
		watch.EnvironmentFingerprint, watch.CredentialSourceHash,
	}, " ")
	for _, forbidden := range []string{"super-secret-value", "GCLOUD_TOKEN"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("watch identity leaked %q: %s", forbidden, rendered)
		}
	}
	if watch.EnvironmentFingerprint == "" || watch.CredentialSourceHash == "" {
		t.Fatal("identity must actually be populated, or the check is a no-op")
	}
}
