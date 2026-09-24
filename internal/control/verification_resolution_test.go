package control

import (
	"context"
	"encoding/json"
	"selfmind/internal/verification"
	"strings"
	"testing"
)

func TestVerificationResolutionAndExactBlocker(t *testing.T) {
	ctx := context.Background()
	s, identity, _, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "two conditions", []RunPlanStepInput{
		{Step: "Inspect input", Status: "in_progress", SuccessCriteria: "input is complete", VerificationRequired: true},
		{Step: "Validate output", Status: "pending", SuccessCriteria: "output preserves input", VerificationRequired: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{}, "/workspace")
	if err != nil || first == nil || first.Target != plan.Plan.Steps[0].StepID || first.StepID != plan.Plan.Steps[0].StepID || first.Version != 3 {
		t.Fatalf("binding=%+v err=%v", first, err)
	}
	second, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{StepID: plan.Plan.Steps[1].StepID}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	add := func(id, status string, b *verification.Binding, at int64) {
		t.Helper()
		raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{"kind": "verification", "tool_name": "verify", "tool_call_id": id, "status": status, "started_at_unix_nano": at, "finished_at_unix_nano": at + 1, "command": map[string]interface{}{"command": "different methods", "cwd": "/workspace", "binding": b}}})
		if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}
	add("input-pass", "succeeded", first, 10)
	add("output-fail", "failed", second, 20)
	complete := []RunPlanStepInput{}
	for _, step := range plan.Plan.Steps {
		complete = append(complete, RunPlanStepInput{StepID: step.StepID, Step: step.Step, Status: "completed"})
	}
	if _, err = s.SyncRunPlan(ctx, identity.TenantID, run.ID, "finish", complete); err == nil || !strings.Contains(err.Error(), "Validate output") || !strings.Contains(err.Error(), "output-fail") {
		t.Fatalf("wrong blocking obligation: %v", err)
	}
	// The rejected transaction must preserve the previous plan and open window.
	unchanged, _ := s.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if unchanged.Version != plan.Plan.Version {
		t.Fatal("failed completion committed new plan")
	}
	replacement, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{Replaces: "output-fail", Reason: "Use a supported reader to check the same output preservation condition"}, "/workspace")
	if err != nil || replacement == nil || replacement.Criterion != second.Criterion || replacement.StepID != second.StepID {
		t.Fatalf("inheritance=%+v err=%v", replacement, err)
	}
	add("output-corrected", "succeeded", replacement, 30)
	if _, err = s.SyncRunPlan(ctx, identity.TenantID, run.ID, "verified", complete); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	s.db.QueryRowContext(ctx, "SELECT count(*) FROM task_events WHERE run_id=? AND type='evidence.recorded'", run.ID).Scan(&count)
	if count != 3 {
		t.Fatal("failure history lost")
	}
	for _, bad := range []verification.Binding{
		{Replaces: "output-fail", Reason: "retry", Criterion: "weaker condition"},
		{Replaces: "output-fail", Reason: "retry", Target: "another output"},
		{StepID: "foreign-step"},
	} {
		if _, err = s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, bad, "/workspace"); err == nil {
			t.Fatalf("invalid reference accepted: %+v", bad)
		}
	}
	if _, err = s.ResolveVerificationBinding(ctx, "other-person-tenant", run.ID, verification.Binding{Replaces: "output-fail", Reason: "retry"}, "/workspace"); err == nil {
		t.Fatal("foreign evidence accepted")
	}
}

func TestVerificationBindsSpecificSubcheckToActiveRequiredStep(t *testing.T) {
	ctx := context.Background()
	s, identity, _, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "verify bytes", []RunPlanStepInput{{
		Step: "Verify generated output", Status: "in_progress",
		SuccessCriteria: "the generated output satisfies the requested format", VerificationRequired: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{
		Criterion: "result.txt ends in one newline", Target: "result.txt",
	}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil || binding.Version != 3 || binding.StepID != plan.Plan.Steps[0].StepID ||
		binding.Criterion != "result.txt ends in one newline" || binding.Target != "result.txt" {
		t.Fatalf("specific check lost model judgment or runtime identity: %+v", binding)
	}
}

func TestVerificationBindsToUniqueOpenRequiredStepBeforePlanTransition(t *testing.T) {
	ctx := context.Background()
	s, identity, _, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "produce and verify", []RunPlanStepInput{
		{Step: "Produce output", Status: "in_progress", SuccessCriteria: "output exists"},
		{Step: "Verify output", Status: "pending", SuccessCriteria: "output contains the expected value", VerificationRequired: true},
		{Step: "Write independent report", Status: "pending", SuccessCriteria: "report explains the result"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := []string{"/workspace/output.txt"}
	binding, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{
		Criterion: "output value equals 7", Target: "output.txt", LocalDependencies: &deps,
	}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	want := plan.Plan.Steps[1].StepID
	if binding == nil || binding.Version != 3 || binding.StepID != want || binding.Criterion != "output value equals 7" || binding.Target != "output.txt" {
		t.Fatalf("binding=%+v, want runtime identity %s on the unique open obligation", binding, want)
	}
	appendEvidence := func(evidence map[string]interface{}) {
		t.Helper()
		raw, _ := json.Marshal(map[string]interface{}{"evidence": evidence})
		if _, appendErr := s.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: raw}); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	appendEvidence(map[string]interface{}{
		"kind": "mutation", "tool_name": "write_file", "tool_call_id": "source-write", "status": "succeeded",
		"started_at_unix_nano": 10, "finished_at_unix_nano": 11,
		"files": []map[string]interface{}{{"path": "/workspace/output.txt", "before_sha256": "", "after_sha256": "source"}},
	})
	appendEvidence(map[string]interface{}{
		"kind": "verification", "tool_name": "verify", "tool_call_id": "source-check", "status": "succeeded",
		"started_at_unix_nano": 20, "finished_at_unix_nano": 21,
		"command": map[string]interface{}{"command": "check output", "cwd": "/workspace", "binding": binding},
	})
	appendEvidence(map[string]interface{}{
		"kind": "mutation", "tool_name": "write_file", "tool_call_id": "report-write", "status": "succeeded",
		"started_at_unix_nano": 30, "finished_at_unix_nano": 31,
		"files": []map[string]interface{}{{"path": "/workspace/report.md", "before_sha256": "", "after_sha256": "report"}},
	})
	if _, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "all outputs complete", []RunPlanStepInput{
		{StepID: plan.Plan.Steps[0].StepID, Status: "completed"},
		{StepID: plan.Plan.Steps[1].StepID, Status: "completed"},
		{StepID: plan.Plan.Steps[2].StepID, Status: "completed"},
	}); err != nil {
		t.Fatalf("independent report invalidated the bound source check: %v", err)
	}
}

func TestVerificationBindsEarliestOpenRequiredStepWithinActiveWorkUnit(t *testing.T) {
	ctx := context.Background()
	s, identity, _, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "inspect then check", []RunPlanStepInput{
		{Step: "Inspect inputs", Status: "in_progress", SuccessCriteria: "inputs understood"},
		{Step: "Check first condition", Status: "pending", SuccessCriteria: "first condition passes", VerificationRequired: true},
		{Step: "Check second condition", Status: "pending", SuccessCriteria: "second condition passes", VerificationRequired: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{
		Criterion: "first condition passes", Target: "input.txt",
	}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil || binding.StepID != plan.Plan.Steps[1].StepID || binding.Version != 3 {
		t.Fatalf("binding=%+v, want earliest pending obligation %s", binding, plan.Plan.Steps[1].StepID)
	}
}

func TestVerificationReplacementPreservesOriginalStepWhileLaterStepIsActive(t *testing.T) {
	ctx := context.Background()
	s, identity, _, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "check each input", []RunPlanStepInput{
		{Step: "Check value seven", Status: "in_progress", SuccessCriteria: "setting is positive", VerificationRequired: true},
		{Step: "Check value eight", Status: "pending", SuccessCriteria: "setting remains positive", VerificationRequired: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := []string{"/workspace/setting.txt"}
	first, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{
		Criterion: "setting is positive", Target: "setting.txt", LocalDependencies: &deps,
	}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	add := func(id string, b *verification.Binding, at int64) {
		t.Helper()
		raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{"kind": "verification", "tool_name": "verify", "tool_call_id": id, "status": "succeeded", "started_at_unix_nano": at, "finished_at_unix_nano": at + 1, "command": map[string]interface{}{"command": "check setting", "cwd": "/workspace", "binding": b}}})
		if _, appendErr := s.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: raw}); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	add("first", first, 10)
	advanced, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "input changed", []RunPlanStepInput{
		{StepID: plan.Plan.Steps[0].StepID, Step: plan.Plan.Steps[0].Step, Status: "completed"},
		{StepID: plan.Plan.Steps[1].StepID, Step: plan.Plan.Steps[1].Step, Status: "in_progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	recheck, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{
		Replaces: "first", Reason: "the input changed; recheck the same positive-value condition",
	}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if recheck.StepID != advanced.Plan.Steps[0].StepID || recheck.Criterion != first.Criterion || recheck.Target != first.Target {
		t.Fatalf("replacement identity=%+v", recheck)
	}
	add("second", recheck, 20)
	secondObligation, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if secondObligation == nil || secondObligation.StepID != advanced.Plan.Steps[1].StepID {
		t.Fatalf("active obligation identity=%+v", secondObligation)
	}
	add("third", secondObligation, 30)
	if _, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "both checked", []RunPlanStepInput{
		{StepID: advanced.Plan.Steps[0].StepID, Step: advanced.Plan.Steps[0].Step, Status: "completed"},
		{StepID: advanced.Plan.Steps[1].StepID, Step: advanced.Plan.Steps[1].Step, Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestVerificationReplacementBindsToActiveStepAfterOriginalWorkUnitCloses(t *testing.T) {
	ctx := context.Background()
	s, identity, _, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "check before and after change", []RunPlanStepInput{
		{Step: "Check original input", Status: "in_progress", SuccessCriteria: "setting is positive", VerificationRequired: true, WorkUnit: true},
		{Step: "Change and recheck input", Status: "pending", SuccessCriteria: "changed setting remains positive", VerificationRequired: true, WorkUnit: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	deps := []string{"/workspace/setting.txt"}
	first, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{
		Criterion: "setting is positive", Target: "setting.txt", LocalDependencies: &deps,
	}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	add := func(id string, binding *verification.Binding, at int64) {
		t.Helper()
		raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
			"kind": "verification", "tool_name": "verify", "tool_call_id": id, "status": "succeeded",
			"started_at_unix_nano": at, "finished_at_unix_nano": at + 1,
			"command": map[string]interface{}{"command": "check setting", "cwd": "/workspace", "binding": binding},
		}})
		if _, appendErr := s.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: raw}); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	add("original", first, 10)
	advanced, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "first unit closed", []RunPlanStepInput{
		{StepID: plan.Plan.Steps[0].StepID, Status: "completed"},
		{StepID: plan.Plan.Steps[1].StepID, Status: "in_progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	recheck, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{
		Replaces: "original", Reason: "the declared input changed; observe the same condition again",
	}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if recheck.StepID != advanced.Plan.Steps[1].StepID || recheck.Criterion != first.Criterion || recheck.Target != first.Target {
		t.Fatalf("closed-unit replacement identity=%+v", recheck)
	}
	add("recheck", recheck, 20)
	if _, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "both units checked", []RunPlanStepInput{
		{StepID: advanced.Plan.Steps[0].StepID, Status: "completed"},
		{StepID: advanced.Plan.Steps[1].StepID, Status: "completed"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestVerificationReplacementBindsAfterOriginalStepIsReplannedAway(t *testing.T) {
	ctx := context.Background()
	s, identity, _, run := newRecoveryFixture(t)
	initial, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "original check", []RunPlanStepInput{{
		Step: "Check obsolete route", Status: "in_progress", SuccessCriteria: "route is reachable", VerificationRequired: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{
		Criterion: "route is reachable", Target: "route",
	}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{
		"kind": "verification", "tool_name": "verify", "tool_call_id": "old-route", "status": "failed",
		"started_at_unix_nano": 10, "finished_at_unix_nano": 11,
		"command": map[string]interface{}{"command": "check old route", "cwd": "/workspace", "binding": old},
	}})
	if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}

	replanned, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "replace obsolete step", []RunPlanStepInput{{
		Step: "Check replacement route", Status: "in_progress", SuccessCriteria: "route is reachable", VerificationRequired: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if replanned.Plan.Steps[0].StepID == initial.Plan.Steps[0].StepID {
		t.Fatal("replanned step unexpectedly retained the removed step id")
	}

	replacement, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{
		Replaces: "old-route", Reason: "the obsolete route was replaced; check the same reachability condition",
	}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.StepID != replanned.Plan.Steps[0].StepID || replacement.Criterion != old.Criterion || replacement.Target != old.Target {
		t.Fatalf("replacement identity=%+v", replacement)
	}
}

// A check recorded while another required step is in_progress belongs to that
// step, whatever its criterion says. The rejection names the missing binding
// and its next action, and following that action closes the plan.
func TestPlanCloseOutRejectionNamesTheBindingAction(t *testing.T) {
	ctx := context.Background()
	s, identity, _, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "release", []RunPlanStepInput{
		{Step: "Check the build", Status: "in_progress", SuccessCriteria: "build succeeded", VerificationRequired: true},
		{Step: "Write the release record", Status: "pending", SuccessCriteria: "record lists the build", VerificationRequired: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	build, record := plan.Plan.Steps[0], plan.Plan.Steps[1]
	resolve := func(b verification.Binding) *verification.Binding {
		t.Helper()
		resolved, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, b, "/workspace")
		if err != nil || resolved == nil {
			t.Fatalf("binding=%+v err=%v", resolved, err)
		}
		return resolved
	}
	appendPlanEvidence(t, s, run.ID, "build-pass", resolve(verification.Binding{Criterion: "build succeeded", Target: "build 7"}), 10)
	early := resolve(verification.Binding{Criterion: "record lists the build", Target: "record.md"})
	if early.StepID != build.StepID {
		t.Fatalf("check bound outside the in_progress step: %+v", early)
	}
	appendPlanEvidence(t, s, run.ID, "record-early", early, 20)
	complete := []RunPlanStepInput{
		{StepID: build.StepID, Step: build.Step, Status: "completed"},
		{StepID: record.StepID, Step: record.Step, Status: "completed"},
	}
	_, err = s.SyncRunPlan(ctx, identity.TenantID, run.ID, "finish", complete)
	if err == nil || !strings.Contains(err.Error(), `step "Write the release record": criterion "record lists the build"; no check is bound to this step; next: make it the in_progress step with update_plan, then run verify`) ||
		strings.Contains(err.Error(), `step "Check the build"`) {
		t.Fatalf("rejection did not name the binding action: %v", err)
	}
	if _, err = s.SyncRunPlan(ctx, identity.TenantID, run.ID, "record", []RunPlanStepInput{
		{StepID: build.StepID, Step: build.Step, Status: "completed"},
		{StepID: record.StepID, Step: record.Step, Status: "in_progress"},
	}); err != nil {
		t.Fatal(err)
	}
	bound := resolve(verification.Binding{Criterion: "record lists the build", Target: "record.md"})
	if bound.StepID != record.StepID {
		t.Fatalf("check did not bind to the in_progress step: %+v", bound)
	}
	appendPlanEvidence(t, s, run.ID, "record-pass", bound, 30)
	if _, err = s.SyncRunPlan(ctx, identity.TenantID, run.ID, "finish", complete); err != nil {
		t.Fatal(err)
	}
}

// A later local write makes an undeclared external check stale. Its step keeps
// the check, so the rejection asks for a replacement rather than a new binding.
func TestPlanCloseOutRejectionRechecksStaleBoundCheck(t *testing.T) {
	ctx := context.Background()
	s, identity, _, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, identity.TenantID, run.ID, "release", []RunPlanStepInput{
		{Step: "Check the deployment", Status: "in_progress", SuccessCriteria: "deployment is healthy", VerificationRequired: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	step := plan.Plan.Steps[0]
	first, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{Criterion: "deployment is healthy", Target: "web"}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	appendPlanEvidence(t, s, run.ID, "deploy-pass", first, 10)
	write, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{"kind": "mutation", "tool_name": "write_file", "tool_call_id": "record-write", "status": "succeeded", "finished_at_unix_nano": 20,
		"files": []map[string]interface{}{{"path": "/workspace/record.md", "before_sha256": "", "after_sha256": "written"}}}})
	if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, Type: "evidence.recorded", Payload: write}); err != nil {
		t.Fatal(err)
	}
	complete := []RunPlanStepInput{{StepID: step.StepID, Step: step.Step, Status: "completed"}}
	_, err = s.SyncRunPlan(ctx, identity.TenantID, run.ID, "finish", complete)
	if err == nil || !strings.Contains(err.Error(), "deploy-pass") ||
		!strings.Contains(err.Error(), "verification stale; Verification exists, but it ran before the latest file change.") ||
		!strings.Contains(err.Error(), "next: recheck with verify replaces=<evidence id> and a reason, which stays bound to this step; declare local_dependencies: [] only when the check reads no local files") ||
		strings.Contains(err.Error(), "no check is bound") {
		t.Fatalf("stale check did not name its replacement action: %v", err)
	}
	none := []string{}
	recheck, err := s.ResolveVerificationBinding(ctx, identity.TenantID, run.ID, verification.Binding{Replaces: "deploy-pass", Reason: "the health check reads only the cluster", LocalDependencies: &none}, "/workspace")
	if err != nil || recheck == nil || recheck.StepID != step.StepID {
		t.Fatalf("replacement=%+v err=%v", recheck, err)
	}
	appendPlanEvidence(t, s, run.ID, "deploy-recheck", recheck, 30)
	if _, err = s.SyncRunPlan(ctx, identity.TenantID, run.ID, "finish", complete); err != nil {
		t.Fatal(err)
	}
}

func appendPlanEvidence(t *testing.T, s *Store, runID, id string, b *verification.Binding, at int64) {
	t.Helper()
	raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{"kind": "verification", "tool_name": "verify", "tool_call_id": id, "status": "succeeded", "started_at_unix_nano": at, "finished_at_unix_nano": at + 1,
		"command": map[string]interface{}{"command": "check", "cwd": "/workspace", "binding": b}}})
	if _, err := s.AppendEvent(context.Background(), Event{RunID: runID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}
}
