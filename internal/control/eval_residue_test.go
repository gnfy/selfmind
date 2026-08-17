package control

import (
	"context"
	"testing"
	"time"
)

// seedResidueWorld builds a store containing three kinds of persons:
//   - an eval-only person (platform `eval` account only) with a full task tree,
//   - a real person (platform `cli`) with its own task tree,
//   - a mixed person that has BOTH an eval and a cli account (must be kept).
//
// It returns the store plus the identities needed for assertions.
func seedResidueWorld(t *testing.T) (*Store, *IdentityContext, *IdentityContext, *IdentityContext, map[string]string) {
	t.Helper()
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	evalID, err := store.ResolveOrCreateAccount(ctx, "eval-tenant-123", "eval", "eval-case1", "SelfMind Eval")
	if err != nil {
		t.Fatalf("create eval identity: %v", err)
	}
	realID, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Real User")
	if err != nil {
		t.Fatalf("create real identity: %v", err)
	}
	mixedID, err := store.ResolveOrCreateAccount(ctx, "default", "eval", "eval-mixed", "Mixed User")
	if err != nil {
		t.Fatalf("create mixed identity: %v", err)
	}
	if _, err := store.BindAccount(ctx, mixedID.TenantID, mixedID.PersonID, "cli", "mixed-terminal", "Mixed User"); err != nil {
		t.Fatalf("bind mixed cli account: %v", err)
	}

	taskIDs := map[string]string{}
	for label, id := range map[string]*IdentityContext{"eval": evalID, "real": realID} {
		ws, err := store.RegisterWorkspace(ctx, Workspace{
			TenantID: id.TenantID, OwnerPersonID: id.PersonID,
			Name: label + "-ws", LocalPath: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("register %s workspace: %v", label, err)
		}
		task, err := store.CreateTask(ctx, TaskCreate{
			TenantID: id.TenantID, PersonID: id.PersonID,
			WorkspaceID: ws.ID, Title: label + " task", Channel: "cli",
		})
		if err != nil {
			t.Fatalf("create %s task: %v", label, err)
		}
		taskIDs[label] = task.ID
		run, err := store.StartRun(ctx, task, "cli", label+" input")
		if err != nil {
			t.Fatalf("start %s run: %v", label, err)
		}
		if _, err := store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "run.started"}); err != nil {
			t.Fatalf("append %s event: %v", label, err)
		}
		if _, err := store.SaveHandoff(ctx, Handoff{TaskID: task.ID, Summary: label + " handoff"}); err != nil {
			t.Fatalf("save %s handoff: %v", label, err)
		}
		if _, err := store.SaveArtifact(ctx, Artifact{TaskID: task.ID, RunID: run.ID, Kind: "file", URI: "file:///" + label}); err != nil {
			t.Fatalf("save %s artifact: %v", label, err)
		}
	}
	return store, evalID, realID, mixedID, taskIDs
}

func TestCleanEvalResidueDryRunDeletesNothing(t *testing.T) {
	ctx := context.Background()
	store, evalID, realID, _, taskIDs := seedResidueWorld(t)

	report, err := store.CleanEvalResidue(ctx, false)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if report.Persons != 1 || report.Accounts != 1 || report.Tasks != 1 || report.Runs != 1 {
		t.Fatalf("dry run should select exactly the eval-only person tree, got %+v", report)
	}
	if report.Events != 1 || report.Handoffs != 1 || report.Artifacts != 1 || report.CurrentTask != 1 {
		t.Fatalf("dry run should count the eval task rows, got %+v", report)
	}
	if report.Tenants != 1 {
		t.Fatalf("the eval-only tenant should be reported as emptied, got %+v", report)
	}
	if len(report.PersonIDs) != 1 || report.PersonIDs[0] != evalID.PersonID {
		t.Fatalf("dry run should name the eval person, got %v", report.PersonIDs)
	}

	// Nothing may be deleted by a dry run.
	if task, _ := store.GetTask(ctx, evalID.TenantID, taskIDs["eval"]); task == nil {
		t.Fatal("dry run must not delete the eval task")
	}
	if same, err := store.ResolveOrCreateAccount(ctx, evalID.TenantID, "eval", "eval-case1", ""); err != nil || same.PersonID != evalID.PersonID {
		t.Fatalf("dry run must keep the eval account binding, got %v (%v)", same, err)
	}
	if task, _ := store.GetTask(ctx, realID.TenantID, taskIDs["real"]); task == nil {
		t.Fatal("dry run must not touch real data")
	}
}

func TestCleanEvalResidueDeletesOnlyEvalRows(t *testing.T) {
	ctx := context.Background()
	store, evalID, realID, mixedID, taskIDs := seedResidueWorld(t)

	report, err := store.CleanEvalResidue(ctx, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if report.Persons != 1 {
		t.Fatalf("apply should delete exactly one person, got %+v", report)
	}

	// Eval rows are gone.
	if task, _ := store.GetTask(ctx, evalID.TenantID, taskIDs["eval"]); task != nil {
		t.Fatal("eval task should be deleted")
	}
	if cur, _ := store.CurrentTask(ctx, evalID.TenantID, evalID.PersonID); cur != nil {
		t.Fatal("eval current_task pointer should be deleted")
	}
	if ws, _ := store.ListWorkspaces(ctx, evalID.TenantID, evalID.PersonID); len(ws) != 0 {
		t.Fatalf("eval workspaces should be deleted, got %d", len(ws))
	}
	if fresh, err := store.ResolveOrCreateAccount(ctx, evalID.TenantID, "eval", "eval-case1", ""); err != nil || fresh.PersonID == evalID.PersonID {
		t.Fatalf("eval account should be gone (resolve must mint a new person), got %v (%v)", fresh, err)
	}

	// Real person and mixed person survive untouched.
	if task, _ := store.GetTask(ctx, realID.TenantID, taskIDs["real"]); task == nil {
		t.Fatal("real task must survive")
	}
	if same, err := store.ResolveOrCreateAccount(ctx, realID.TenantID, "cli", "local", ""); err != nil || same.PersonID != realID.PersonID {
		t.Fatalf("real account must survive, got %v (%v)", same, err)
	}
	if same, err := store.ResolveOrCreateAccount(ctx, mixedID.TenantID, "eval", "eval-mixed", ""); err != nil || same.PersonID != mixedID.PersonID {
		t.Fatalf("mixed person (eval+cli accounts) must survive, got %v (%v)", same, err)
	}

	// A second pass finds nothing (idempotent) — except the eval account the
	// resolve call above just re-created, which now has a fresh person.
	report2, err := store.CleanEvalResidue(ctx, false)
	if err != nil {
		t.Fatalf("second dry run: %v", err)
	}
	if report2.Tasks != 0 || report2.Runs != 0 || report2.Events != 0 {
		t.Fatalf("second pass should find no task residue, got %+v", report2)
	}
}

func TestCleanEvalResidueIncludesSkillEvolutionRows(t *testing.T) {
	ctx := context.Background()
	store, evalID, realID, _, taskIDs := seedResidueWorld(t)
	var runID, workUnitID string
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM task_runs WHERE person_id=?`, evalID.PersonID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM run_work_units WHERE person_id=?`, evalID.PersonID).Scan(&workUnitID); err != nil {
		t.Fatal(err)
	}
	skillKey := SkillKey(evalID.TenantID, "eval-skill", "user", "agent-created", "/skills", "eval-skill/SKILL.md")
	if _, err := store.ActivateSkill(ctx, ActivateSkillInput{
		IdentityTenantID: evalID.TenantID, ControlTenantID: evalID.TenantID,
		PersonID: evalID.PersonID, RunID: runID, WorkUnitID: workUnitID,
		SkillKey: skillKey, SkillName: "eval-skill", VersionHash: "v1",
		ContentBody: "eval", CreatedBy: "eval",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	statements := []struct {
		query string
		args  []interface{}
	}{
		{`INSERT INTO workflow_profiles(run_id, tenant_id, person_id, task_id, workflow_signature, created_at) VALUES(?,?,?,?,?,?)`, []interface{}{runID, evalID.TenantID, evalID.PersonID, taskIDs["eval"], "sig", now}},
		{`INSERT INTO evolution_candidates(id, tenant_id, person_id, workflow_signature, kind, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`, []interface{}{"candidate-eval", evalID.TenantID, evalID.PersonID, "sig", "batch_read", now, now}},
		{`INSERT INTO task_skill_bindings(identity_tenant_id, person_id, task_id, control_tenant_id, skill_key, skill_name, state, binding_source, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, []interface{}{evalID.TenantID, evalID.PersonID, taskIDs["eval"], evalID.TenantID, skillKey, "eval-skill", "active", "eval", now, now}},
		{`INSERT INTO workflow_observations(id, identity_tenant_id, control_tenant_id, person_id, run_id, work_unit_id, workflow_signature, outcome_status, created_at) VALUES(?,?,?,?,?,?,?,?,?)`, []interface{}{"observation-eval", evalID.TenantID, evalID.TenantID, evalID.PersonID, runID, workUnitID, "sig", "completed", now}},
		{`INSERT INTO skill_failure_guards(control_tenant_id, skill_key, version_hash, failure_signature, state, source_run_id, created_at, last_seen_at) VALUES(?,?,?,?,?,?,?,?)`, []interface{}{evalID.TenantID, skillKey, "v1", "failure-eval", "active", runID, now, now}},
	}
	for _, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	report, err := store.CleanEvalResidue(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.WorkUnits != 1 || report.SkillActivations != 1 || report.SkillBindings != 1 ||
		report.WorkflowProfiles != 1 || report.EvolutionCandidates != 1 || report.WorkflowObservations != 1 ||
		report.SkillVersions != 1 || report.SkillFailureGuards != 1 {
		t.Fatalf("skill/evolution dry-run report=%+v", report)
	}
	if _, err := store.CleanEvalResidue(ctx, true); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"run_work_units", "run_skill_activations", "task_skill_bindings", "workflow_profiles", "evolution_candidates", "workflow_observations"} {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE person_id=?`, evalID.PersonID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s eval rows=%d err=%v", table, count, err)
		}
	}
	for _, table := range []string{"skill_versions", "skill_failure_guards"} {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE control_tenant_id=?`, evalID.TenantID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s eval tenant rows=%d err=%v", table, count, err)
		}
	}
	var realUnits int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM run_work_units WHERE person_id=?`, realID.PersonID).Scan(&realUnits); err != nil || realUnits != 1 {
		t.Fatalf("real work units=%d err=%v", realUnits, err)
	}
}

func TestListRunningRunsScopesToPersons(t *testing.T) {
	ctx := context.Background()
	store, evalID, realID, _, _ := seedResidueWorld(t)

	runs, err := store.ListRunningRuns(ctx, evalID.TenantID, []string{evalID.PersonID})
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(runs) != 1 || runs[0].PersonID != evalID.PersonID {
		t.Fatalf("expected exactly the eval person's running run, got %+v", runs)
	}
	// A different person filter must exclude the eval run even in-tenant.
	runs, err = store.ListRunningRuns(ctx, evalID.TenantID, []string{realID.PersonID})
	if err != nil {
		t.Fatalf("list running (other person): %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("person filter should exclude other persons' runs, got %+v", runs)
	}
}
