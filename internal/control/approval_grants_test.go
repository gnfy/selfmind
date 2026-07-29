package control

import (
	"context"
	"testing"
	"time"
)

func newApprovalGrantTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestApprovalGrantExpiryAndRevocation(t *testing.T) {
	store := newApprovalGrantTestStore(t)
	ctx := context.Background()
	const (
		tenant = DefaultTenantID
		person = "person_1"
		key    = "exec:requests execution on the host outside the isolated sandbox|resource=workspace:abc:command:gcloud"
	)

	// An expired grant must not authorize its class. Without a deadline column
	// a remembered class was permanent, so one defective key stayed
	// authoritative forever.
	if err := store.GrantApproval(ctx, "person", tenant, person, person, key, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	granted, err := store.IsApprovalGranted(ctx, tenant, person, "", key)
	if err != nil {
		t.Fatal(err)
	}
	if granted {
		t.Fatal("an expired grant must not authorize its class")
	}
	if active, err := store.ListApprovalGrants(ctx, tenant, person, false); err != nil {
		t.Fatal(err)
	} else if len(active) != 0 {
		t.Fatalf("expired grant must not be listed as active: %+v", active)
	}
	if all, err := store.ListApprovalGrants(ctx, tenant, person, true); err != nil {
		t.Fatal(err)
	} else if len(all) != 1 {
		t.Fatalf("history must remain visible, got %d rows", len(all))
	}

	// Re-granting is a fresh human decision: it refreshes the deadline.
	if err := store.GrantApproval(ctx, "person", tenant, person, person, key, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if granted, err = store.IsApprovalGranted(ctx, tenant, person, "", key); err != nil {
		t.Fatal(err)
	} else if !granted {
		t.Fatal("a refreshed grant must authorize its class")
	}

	active, err := store.ListApprovalGrants(ctx, tenant, person, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active grant, got %d", len(active))
	}
	if !active[0].Active(time.Now()) {
		t.Fatal("listed grant should report itself active")
	}

	withdrawn, err := store.RevokeApprovalGrant(ctx, tenant, person, active[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !withdrawn {
		t.Fatal("revoke should report a change")
	}
	if granted, err = store.IsApprovalGranted(ctx, tenant, person, "", key); err != nil {
		t.Fatal(err)
	} else if granted {
		t.Fatal("a revoked grant must not authorize its class")
	}
	// Revocation is recorded, not deleted, so the decision stays auditable.
	all, err := store.ListApprovalGrants(ctx, tenant, person, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || !all[0].Revoked() {
		t.Fatalf("revocation must be durable and visible: %+v", all)
	}
	if again, err := store.RevokeApprovalGrant(ctx, tenant, person, active[0].ID); err != nil {
		t.Fatal(err)
	} else if again {
		t.Fatal("revoking twice must be idempotent")
	}
}

// A grant written before expires_at existed keeps the legacy "no deadline"
// meaning rather than being treated as already expired.
func TestApprovalGrantWithoutDeadlineStaysActive(t *testing.T) {
	store := newApprovalGrantTestStore(t)
	ctx := context.Background()
	const key = "exec:invokes dangerous command: chmod"
	if err := store.GrantApproval(ctx, "task", DefaultTenantID, "p", "task-1", key, time.Time{}); err != nil {
		t.Fatal(err)
	}
	granted, err := store.IsApprovalGranted(ctx, DefaultTenantID, "p", "task-1", key)
	if err != nil {
		t.Fatal(err)
	}
	if !granted {
		t.Fatal("a grant without a deadline must remain active")
	}
}

func TestListAllApprovalGrantsSpansPersons(t *testing.T) {
	store := newApprovalGrantTestStore(t)
	ctx := context.Background()
	if err := store.GrantApproval(ctx, "person", DefaultTenantID, "p1", "p1", "exec:a|resource=workspace:x:command:gcloud", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := store.GrantApproval(ctx, "person", DefaultTenantID, "p2", "p2", "exec:a|resource=workspace:x:command:set", time.Time{}); err != nil {
		t.Fatal(err)
	}
	all, err := store.ListAllApprovalGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("boot review must see every person's grants, got %d", len(all))
	}
}
