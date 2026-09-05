package control

import (
	"context"
	"testing"
)

func TestContinuityAuditFindsHistoricalEdgeAndOwnerDamage(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
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

	// A healthy store first: parked work has no aggregate status projection to
	// drift, so only edge and owner integrity are audited.
	healthy := newTask("healthy parked work")
	parkedRun(healthy, "asked the user", "waiting_user")
	findings, err := store.AuditTaskRunContinuity(ctx, identity.TenantID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("healthy store must audit clean, got %+v", findings)
	}

	// legacy_resume_unfilled: a pre-v7 reverse edge with no forward edge. The
	// Thread itself carries no execution state, so this remains a pure edge
	// finding without any projection setup.
	legacy := newTask("legacy resume edge")
	legacyParent := parkedRun(legacy, "old interrupted work", "interrupted")
	legacyChild := parkedRun(legacy, "old continuation", "done")
	if _, err := store.db.ExecContext(ctx,
		`UPDATE runs SET resumed_by_run_id = ? WHERE id = ?`,
		legacyChild.ID, legacyParent.ID); err != nil {
		t.Fatal(err)
	}
	// illegal_parent_edge: a forward edge pointing at a missing run.
	orphan := newTask("orphan parent edge")
	orphanChild := parkedRun(orphan, "child of nothing", "done")
	if _, err := store.db.ExecContext(ctx,
		`UPDATE runs SET resumes_run_id = 'run_missing' WHERE id = ?`,
		orphanChild.ID); err != nil {
		t.Fatal(err)
	}

	// ownerless_approval: pending human input whose task row is gone.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO approval_requests
		(id, tenant_id, person_id, thread_id, run_id, action_type, status, created_at, updated_at)
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
	if got := byKind["legacy_resume_unfilled"]; len(got) != 1 || got[0].RunID != legacyParent.ID {
		t.Fatalf("legacy_resume_unfilled findings = %+v", got)
	}
	if got := byKind["illegal_parent_edge"]; len(got) != 1 || got[0].RunID != orphanChild.ID {
		t.Fatalf("illegal_parent_edge findings = %+v", got)
	}
	if got := byKind["ownerless_approval"]; len(got) != 1 {
		t.Fatalf("ownerless_approval findings = %+v", got)
	}

}
