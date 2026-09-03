package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
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
	taskB, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Task B", Channel: "cli"})
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

func TestWaitingRunsParkWorkUnitsAndSkillActivations(t *testing.T) {
	for _, status := range []string{"waiting_user", "waiting_external", "waiting_finalization"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			store, err := OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "park-"+status, "Parked")
			task, _ := store.CreateTask(ctx, TaskCreate{
				TenantID: identity.TenantID, PersonID: identity.PersonID,
				Title: "prepare release", Channel: "cli",
			})
			run, _ := store.StartRun(ctx, task, "cli", "prepare release and wait")
			key := SkillKey(identity.TenantID, "prepare-release", "user", "agent-created", "/skills", "prepare-release/SKILL.md")
			if _, err := store.ActivateSkill(ctx, ActivateSkillInput{
				IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID,
				PersonID: identity.PersonID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
				SkillKey: key, SkillName: "prepare-release", VersionHash: "v1",
				ActivationSource: "model", ContentBody: "prepare", CreatedBy: "external_reconcile",
			}); err != nil {
				t.Fatal(err)
			}
			_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.started", Payload: json.RawMessage(`{"tool":"read_file"}`)})
			_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.completed", Payload: json.RawMessage(`{"tool":"read_file"}`)})
			if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
				Identity: *identity, RunID: run.ID, RunStatus: status, TaskID: task.ID,
				TaskStatus: status, Summary: "prepared and parked", VerificationState: "passed",
				Channel: "cli", Event: Event{Type: "run.finished", Payload: json.RawMessage(`{"status":"` + status + `"}`)},
			}); err != nil {
				t.Fatal(err)
			}
			units, err := store.ListRunWorkUnits(ctx, identity.TenantID, run.ID)
			if err != nil || len(units) != 1 || units[0].Status != WorkUnitParked {
				t.Fatalf("parked units=%+v err=%v", units, err)
			}
			activations, err := store.runSkillActivations(ctx, identity.TenantID, run.ID)
			if err != nil || len(activations) != 1 || activations[0].State != SkillActivationParked || activations[0].FallbackReason != "" {
				t.Fatalf("parked activations=%+v err=%v", activations, err)
			}
			observations, err := store.MaterializeWorkflowObservations(ctx, identity.TenantID, run.ID)
			if err != nil || len(observations) != 1 || observations[0].EvidenceRole != "audit" || observations[0].OutcomeStatus != WorkUnitParked {
				t.Fatalf("parked observations=%+v err=%v", observations, err)
			}
			if binding, err := store.GetTaskSkillBinding(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil || binding != nil {
				t.Fatalf("parked run created binding=%+v err=%v", binding, err)
			}
			var guards int
			if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_failure_guards WHERE source_run_id=?`, run.ID).Scan(&guards); err != nil || guards != 0 {
				t.Fatalf("parked run created %d failure guards: %v", guards, err)
			}
		})
	}
}

func TestWaitingRunWithFailedVerificationRemainsFailure(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "park-failed", "Parked")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "verify", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "verify and wait")
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "waiting_user", TaskID: task.ID,
		TaskStatus: "waiting_user", Summary: "verification failed", VerificationState: "failed",
	}); err != nil {
		t.Fatal(err)
	}
	units, _ := store.ListRunWorkUnits(ctx, identity.TenantID, run.ID)
	if len(units) != 1 || units[0].Status != WorkUnitFailed {
		t.Fatalf("failed verification was parked: %+v", units)
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

func TestVerifiedSkillFallbackRecoveryNominatesImmediateRepair(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "repair", "Repair")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "repair release skill", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "update release record safely")
	key := SkillKey(identity.TenantID, "release-record", "user", "agent-created", "/skills", "release-record/SKILL.md")
	activeContent := `---
name: release-record
description: Update release records.
---

## Applicability
Release record updates.

## Inputs
A release record.

## Preconditions
The record exists.

## Procedure
Write the legacy layout.

## Failure Guards
Do not guess the layout.

## Recovery
Return to ordinary planning.

## Verification
Read the result.`
	activeHash, err := store.CreateSkillCandidateVersion(ctx, identity.TenantID, key, "release-record", "", activeContent, "initial-repair-evidence", []string{"initial"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PromoteSkillCandidate(ctx, identity.TenantID, key, activeHash, "/skills/release-record/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivateSkill(ctx, ActivateSkillInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, WorkUnitID: run.WorkUnitID, SkillKey: key, SkillName: "release-record", VersionHash: activeHash,
		ActivationSource: "binding", ContentBody: activeContent, CreatedBy: "external_reconcile",
	}); err != nil {
		t.Fatal(err)
	}
	started := json.RawMessage(`{"tool":"write_file","tool_origin":"builtin","tool_category":"general","tool_risk_level":"high","tool_read_only":false,"operation_classes":["write"]}`)
	_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.started", Payload: started})
	_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.completed", Payload: json.RawMessage(`{"tool":"write_file","tool_call_id":"write-old-layout","error":"stale layout","error_category":"interface_drift"}`)})
	// A successful diagnostic read must not erase the actual failed call that
	// the later fallback attributes to the active Skill.
	_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.started", Payload: json.RawMessage(`{"tool":"read_file","tool_call_id":"inspect-layout","tool_origin":"builtin","tool_category":"general","tool_risk_level":"low","tool_read_only":true}`)})
	_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.completed", Payload: json.RawMessage(`{"tool":"read_file","tool_call_id":"inspect-layout"}`)})
	if _, err := store.FallbackCurrentSkill(ctx, SkillFallbackInput{
		IdentityTenantID: identity.TenantID, RunID: run.ID, WorkUnitID: run.WorkUnitID,
		Reason: "the Skill used the old record layout", FailureSignature: "layout-v1",
		FailedStepID: "Procedure", ErrorCategory: "stale_precondition", NormalizedInputShape: "release record",
	}); err != nil {
		t.Fatal(err)
	}
	fallbackPayload, _ := json.Marshal(map[string]interface{}{
		"work_unit_id": run.WorkUnitID, "skill_key": key, "version_hash": activeHash,
		"failure_signature": "layout-v1", "failed_step_id": "Procedure",
		"failed_tool_call_id": "write-old-layout",
		"error_category":      "stale_precondition", "normalized_input_shape": "release record",
		"reason": "the Skill used the old record layout",
	})
	_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "skill.fallback", Payload: fallbackPayload})
	_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.started", Payload: started})
	_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.completed", Payload: json.RawMessage(`{"tool":"write_file"}`)})
	if _, err := store.MaterializeRunFinalization(ctx, RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "done", TaskID: task.ID, TaskStatus: "done",
		Summary: "record updated with the current layout", VerificationState: "passed", Channel: "cli",
		Event: Event{Type: "run.finished", Payload: json.RawMessage(`{"status":"done"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeWorkflowObservations(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	digests, err := store.ReadySkillEvidenceDigestsForRun(ctx, identity.TenantID, run.ID)
	if err != nil || len(digests) != 1 {
		t.Fatalf("repair digest=%+v err=%v", digests, err)
	}
	digest := digests[0]
	if digest.TargetSkillKey != key || len(digest.NegativeObservations) != 1 || len(digest.SuccessObservations) != 0 {
		t.Fatalf("repair cohort=%+v", digest)
	}
	incident := digest.NegativeObservations[0].Incident
	if incident == nil || !incident.RecoveryVerified || !incident.FailureObserved || incident.FailedToolCallID != "write-old-layout" || incident.FailedStepID != "Procedure" || len(incident.RecoveryToolSequence) != 1 {
		t.Fatalf("repair incident=%+v", incident)
	}
	if len(digest.NegativeObservations[0].ToolEvidence) != 3 || digest.NegativeObservations[0].ToolEvidence[0].Origin != "builtin" {
		t.Fatalf("tool evidence=%+v", digest.NegativeObservations[0].ToolEvidence)
	}
}

func TestSkillFallbackFailureAttributionRejectsUnknownCallID(t *testing.T) {
	metrics := accumulateWorkUnitEvents([]RunWorkUnit{{ID: "unit-1"}}, []evolutionEvent{
		{typ: "skill.activated", payload: map[string]interface{}{"work_unit_id": "unit-1"}},
		{typ: "tool.completed", payload: map[string]interface{}{
			"tool": "write_file", "tool_call_id": "actual-failure", "error": "stale", "error_category": "interface_drift",
		}},
		{typ: "skill.fallback", payload: map[string]interface{}{
			"work_unit_id": "unit-1", "failed_tool_call_id": "invented-failure",
			"failure_signature": "layout-v1", "failed_step_id": "Procedure", "error_category": "stale_precondition",
		}},
	})
	incident := metrics["unit-1"].incident
	if incident == nil || incident.FailureObserved || incident.FailedToolCallID != "" {
		t.Fatalf("unknown call id authorized repair evidence: %+v", incident)
	}
}

func TestCompletedToolMetadataReplacesPreflightEvidence(t *testing.T) {
	metrics := accumulateWorkUnitEvents([]RunWorkUnit{{ID: "unit-1"}}, []evolutionEvent{
		{typ: "tool.started", payload: map[string]interface{}{
			"work_unit_id": "unit-1", "tool": "terminal", "tool_call_id": "call-1",
			"tool_origin": "builtin", "tool_category": "general", "tool_risk_level": "high",
			"operation_classes": []interface{}{"exec.in_turn"},
		}},
		{typ: "tool.completed", payload: map[string]interface{}{
			"work_unit_id": "unit-1", "tool": "terminal", "tool_call_id": "call-1",
			"tool_origin": "builtin", "tool_category": "general", "tool_risk_level": "high",
			"operation_classes": []interface{}{"exec.in_turn", "dangerous"},
		}},
	})
	evidence := metrics["unit-1"].toolEvidence
	if len(evidence) != 1 || evidence[0].ToolCallID != "call-1" || len(evidence[0].OperationClasses) != 2 {
		t.Fatalf("merged evidence=%+v", evidence)
	}
}

func TestSkillRepairObservedFailureCategoryMapping(t *testing.T) {
	tests := []struct {
		repair   string
		observed string
		want     bool
	}{
		{repair: "stale_precondition", observed: "interface_drift", want: true},
		{repair: "schema_changed", observed: "not_found", want: true},
		{repair: "verification_mismatch", observed: "check_definition", want: true},
		{repair: "invalid_procedure", observed: "syntax", want: true},
		{repair: "stale_precondition", observed: "tool_schema", want: false},
		{repair: "verification_mismatch", observed: "not_found", want: false},
		{repair: "invented", observed: "command_failed", want: false},
	}
	for _, tt := range tests {
		if got := SkillRepairObservedFailureEligible(tt.repair, tt.observed); got != tt.want {
			t.Errorf("eligible(%q, %q)=%v, want %v", tt.repair, tt.observed, got, tt.want)
		}
	}
}

func TestVerifiedSkillRepairIncidentRejectsTransientFailure(t *testing.T) {
	observation := WorkflowObservation{
		EvidenceRole: "failure_guard",
		Incident: &SkillIncidentEvidence{
			FailureSignature: "temporary-network", FailedStepID: "Procedure",
			ErrorCategory: "network_transient", ObservedErrorCategory: "command_failed",
			FailureObserved: true, RecoveryVerified: true,
		},
	}
	if VerifiedSkillRepairIncident(observation) {
		t.Fatal("transient failure became automatic Skill repair evidence")
	}
}

func TestSkillRepairEvidenceClassesEnforceDifferentThresholds(t *testing.T) {
	observation := func(runID, signature, repairCategory, observedCategory string) WorkflowObservation {
		return WorkflowObservation{
			RunID: runID, EvidenceRole: "failure_guard",
			Incident: &SkillIncidentEvidence{
				FailureSignature: signature, FailedStepID: "Procedure",
				ErrorCategory: repairCategory, ObservedErrorCategory: observedCategory,
				FailureObserved: true, RecoveryVerified: true,
			},
		}
	}
	interfaceDrift := observation("run-a", "schema-v2", "schema_changed", "tool_schema")
	if got := ClassifySkillRepairIncident(interfaceDrift.Incident); got != SkillRepairClassDeterministicInterface {
		t.Fatalf("interface class=%q", got)
	}
	if !SkillRepairAutomaticPromotionReady(SkillEvidenceDigest{NegativeObservations: []WorkflowObservation{interfaceDrift}}) {
		t.Fatal("one verified deterministic interface recovery did not become ready")
	}

	precondition := observation("run-a", "manifest-moved", "stale_precondition", "not_found")
	if got := ClassifySkillRepairIncident(precondition.Incident); got != SkillRepairClassStablePrecondition {
		t.Fatalf("precondition class=%q", got)
	}
	if SkillRepairAutomaticPromotionReady(SkillEvidenceDigest{PublicationScope: "user", WorkspaceID: "workspace-a", NegativeObservations: []WorkflowObservation{precondition}}) {
		t.Fatal("one workspace incident rewrote a user-global precondition")
	}
	if !SkillRepairAutomaticPromotionReady(SkillEvidenceDigest{PublicationScope: "workspace", WorkspaceID: "workspace-a", NegativeObservations: []WorkflowObservation{precondition}}) {
		t.Fatal("verified workspace-scoped precondition recovery did not become ready")
	}

	semantic := []WorkflowObservation{
		observation("run-a", "meaning-v2", "verification_mismatch", "check_definition"),
		observation("run-b", "meaning-v2", "verification_mismatch", "check_definition"),
	}
	if got := ClassifySkillRepairIncident(semantic[0].Incident); got != SkillRepairClassSemantic {
		t.Fatalf("semantic class=%q", got)
	}
	if !SkillRepairCandidateEvidenceReady(SkillEvidenceDigest{NegativeObservations: semantic[:1]}) {
		t.Fatal("one semantic recovery did not create a reviewable candidate")
	}
	if SkillRepairAutomaticPromotionReady(SkillEvidenceDigest{NegativeObservations: semantic}) {
		t.Fatal("two semantic recoveries crossed the three-run threshold")
	}
	semantic = append(semantic, observation("run-c", "meaning-v2", "verification_mismatch", "check_definition"))
	if !SkillRepairAutomaticPromotionReady(SkillEvidenceDigest{NegativeObservations: semantic}) {
		t.Fatal("three independent semantic recoveries did not become ready")
	}
	semantic[2].RunID = "run-b"
	if SkillRepairAutomaticPromotionReady(SkillEvidenceDigest{NegativeObservations: semantic}) {
		t.Fatal("replayed semantic recovery counted as independent evidence")
	}

	notApplicable := observation("run-a", "wrong-selection", "missing_failure_guard", "not_found")
	if got := ClassifySkillRepairIncident(notApplicable.Incident); got != SkillRepairClassNotApplicable {
		t.Fatalf("not-applicable class=%q", got)
	}
	if !SkillRepairCandidateEvidenceReady(SkillEvidenceDigest{PublicationScope: "workspace", WorkspaceID: "workspace-a", NegativeObservations: []WorkflowObservation{notApplicable}}) {
		t.Fatal("not-applicable incident did not remain available for applicability review")
	}
	if SkillRepairAutomaticPromotionReady(SkillEvidenceDigest{PublicationScope: "workspace", WorkspaceID: "workspace-a", NegativeObservations: []WorkflowObservation{notApplicable}}) {
		t.Fatal("not-applicable incident authorized a procedure repair")
	}
}

func TestSkillRepairContentTopologyRequiresCanonicalUniqueSections(t *testing.T) {
	canonical := `## Applicability
A
## Inputs
I
## Preconditions
P
## Procedure
P
## Failure Guards
F
## Recovery
R
## Verification
V`
	if !skillRepairContentTopologyEligible(canonical) {
		t.Fatal("canonical repair topology was rejected")
	}
	if skillRepairContentTopologyEligible(canonical + "\n## Recovery\nDuplicate") {
		t.Fatal("duplicate repair section was accepted")
	}
	if skillRepairContentTopologyEligible(strings.Replace(canonical, "## Recovery\nR\n## Verification", "## Verification\nV\n## Recovery", 1)) {
		t.Fatal("reordered repair topology was accepted")
	}
}

func TestWorkflowCohortExcludesExternalWatchRuns(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "watch-cohort", "Watch")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "inspect release", Channel: "cli"})
	runOne := func(origin string) *Run {
		run, runErr := store.StartRun(ctx, task, "cli", "inspect release metadata")
		if runErr != nil {
			t.Fatal(runErr)
		}
		started, _ := json.Marshal(map[string]string{"origin": origin})
		_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "run.started", Payload: started})
		_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.started", Payload: json.RawMessage(`{"tool":"read_file"}`)})
		_, _ = store.AppendEvent(ctx, Event{TaskID: task.ID, RunID: run.ID, Type: "tool.completed", Payload: json.RawMessage(`{"tool":"read_file"}`)})
		if _, runErr = store.MaterializeRunFinalization(ctx, RunFinalization{
			Identity: *identity, RunID: run.ID, RunStatus: "done", TaskID: task.ID,
			TaskStatus: "in_progress", Summary: "inspected", VerificationState: "passed",
			Event: Event{Type: "run.finished", Payload: json.RawMessage(`{"status":"done"}`)},
		}); runErr != nil {
			t.Fatal(runErr)
		}
		if _, runErr = store.MaterializeWorkflowObservations(ctx, identity.TenantID, run.ID); runErr != nil {
			t.Fatal(runErr)
		}
		return run
	}
	var latestWatch *Run
	for i := 0; i < 3; i++ {
		latestWatch = runOne("watch")
	}
	if digests, err := store.ReadySkillEvidenceDigestsForRun(ctx, identity.TenantID, latestWatch.ID); err != nil || len(digests) != 0 {
		t.Fatalf("watch run nominated cohort=%+v err=%v", digests, err)
	}
	foreground := runOne("")
	if digests, err := store.ReadySkillEvidenceDigestsForRun(ctx, identity.TenantID, foreground.ID); err != nil || len(digests) != 0 {
		t.Fatalf("watch history leaked into foreground cohort=%+v err=%v", digests, err)
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
	taskB, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "B", Channel: "cli"})
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

func TestStaleWorkUnitReferenceReturnsCurrentRunIDs(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "stale-unit", "Stale Unit")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "work", Channel: "cli"})
	run, _ := store.StartRun(ctx, task, "cli", "work")

	_, err = store.SyncRunWorkUnits(ctx, identity.TenantID, run.ID, []WorkUnitPlanInput{{
		WorkUnitID: "wu-from-prior-run", GoalDigest: "work", PlanStatus: "in_progress",
	}})
	var stale *StaleWorkUnitReferenceError
	if !errors.As(err, &stale) {
		t.Fatalf("SyncRunWorkUnits error = %T %v", err, err)
	}
	current := stale.CurrentWorkUnitIDs()
	if stale.WorkUnitID != "wu-from-prior-run" || stale.RunID != run.ID || len(current) != 1 || current[0] != run.WorkUnitID {
		t.Fatalf("stale work-unit detail = %+v current=%v", stale, current)
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

func TestSkillFailureGuardMatchesOnlyTheSameEnvironmentFingerprint(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "guard-environment", "Guard")
	task, _ := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "inspect", Channel: "cli"})
	runA, _ := store.StartRun(ctx, task, "cli", "inspect the release manifest")
	if _, err := store.MaterializeExecutionLease(ctx, executionenv.Lease{
		RunID: runA.ID, TenantID: identity.TenantID, PersonID: identity.PersonID, EnvironmentFingerprint: "environment-a",
	}); err != nil {
		t.Fatal(err)
	}
	key := SkillKey(identity.TenantID, "inspect-build", "user", "agent-created", "/skills", "inspect-build/SKILL.md")
	if _, err := store.ActivateSkill(ctx, ActivateSkillInput{
		IdentityTenantID: identity.TenantID, ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: runA.ID, WorkUnitID: runA.WorkUnitID, SkillKey: key, SkillName: "inspect-build", VersionHash: "v1", ContentBody: "inspect",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FallbackCurrentSkill(ctx, SkillFallbackInput{
		IdentityTenantID: identity.TenantID, RunID: runA.ID, WorkUnitID: runA.WorkUnitID,
		Reason: "schema changed", FailureSignature: "schema-v2", FailedStepID: "Procedure", ErrorCategory: "schema_changed",
	}); err != nil {
		t.Fatal(err)
	}
	if guard, err := store.MatchSkillFailureGuardForWorkUnit(ctx, identity.TenantID, key, "v1", runA.ID, runA.WorkUnitID); err != nil || guard == nil || guard.EnvironmentFingerprint != "environment-a" {
		t.Fatalf("same-environment guard=%+v err=%v", guard, err)
	}
	runB, _ := store.StartRun(ctx, task, "cli", "inspect the release manifest")
	if _, err := store.MaterializeExecutionLease(ctx, executionenv.Lease{
		RunID: runB.ID, TenantID: identity.TenantID, PersonID: identity.PersonID, EnvironmentFingerprint: "environment-b",
	}); err != nil {
		t.Fatal(err)
	}
	if guard, err := store.MatchSkillFailureGuardForWorkUnit(ctx, identity.TenantID, key, "v1", runB.ID, runB.WorkUnitID); err != nil || guard != nil {
		t.Fatalf("cross-environment guard=%+v err=%v", guard, err)
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
