package control

import (
	"context"
	"testing"
	"time"
)

func newApprovalGrantTestStore(t *testing.T) *Store {
	t.Helper()
	store := newTestStore(t)
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
	granted, err := store.IsApprovalGranted(ctx, tenant, person, key)
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
	if granted, err = store.IsApprovalGranted(ctx, tenant, person, key); err != nil {
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
	if granted, err = store.IsApprovalGranted(ctx, tenant, person, key); err != nil {
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
	if err := store.GrantApproval(ctx, "person", DefaultTenantID, "p", "p", key, time.Time{}); err != nil {
		t.Fatal(err)
	}
	granted, err := store.IsApprovalGranted(ctx, DefaultTenantID, "p", key)
	if err != nil {
		t.Fatal(err)
	}
	if !granted {
		t.Fatal("a grant without a deadline must remain active")
	}
}

// TestHistoricalTaskScopedGrantAuthorizesNothing replaces the compatibility
// rule that kept task-scoped ledger rows readable. Task-scoped reuse rested on
// the judgment that a set of runs is one piece of work; that judgment
// mis-groups unrelated runs, so honouring such a row would let one decision
// authorize work the person never saw. Rows are left in place for audit and
// simply stop authorizing.
func TestHistoricalTaskScopedGrantAuthorizesNothing(t *testing.T) {
	store := newApprovalGrantTestStore(t)
	ctx := context.Background()
	const key = "exec:requests execution on the host outside the isolated sandbox"
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO approval_grants (id, tenant_id, person_id, scope_kind, scope_id, pattern_key, created_at, expires_at, revoked_at)
		 VALUES ('agr_legacy', ?, 'p', 'task', 'task-1', ?, 1, 0, 0)`, DefaultTenantID, key); err != nil {
		t.Fatal(err)
	}
	granted, err := store.IsApprovalGranted(ctx, DefaultTenantID, "p", key)
	if err != nil {
		t.Fatal(err)
	}
	if granted {
		t.Fatal("a historical task-scoped row must not authorize anything")
	}
	grants, err := store.ListApprovalGrants(ctx, DefaultTenantID, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].ScopeKind != "task" {
		t.Fatalf("the row must stay listable for audit: %+v", grants)
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
