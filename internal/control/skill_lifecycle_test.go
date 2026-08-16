package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSkillLifecycleSeparatesWorkUnitsFallbackAndTaskAffinity(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	taskA, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Task A", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Task B", Channel: "cli", KeepCurrent: true})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, taskA, "cli", "handle A then B")
	if err != nil {
		t.Fatal(err)
	}
	if run.WorkUnitID == "" {
		t.Fatal("run did not receive a default work unit")
	}
	units, err := store.SyncRunWorkUnits(ctx, identity.TenantID, run.ID, []WorkUnitPlanInput{
		{GoalDigest: "handle A", PlanStatus: "in_progress", RelatedTaskID: taskA.ID},
		{GoalDigest: "handle B", PlanStatus: "pending", RelatedTaskID: taskB.ID},
	})
	if err != nil || len(units) != 2 {
		t.Fatalf("sync units=%+v err=%v", units, err)
	}
	firstIDs := []string{units[0].ID, units[1].ID}

	keyA := SkillKey("default", "skill-a", "user", "agent-created", "/skills", "skill-a/SKILL.md")
	activationA, err := store.ActivateSkill(ctx, ActivateSkillInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
		PersonID: identity.PersonID, RunID: run.ID, WorkUnitID: units[0].ID,
		SkillKey: keyA, SkillName: "skill-a", VersionHash: "v1", ActivationSource: "model",
		ContentBody: "procedure A", CreatedBy: "external_reconcile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FallbackCurrentSkill(ctx, SkillFallbackInput{
		IdentityTenantID: identity.TenantID, RunID: run.ID, WorkUnitID: units[0].ID,
		Reason: "precondition was false", FailureSignature: "failure-a", ErrorCategory: "stale_precondition",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivateSkill(ctx, ActivateSkillInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
		PersonID: identity.PersonID, RunID: run.ID, WorkUnitID: units[0].ID,
		SkillKey: "another", SkillName: "another", VersionHash: "v1",
	}); err == nil {
		t.Fatal("a fallback work unit accepted a replacement skill")
	}
	if activationA.State != SkillActivationActive {
		t.Fatalf("initial activation=%+v", activationA)
	}
	units, err = store.SyncRunWorkUnits(ctx, identity.TenantID, run.ID, []WorkUnitPlanInput{
		{WorkUnitID: firstIDs[0], GoalDigest: "handle A", PlanStatus: "completed", RelatedTaskID: taskA.ID},
		{WorkUnitID: firstIDs[1], GoalDigest: "handle B", PlanStatus: "in_progress", RelatedTaskID: taskB.ID},
	})
	if err != nil || units[0].ID != firstIDs[0] || units[1].ID != firstIDs[1] {
		t.Fatalf("work unit ids were not stable: %+v err=%v", units, err)
	}

	keyB := SkillKey("default", "skill-b", "user", "agent-created", "/skills", "skill-b/SKILL.md")
	if _, err := store.ActivateSkill(ctx, ActivateSkillInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
		PersonID: identity.PersonID, RunID: run.ID, WorkUnitID: units[1].ID,
		SkillKey: keyB, SkillName: "skill-b", VersionHash: "v2", ActivationSource: "model",
		ContentBody: "procedure B", CreatedBy: "external_reconcile",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "done", TaskID: taskA.ID,
		TaskStatus: "done", Summary: "both work units completed", VerificationState: "passed",
		Channel: "cli", Event: Event{Type: "run.finished", Payload: []byte(`{"status":"done"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if binding, err := store.GetTaskSkillBinding(ctx, identity.TenantID, identity.PersonID, taskA.ID); err != nil || binding != nil {
		t.Fatalf("fallback skill created task A affinity: %+v err=%v", binding, err)
	}
	bindingB, err := store.GetTaskSkillBinding(ctx, identity.TenantID, identity.PersonID, taskB.ID)
	if err != nil || bindingB == nil || bindingB.SkillKey != keyB || bindingB.State != TaskSkillBindingActive {
		t.Fatalf("task B binding=%+v err=%v", bindingB, err)
	}
	units, err = store.ListRunWorkUnits(ctx, identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if units[0].Status != WorkUnitCompleted || units[0].VerificationState != "not_applicable" {
		t.Fatalf("completed first unit did not retain its own projection: %+v", units[0])
	}
	if units[1].Status != WorkUnitCompleted || units[1].VerificationState != "passed" {
		t.Fatalf("live second unit did not receive the run fallback projection: %+v", units[1])
	}
}

func TestWorkUnitOutcomeAndActivationRemainIndependentWhenLaterUnitFails(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "mixed", "Mixed")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "mixed work", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "complete A then attempt B")
	units, err := store.SyncRunWorkUnits(ctx, identity.TenantID, run.ID, []WorkUnitPlanInput{
		{GoalDigest: "inspect component A", PlanStatus: "in_progress", RelatedTaskID: task.ID},
		{GoalDigest: "change component B", PlanStatus: "pending"},
	})
	if err != nil || len(units) != 2 {
		t.Fatalf("initial units=%+v err=%v", units, err)
	}
	keyA := SkillKey(identity.TenantID, "inspect-a", "user", "agent-created", "/skills", "inspect-a/SKILL.md")
	activationA, err := store.ActivateSkill(ctx, ActivateSkillInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, WorkUnitID: units[0].ID, SkillKey: keyA, SkillName: "inspect-a", VersionHash: "v1",
		ActivationSource: "model", ContentBody: "inspect A", CreatedBy: "external_reconcile",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "evidence.recorded", Payload: json.RawMessage(`{"evidence":{"tool_name":"verify","kind":"verification","status":"succeeded","started_at_unix_nano":10,"finished_at_unix_nano":20,"command":{"command":"verify A","kind":"test","exit_code":0}}}`)})
	units, err = store.SyncRunWorkUnits(ctx, identity.TenantID, run.ID, []WorkUnitPlanInput{
		{WorkUnitID: units[0].ID, GoalDigest: "inspect component A", PlanStatus: "completed", RelatedTaskID: task.ID},
		{WorkUnitID: units[1].ID, GoalDigest: "change component B", PlanStatus: "in_progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	keyB := SkillKey(identity.TenantID, "change-b", "user", "agent-created", "/skills", "change-b/SKILL.md")
	if _, err := store.ActivateSkill(ctx, ActivateSkillInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, WorkUnitID: units[1].ID, SkillKey: keyB, SkillName: "change-b", VersionHash: "v1",
		ActivationSource: "model", ContentBody: "change B", CreatedBy: "external_reconcile",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "failed", TaskID: task.ID, TaskStatus: "blocked",
		Summary: "B failed", VerificationState: "failed", Channel: "cli",
		Event: Event{Type: "run.finished", Payload: json.RawMessage(`{"status":"failed"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	units, _ = store.ListRunWorkUnits(ctx, identity.TenantID, run.ID)
	if units[0].Status != WorkUnitCompleted || units[0].VerificationState != "passed" {
		t.Fatalf("A inherited B failure: %+v", units[0])
	}
	if units[1].Status != WorkUnitFailed || units[1].VerificationState != "failed" {
		t.Fatalf("B did not retain run failure fallback: %+v", units[1])
	}
	activations, err := store.runSkillActivations(ctx, identity.TenantID, run.ID)
	if err != nil || len(activations) != 2 || activations[0].ID != activationA.ID || activations[0].State != SkillActivationCompleted || activations[1].State != SkillActivationFallback {
		t.Fatalf("activation states=%+v err=%v", activations, err)
	}
	observations, err := store.MaterializeWorkflowObservations(ctx, identity.TenantID, run.ID)
	if err != nil || len(observations) != 2 || observations[0].EvidenceRole != "success_path" || observations[1].EvidenceRole != "failure_guard" {
		t.Fatalf("mixed observations=%+v err=%v", observations, err)
	}
}

func TestWorkflowCohortRequiresMultipleComparableSuccessesAndKeepsFailures(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "cohort", "Cohort")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "inspect releases", Channel: "cli"})

	runOne := func(status, verification string, failTool bool) *Run {
		run, err := store.StartRun(ctx, task, "cli", "inspect release metadata 12345")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.started", Payload: json.RawMessage(`{"tool":"read_file"}`)})
		completed := map[string]interface{}{"tool": "read_file"}
		if failTool {
			completed["error"] = "not found"
		}
		raw, _ := json.Marshal(completed)
		_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.completed", Payload: raw})
		_, err = store.MaterializeRunFinalization(ctx, RunFinalization{
			Identity: *identity, RunID: run.ID, RunStatus: status, TaskID: task.ID,
			TaskStatus: "in_progress", Summary: "inspected release metadata", VerificationState: verification,
			Channel: "cli", Event: Event{Type: "run.finished", Payload: json.RawMessage(`{"status":"done"}`)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.MaterializeWorkflowObservations(ctx, identity.TenantID, run.ID); err != nil {
			t.Fatal(err)
		}
		return run
	}

	runOne("failed", "failed", true)
	first := runOne("done", "passed", false)
	if digests, err := store.ReadySkillEvidenceDigestsForRun(ctx, identity.TenantID, first.ID); err != nil || len(digests) != 0 {
		t.Fatalf("single success produced a cohort: %+v err=%v", digests, err)
	}
	second := runOne("done", "passed", false)
	if digests, err := store.ReadySkillEvidenceDigestsForRun(ctx, identity.TenantID, second.ID); err != nil || len(digests) != 0 {
		t.Fatalf("two successes produced a cohort: %+v err=%v", digests, err)
	}
	third := runOne("done", "passed", false)
	digests, err := store.ReadySkillEvidenceDigestsForRun(ctx, identity.TenantID, third.ID)
	if err != nil || len(digests) != 1 {
		t.Fatalf("ready cohort=%+v err=%v", digests, err)
	}
	digest := digests[0]
	if len(digest.SuccessObservations) != 3 || len(digest.NegativeObservations) != 1 {
		t.Fatalf("cohort did not preserve successes and relevant failure: %+v", digest)
	}
	ids := make([]string, 0, 4)
	for _, observation := range append(digest.SuccessObservations, digest.NegativeObservations...) {
		ids = append(ids, observation.ID)
	}
	key := SkillKey(identity.TenantID, "release-inspection", "user", "agent-created", "control://agent-created", "release-inspection/SKILL.md")
	if _, err := store.CreateSkillCandidateVersion(ctx, identity.TenantID, key, "release-inspection", "", "# Procedure\nInspect and verify.", digest.EvidenceSetHash, ids, digest); err != nil {
		t.Fatal(err)
	}
	var candidates, active int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_versions WHERE skill_key=? AND state='candidate'`, key).Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_versions WHERE skill_key=? AND state='active'`, key).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if candidates != 1 || active != 0 {
		t.Fatalf("candidate crossed active boundary: candidates=%d active=%d", candidates, active)
	}
}

func TestWorkflowCohortNominatesCJKParaphrasesAndSeparatesSkillVersions(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "cjk-cohort", "CJK")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "发布检查", Channel: "cli"})
	goals := []string{"检查发布元数据 12345", "查看发布元数据 67890", "核对发布元数据 24680"}
	var latest *Run
	for _, goal := range goals {
		run, runErr := store.StartRun(ctx, task, "cli", goal)
		if runErr != nil {
			t.Fatal(runErr)
		}
		latest = run
		_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.started", Payload: json.RawMessage(`{"tool":"read_file"}`)})
		_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.completed", Payload: json.RawMessage(`{"tool":"read_file"}`)})
		if _, runErr = store.MaterializeRunFinalization(ctx, RunFinalization{
			Identity: *identity, RunID: run.ID, RunStatus: "done", TaskID: task.ID, TaskStatus: "in_progress",
			Summary: "已核对", VerificationState: "passed", Channel: "cli",
			Event: Event{Type: "run.finished", Payload: json.RawMessage(`{"status":"done"}`)},
		}); runErr != nil {
			t.Fatal(runErr)
		}
		if _, runErr = store.MaterializeWorkflowObservations(ctx, identity.TenantID, run.ID); runErr != nil {
			t.Fatal(runErr)
		}
	}
	digests, err := store.ReadySkillEvidenceDigestsForRun(ctx, identity.TenantID, latest.ID)
	if err != nil || len(digests) != 1 || len(digests[0].SuccessObservations) != 3 {
		t.Fatalf("CJK paraphrase cohort=%+v err=%v", digests, err)
	}
	anchor := WorkflowObservation{SkillKey: "skill", VersionHash: "v2", EnvironmentFingerprint: "env", GoalDigest: "检查发布元数据", ToolSequence: []string{"file.read"}}
	older := anchor
	older.VersionHash = "v1"
	if workflowObservationsComparable(anchor, older) {
		t.Fatal("different Skill versions entered the same comparable cohort")
	}
}

func TestWorkUnitStableIDsSurvivePlanReordering(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "reorder", "Reorder")
	taskA, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "A", Channel: "cli"})
	taskB, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "B", Channel: "cli", KeepCurrent: true})
	run, _ := store.StartRun(ctx, taskA, "cli", "A and B")
	units, err := store.SyncRunWorkUnits(ctx, identity.TenantID, run.ID, []WorkUnitPlanInput{
		{GoalDigest: "handle A", PlanStatus: "in_progress", RelatedTaskID: taskA.ID},
		{GoalDigest: "handle B", PlanStatus: "pending", RelatedTaskID: taskB.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	idA, idB := units[0].ID, units[1].ID
	units, err = store.SyncRunWorkUnits(ctx, identity.TenantID, run.ID, []WorkUnitPlanInput{
		{WorkUnitID: idB, GoalDigest: "handle B", PlanStatus: "in_progress", RelatedTaskID: taskB.ID},
		{WorkUnitID: idA, GoalDigest: "handle A", PlanStatus: "pending", RelatedTaskID: taskA.ID},
	})
	if err != nil || units[0].ID != idB || units[1].ID != idA {
		t.Fatalf("reordered units lost identity: %+v err=%v", units, err)
	}
}

func TestWeakPlanCannotAttachAnotherPersonsTask(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
	other, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "other", "Other")
	ownerTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "owner", Channel: "cli"})
	otherTask, _ := store.CreateTask(ctx, TaskCreate{TenantID: other.TenantID, PersonID: other.PersonID, Title: "other", Channel: "cli"})
	run, _ := store.StartRun(ctx, ownerTask, "cli", "input")
	if _, err := store.SyncRunWorkUnits(ctx, owner.TenantID, run.ID, []WorkUnitPlanInput{{GoalDigest: "steal", PlanStatus: "in_progress", RelatedTaskID: otherTask.ID}}); err == nil {
		t.Fatal("cross-person related task was accepted")
	}
}

func TestSkillFailureGuardMatchesOnlyTheSameInputShape(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "guard", "Guard")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "inspect", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "inspect build 12345 at /tmp/build-123")
	key := SkillKey(identity.TenantID, "inspect-build", "user", "agent-created", "/skills", "inspect-build/SKILL.md")
	if _, err := store.ActivateSkill(ctx, ActivateSkillInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
		PersonID: identity.PersonID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
		SkillKey: key, SkillName: "inspect-build", VersionHash: "v1", ContentBody: "inspect",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FallbackCurrentSkill(ctx, SkillFallbackInput{
		IdentityTenantID: identity.TenantID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
		Reason: "stale command", FailureSignature: "sig-1", FailedStepID: "read-build",
		ErrorCategory: "stale_precondition",
	}); err != nil {
		t.Fatal(err)
	}
	guard, err := store.MatchSkillFailureGuardForWorkUnit(ctx, identity.TenantID, key, "v1", run.ID, run.WorkUnitID)
	if err != nil || guard == nil || guard.OccurrenceCount != 1 {
		t.Fatalf("guard=%+v err=%v", guard, err)
	}
	if guard.NormalizedInputShape == "" || guard.NormalizedInputShape == "inspect build 12345 at /tmp/build-123" {
		t.Fatalf("input shape was not normalized and hashed: %+v", guard)
	}
	count, err := store.RecordSkillFailureGuardMatch(ctx, *guard)
	if err != nil || count != 2 {
		t.Fatalf("matched count=%d err=%v", count, err)
	}
}

func TestTransientSkillFallbackDoesNotCreateFailureGuard(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "transient-guard", "Guard")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "fetch", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "fetch status")
	if _, err := store.ActivateSkill(ctx, ActivateSkillInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
		PersonID: identity.PersonID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
		SkillKey: "fetch-skill", SkillName: "fetch-skill", VersionHash: "v1", ContentBody: "fetch",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FallbackCurrentSkill(ctx, SkillFallbackInput{
		IdentityTenantID: identity.TenantID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
		Reason: "temporary network timeout", FailureSignature: "transient-sig",
		FailedStepID: "fetch", ErrorCategory: "network_transient",
	}); err != nil {
		t.Fatal(err)
	}
	guard, err := store.MatchSkillFailureGuardForWorkUnit(ctx, identity.TenantID, "fetch-skill", "v1", run.ID, run.WorkUnitID)
	if err != nil || guard != nil {
		t.Fatalf("transient incident became a durable Skill guard: %+v err=%v", guard, err)
	}
}

func TestWorkflowGoalSanitizationRemovesCredentialValues(t *testing.T) {
	got := sanitizeWorkflowGoal("inspect token=super-secret-value and sk-abcdefghijklmnopqrstuvwxyz123456")
	if strings.Contains(got, "super-secret-value") || strings.Contains(got, "sk-abcdefghijklmnopqrstuvwxyz123456") || !strings.Contains(got, "[redacted") {
		t.Fatalf("workflow goal retained credential material: %s", got)
	}
}
