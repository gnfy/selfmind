package control

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestApprovalNotifiedRoundTrip: an un-notified pending approval appears in the
// escrow scan; MarkApprovalNotified stamps it (idempotently) and it drops out.
func TestApprovalNotifiedRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, ActionType: "tool_call",
	})
	if err != nil {
		t.Fatal(err)
	}

	// created_at <= now+1m includes the fresh row.
	pending, err := store.ListPendingApprovalsForEscrow(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != approval.ID {
		t.Fatalf("un-notified approval must be escrow-eligible, got %+v", pending)
	}

	// A cutoff before the row excludes it (threshold not yet reached).
	early, err := store.ListPendingApprovalsForEscrow(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(early) != 0 {
		t.Fatalf("a row newer than the cutoff must be excluded, got %+v", early)
	}

	if err := store.MarkApprovalNotified(ctx, identity.TenantID, approval.ID); err != nil {
		t.Fatal(err)
	}
	after, err := store.ListPendingApprovalsForEscrow(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("a notified approval must drop out of escrow, got %+v", after)
	}

	// Idempotent: marking again is a harmless no-op.
	if err := store.MarkApprovalNotified(ctx, identity.TenantID, approval.ID); err != nil {
		t.Fatalf("second mark must not error: %v", err)
	}
}

// TestClarifyNotifiedRoundTrip mirrors the approval round-trip for clarifies.
func TestClarifyNotifiedRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	clarify, err := store.CreateClarifyRequest(ctx, ClarifyRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Question: "which port?",
	})
	if err != nil {
		t.Fatal(err)
	}

	pending, err := store.ListPendingClarifiesForEscrow(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != clarify.ID {
		t.Fatalf("un-notified clarify must be escrow-eligible, got %+v", pending)
	}

	if err := store.MarkClarifyNotified(ctx, identity.TenantID, clarify.ID); err != nil {
		t.Fatal(err)
	}
	after, err := store.ListPendingClarifiesForEscrow(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("a notified clarify must drop out of escrow, got %+v", after)
	}
}

// TestNotifiedAtMigrationOnLegacyDB: a control.db whose approval_requests table
// predates notified_at (created without the column, with a pending row) must
// gain the column on OpenStore and expose the legacy row to the escrow scan
// (its notified_at defaults NULL).
func TestNotifiedAtMigrationOnLegacyDB(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "control.db")

	// Hand-build a legacy approval_requests table (no notified_at column) and a
	// pending row, exactly as an older daemon would have left it.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	_, err = raw.ExecContext(ctx, `CREATE TABLE approval_requests (
		id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		person_id TEXT NOT NULL,
		task_id TEXT,
		run_id TEXT,
		action_type TEXT NOT NULL,
		payload_json TEXT,
		status TEXT NOT NULL,
		requested_channel TEXT,
		approved_channel TEXT,
		decision_scope TEXT,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);`)
	if err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	old := time.Now().Add(-time.Hour).Unix()
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO approval_requests (id, tenant_id, person_id, action_type, payload_json, status, created_at, updated_at)
		 VALUES ('apr_legacy', 'default', 'person_legacy', 'tool_call', '{}', 'pending', ?, ?)`, old, old); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	// OpenStore must migrate the table (add notified_at) without error.
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore on legacy db: %v", err)
	}
	defer store.Close()

	pending, err := store.ListPendingApprovalsForEscrow(ctx, time.Now())
	if err != nil {
		t.Fatalf("escrow scan on migrated db: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "apr_legacy" {
		t.Fatalf("legacy pending approval must be escrow-eligible after migration, got %+v", pending)
	}
	if err := store.MarkApprovalNotified(ctx, "default", "apr_legacy"); err != nil {
		t.Fatalf("mark notified on migrated row: %v", err)
	}
}
