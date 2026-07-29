package tools

import (
	"strings"
	"testing"

	"selfmind/internal/executionenv"
)

// Reading os.Environ() at the execution callsite is what let a long-lived
// daemon hand children a stale PATH and change account mid-run. Every command
// of a run must resolve the environment its lease was bound to.
func TestLeaseProcessEnvResolvesThroughTheLease(t *testing.T) {
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })

	runDir, laterDir := t.TempDir(), t.TempDir()
	runEnv := registry.Install([]string{"PATH=" + runDir, "HOME=/home/u"}, "inherited", "p", nil)
	registry.BindLease("lease-run-1", runEnv.ID)
	// The host environment then moves on: a new generation exists, but the
	// bound run must not see it.
	registry.Install([]string{"PATH=" + laterDir, "HOME=/home/u"}, "login-shell", "p", nil)

	tenant := "tenant-lease-test"
	cleanup := SetExecutionScope(tenant, ExecutionScope{
		TenantID:      tenant,
		PersonID:      "p",
		WorkspaceRoot: t.TempDir(),
		LeaseID:       "lease-run-1",
	})
	t.Cleanup(cleanup)

	env := leaseProcessEnv(map[string]interface{}{"_tenant_id": tenant})
	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "PATH="+runDir) {
		t.Fatalf("expected the lease's own environment, got %q", joined)
	}
	if strings.Contains(joined, laterDir) {
		t.Fatalf("a later generation leaked into a bound run: %q", joined)
	}
}

// A scope with no lease binding still resolves the CURRENT snapshot rather than
// re-reading the process environment, so the filter can never be bypassed.
func TestLeaseProcessEnvFallsBackToCurrentSnapshot(t *testing.T) {
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })

	registry.Install([]string{"PATH=/snapshot/current"}, "inherited", "p", nil)
	env := leaseProcessEnv(nil)
	if !strings.Contains(strings.Join(env, " "), "/snapshot/current") {
		t.Fatalf("expected the current snapshot, got %v", env)
	}
}

// InstallEnvironmentSnapshot is the only snapshot constructor, so the
// control-plane strip must apply to everything a child can ever see.
func TestInstallEnvironmentSnapshotAppliesTheProcessEnvPolicy(t *testing.T) {
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })

	snapshot := InstallEnvironmentSnapshot([]string{
		"PATH=/usr/bin",
		"SELF_GATEWAY_TOKEN=daemon-secret",
		"SELFMIND_TENANT_ID=default",
		"GCLOUD_PROJECT=proj",
	}, "inherited")
	joined := strings.Join(snapshot.Env(), " ")
	for _, stripped := range []string{"SELF_GATEWAY_TOKEN", "SELFMIND_TENANT_ID", "daemon-secret"} {
		if strings.Contains(joined, stripped) {
			t.Fatalf("control-plane state reached a child environment: %q", joined)
		}
	}
	if !strings.Contains(joined, "GCLOUD_PROJECT=proj") {
		t.Fatalf("operator toolchain state must survive: %q", joined)
	}
}
