package control

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"selfmind/internal/executionenv"
)

func TestLegacyWorkspaceMigrationRequiresLocalTrustReview(t *testing.T) {
	dataDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE workspaces (
		id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		owner_person_id TEXT NOT NULL,
		name TEXT NOT NULL,
		repo_url TEXT,
		local_path TEXT NOT NULL,
		default_branch TEXT,
		allowed_roots_json TEXT,
		status TEXT NOT NULL DEFAULT 'active',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		UNIQUE(tenant_id, owner_person_id, local_path)
	);
	INSERT INTO workspaces (
		id, tenant_id, owner_person_id, name, local_path, allowed_roots_json,
		status, created_at, updated_at
	) VALUES (
		'workspace-legacy', 'tenant-legacy', 'person-legacy', 'legacy',
		'/work/legacy', '[]', 'active', 1, 1
	)`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	workspace, err := store.GetWorkspace(context.Background(), "tenant-legacy", "workspace-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if workspace == nil {
		t.Fatal("legacy workspace was lost during migration")
	}
	if workspace.TrustLevel != executionenv.TrustUntrusted {
		t.Fatalf("legacy workspace trust = %q, want untrusted", workspace.TrustLevel)
	}
	if workspace.TrustSource != "migration_review_required" || workspace.TrustedAt != nil {
		t.Fatalf("legacy workspace migration metadata = %#v", workspace)
	}
}

func TestWorkspaceTrustControlsExecutionCapabilities(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	workspace, err := store.EnsureWorkspace(ctx, Workspace{
		TenantID:      "tenant-trust",
		OwnerPersonID: "person-trust",
		Name:          "project",
		LocalPath:     "/work/project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.TrustLevel != executionenv.TrustUntrusted {
		t.Fatalf("new remotely observed workspace trust = %q, want untrusted", workspace.TrustLevel)
	}

	workspace, err = store.SetWorkspaceTrust(
		ctx,
		workspace.TenantID,
		workspace.OwnerPersonID,
		workspace.ID,
		executionenv.TrustTrusted,
		"local_cli",
	)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.TrustLevel != executionenv.TrustTrusted || workspace.TrustSource != "local_cli" {
		t.Fatalf("trusted workspace = %#v", workspace)
	}

	if err := store.GrantExecutionCapability(
		ctx,
		workspace.TenantID,
		workspace.OwnerPersonID,
		workspace.ID,
		executionenv.CapabilityNetworkShared,
		"network:shared",
		"person",
		time.Now().Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	granted, err := store.HasExecutionCapability(
		ctx,
		workspace.TenantID,
		workspace.OwnerPersonID,
		workspace.ID,
		executionenv.CapabilityNetworkShared,
		"network:shared",
	)
	if err != nil || !granted {
		t.Fatalf("granted=%v err=%v", granted, err)
	}

	if _, err := store.SetWorkspaceTrust(
		ctx,
		workspace.TenantID,
		workspace.OwnerPersonID,
		workspace.ID,
		executionenv.TrustUntrusted,
		"local_cli",
	); err != nil {
		t.Fatal(err)
	}
	granted, err = store.HasExecutionCapability(
		ctx,
		workspace.TenantID,
		workspace.OwnerPersonID,
		workspace.ID,
		executionenv.CapabilityNetworkShared,
		"network:shared",
	)
	if err != nil {
		t.Fatal(err)
	}
	if granted {
		t.Fatal("untrusting a workspace must revoke its active capabilities")
	}
}

func TestExecutionLeaseIsImmutablePerRun(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	first, err := store.MaterializeExecutionLease(ctx, executionenv.Lease{
		RunID:              "run-lease",
		TenantID:           "tenant-lease",
		PersonID:           "person-lease",
		WorkspaceID:        "workspace-lease",
		EnvironmentProfile: "operator",
		CredentialRefs: []executionenv.CredentialRef{
			{Kind: "profile", Source: "AWS_PROFILE", Principal: "profile-a"},
		},
		PrincipalFingerprint: "principal-a",
		ExecutionCapabilities: []string{
			executionenv.CapabilityNetworkShared,
			executionenv.CapabilityNetworkShared,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.MaterializeExecutionLease(ctx, executionenv.Lease{
		RunID:                "run-lease",
		TenantID:             "tenant-lease",
		PersonID:             "person-lease",
		WorkspaceID:          "workspace-lease",
		EnvironmentProfile:   "different",
		PrincipalFingerprint: "principal-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("replayed run created a new lease: %q != %q", second.ID, first.ID)
	}
	if second.PrincipalFingerprint != "principal-a" || second.EnvironmentProfile != "operator" {
		t.Fatalf("replayed run mutated immutable lease: %#v", second)
	}
	if len(second.ExecutionCapabilities) != 1 {
		t.Fatalf("capabilities were not normalized: %#v", second.ExecutionCapabilities)
	}
	if len(second.CredentialRefs) != 1 || second.CredentialRefs[0].Source != "AWS_PROFILE" {
		t.Fatalf("credential references were not preserved: %#v", second.CredentialRefs)
	}
}

func TestExpiredExecutionCapabilityIsInactive(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().Unix()
	_, err = store.db.ExecContext(ctx, `INSERT INTO execution_capability_grants
		(id, tenant_id, person_id, workspace_id, capability, resource_fingerprint,
		 granted_by, expires_at, revoked_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)`,
		"cap-expired", "tenant-cap", "person-cap", "workspace-cap",
		executionenv.CapabilityNetworkShared, "network:shared", "person",
		now-1, now-10, now-10)
	if err != nil {
		t.Fatal(err)
	}
	granted, err := store.HasExecutionCapability(
		ctx,
		"tenant-cap",
		"person-cap",
		"workspace-cap",
		executionenv.CapabilityNetworkShared,
		"network:shared",
	)
	if err != nil {
		t.Fatal(err)
	}
	if granted {
		t.Fatal("expired capability must not be active")
	}
	active, err := store.ListActiveExecutionCapabilities(ctx, "tenant-cap", "person-cap", "workspace-cap")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("expired capability returned by active listing: %#v", active)
	}
}
