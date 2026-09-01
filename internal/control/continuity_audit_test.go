package control

import (
	"context"
	"testing"
)

func TestContinuityAuditFindsAndClassifiesHistoricalDamage(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "audit-user", "Audit User")
	if err != nil {
		t.Fatal(err)
	}
	newTask := func(title string) *Task {
		t.Helper()
		task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title, Channel: "cli"})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	parkedRun := func(task *Task, input, status string) *Run {
		t.Helper()
		run, err := store.StartRun(ctx, task, "cli", input)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
			Identity: *identity, RunID: run.ID, RunStatus: status,
			TaskID: task.ID, TaskStatus: status, Summary: input,
		}); err != nil {
			t.Fatal(err)
		}
		return run
	}

	// A healthy store first: parked work whose projection matches derivation.
	healthy := newTask("healthy parked work")
	parkedRun(healthy, "asked the user", "waiting_user")
	findings, err := store.AuditTaskRunContinuity(ctx, identity.TenantID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("healthy store must audit clean, got %+v", findings)
	}

	// projection_mismatch: the run is parked but someone cached "done".
	drifted := newTask("drifted projection")
	parkedRun(drifted, "waiting on operator", "waiting_user")
	if _, err := store.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'done' WHERE tenant_id = ? AND id = ?`,
		identity.TenantID, drifted.ID); err != nil {
		t.Fatal(err)
	}

	// stale_wait_projection: a wait label with zero live evidence — the parked
	// run was claimed and its child completed, but the cached label survived.
	stale := newTask("stale wait label")
	staleRun := parkedRun(stale, "was interrupted", "interrupted")
	staleChild, err := store.StartRunWithOptions(ctx, stale, "cli", "claimed continuation", StartRunOptions{ParentRunID: staleRun.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, staleChild.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'waiting_user' WHERE tenant_id = ? AND id = ?`,
		identity.TenantID, stale.ID); err != nil {
		t.Fatal(err)
	}

	// legacy_resume_unfilled: a pre-v7 reverse edge with no forward edge. The
	// legacy daemon also stored a terminal label when it marked the resume, so
	// pin "done" to keep this fixture a pure edge finding.
	legacy := newTask("legacy resume edge")
	legacyParent := parkedRun(legacy, "old interrupted work", "interrupted")
	legacyChild := parkedRun(legacy, "old continuation", "done")
	if _, err := store.db.ExecContext(ctx,
		`UPDATE task_runs SET resumed_by_run_id = ? WHERE id = ?`,
		legacyChild.ID, legacyParent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE tasks SET status = 'done' WHERE tenant_id = ? AND id = ?`,
		identity.TenantID, legacy.ID); err != nil {
		t.Fatal(err)
	}

	// illegal_parent_edge: a forward edge pointing at a missing run.
	orphan := newTask("orphan parent edge")
	orphanChild := parkedRun(orphan, "child of nothing", "done")
	if _, err := store.db.ExecContext(ctx,
		`UPDATE task_runs SET parent_run_id = 'run_missing' WHERE id = ?`,
		orphanChild.ID); err != nil {
		t.Fatal(err)
	}

	// ownerless_approval: pending human input whose task row is gone.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO approval_requests
		(id, tenant_id, person_id, task_id, run_id, action_type, status, created_at, updated_at)
		VALUES ('approval_orphan', ?, ?, 'task_missing', '', 'terminal', 'pending', 1, 1)`,
		identity.TenantID, identity.PersonID); err != nil {
		t.Fatal(err)
	}

	findings, err = store.AuditTaskRunContinuity(ctx, identity.TenantID, 100)
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string][]ContinuityAuditFinding{}
	for _, finding := range findings {
		byKind[finding.Kind] = append(byKind[finding.Kind], finding)
	}
	if got := byKind["projection_mismatch"]; len(got) != 1 || got[0].TaskID != drifted.ID || !got[0].SafeFix {
		t.Fatalf("projection_mismatch findings = %+v", got)
	}
	if got := byKind["stale_wait_projection"]; len(got) != 1 || got[0].TaskID != stale.ID || got[0].SafeFix {
		t.Fatalf("stale_wait_projection findings = %+v", got)
	}
	if got := byKind["legacy_resume_unfilled"]; len(got) != 1 || got[0].RunID != legacyParent.ID {
		t.Fatalf("legacy_resume_unfilled findings = %+v", got)
	}
	if got := byKind["illegal_parent_edge"]; len(got) != 1 || got[0].RunID != orphanChild.ID {
		t.Fatalf("illegal_parent_edge findings = %+v", got)
	}
	if got := byKind["ownerless_approval"]; len(got) != 1 {
		t.Fatalf("ownerless_approval findings = %+v", got)
	}

	// The only repair: reconcile the drifted projection through the reducer.
	if err := store.ReconcileTaskProjection(ctx, identity.TenantID, drifted.ID); err != nil {
		t.Fatal(err)
	}
	repaired, err := store.GetTask(ctx, identity.TenantID, drifted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Status != "waiting_user" {
		t.Fatalf("reconciled status = %q, want waiting_user", repaired.Status)
	}
	findings, err = store.AuditTaskRunContinuity(ctx, identity.TenantID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.Kind == "projection_mismatch" {
			t.Fatalf("repair must clear the mismatch, still found %+v", finding)
		}
	}
}
