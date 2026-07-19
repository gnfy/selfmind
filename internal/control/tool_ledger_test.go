package control

import (
	"context"
	"testing"
	"time"
)

// TestToolLedgerUncertainWindow pins the safety property: a started entry
// with no recorded outcome is uncertain; recording the outcome closes it;
// read-only entries are excluded from the verify set; the per-task query spans
// runs (a resume uses a fresh run id).
func TestToolLedgerUncertainWindow(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)

	// A side-effect tool dispatched but never resolved = the crash window.
	if err := store.RecordToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "call-1", ToolName: "terminal",
		ArgsHash: "h1", RetryClass: "side_effect",
	}); err != nil {
		t.Fatal(err)
	}
	// A read-only tool, also unresolved: must NOT appear (blind re-run is safe).
	if err := store.RecordToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "call-2", ToolName: "read_file",
		ArgsHash: "h2", RetryClass: "read_only",
	}); err != nil {
		t.Fatal(err)
	}
	// A side-effect tool that DID resolve: closed, not uncertain.
	if err := store.RecordToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "call-3", ToolName: "verify",
		ArgsHash: "h3", RetryClass: "side_effect",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordToolOutcome(ctx, identity.TenantID, run.ID, "call-3", true); err != nil {
		t.Fatal(err)
	}

	uncertain, err := store.ListUncertainToolEntriesForTask(ctx, identity.TenantID, task.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(uncertain) != 1 || uncertain[0].ToolCallID != "call-1" {
		t.Fatalf("uncertain = %+v, want only call-1", uncertain)
	}

	// Closing the window removes it from the verify set.
	if err := store.RecordToolOutcome(ctx, identity.TenantID, run.ID, "call-1", true); err != nil {
		t.Fatal(err)
	}
	if u, _ := store.ListUncertainToolEntriesForTask(ctx, identity.TenantID, task.ID, 10); len(u) != 0 {
		t.Fatalf("resolved entry still uncertain: %+v", u)
	}

	// Prune drops resolved rows but never a started (uncertain) one.
	if err := store.RecordToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "call-4", ToolName: "terminal", ArgsHash: "h4", RetryClass: "side_effect",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tool_ledger SET updated_at = ? WHERE status IN ('completed','failed')`,
		time.Now().Add(-30*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	pruned, err := store.PruneToolLedger(ctx, 0)
	if err != nil || pruned == 0 {
		t.Fatalf("prune = %d err=%v", pruned, err)
	}
	var uncertainRows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_ledger WHERE status IN ('started','dispatched')`).Scan(&uncertainRows); err != nil {
		t.Fatal(err)
	}
	// Two started rows remain (call-2 read_only + call-4 side_effect); prune
	// touched neither — uncertain rows are correctness state.
	if uncertainRows != 2 {
		t.Fatalf("uncertain rows after prune = %d, want 2 (never pruned)", uncertainRows)
	}
}

// A re-dispatch of the same call never duplicates or reopens the execution.
func TestToolLedgerRedispatchIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	for i := 0; i < 2; i++ {
		if err := store.RecordToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
			RunID: run.ID, ToolCallID: "call-x", ToolName: "terminal", ArgsHash: "h", RetryClass: "side_effect",
		}); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_ledger WHERE run_id = ? AND tool_call_id = 'call-x'`, run.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("re-dispatch created %d rows, want 1", rows)
	}
	if err := store.RecordToolOutcome(ctx, identity.TenantID, run.ID, "call-x", true); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordToolDispatch(ctx, identity.TenantID, ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "call-x", ToolName: "terminal", ArgsHash: "h", RetryClass: "side_effect",
	}); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM tool_ledger WHERE run_id = ? AND tool_call_id = 'call-x'`, run.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != ToolLedgerCompleted {
		t.Fatalf("terminal ledger state regressed to %q", status)
	}
}

func TestToolLedgerClaimIsSingleUseAndRejectsArgumentDrift(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	entry := ToolLedgerEntry{
		RunID: run.ID, ToolCallID: "call-once", ToolName: "terminal", ArgsHash: "hash-a", RetryClass: "side_effect",
	}
	first, err := store.ClaimToolDispatch(ctx, identity.TenantID, entry)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Execute || first.Status != ToolLedgerStarted {
		t.Fatalf("first claim = %+v, want one executable started claim", first)
	}
	second, err := store.ClaimToolDispatch(ctx, identity.TenantID, entry)
	if err != nil {
		t.Fatal(err)
	}
	if second.Execute || second.Status != ToolLedgerStarted {
		t.Fatalf("second claim = %+v, want non-executable started claim", second)
	}
	drifted := entry
	drifted.ArgsHash = "hash-b"
	if _, err := store.ClaimToolDispatch(ctx, identity.TenantID, drifted); err == nil {
		t.Fatal("argument drift for an existing tool call id must be rejected")
	}
}
