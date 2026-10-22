package control

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"selfmind/internal/verification"
)

func TestCompletionAdoptsObservedFactsWithoutInventingExecution(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		required   bool
		version    int
		exit       bool
		want       bool
	}{
		{"executed check", "command", true, 1, true, true},
		{"file inspection", "file", false, 1, false, true},
		{"file cannot prove execution", "file", true, 1, false, false},
		{"missing exit", "command", true, 1, false, false},
		{"historical observation stays inert", "command", true, 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, owner, task, run := newRecoveryFixture(t)
			plan, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Inspect input", Status: "in_progress", SuccessCriteria: "input satisfies the request", VerificationRequired: tc.required}})
			if err != nil {
				t.Fatal(err)
			}
			fact := map[string]interface{}{"version": tc.version, "tool_call_id": "observed", "tool_name": "terminal", "kind": "command", "status": "succeeded", "invoked": true, "started_at_unix_nano": 10, "finished_at_unix_nano": 11}
			if tc.kind == "file" {
				fact["kind"] = "observation"
				fact["tool_name"] = "read_file"
				fact["observation"] = verification.Observation{Kind: "file", Target: "/workspace/input.txt"}
			} else {
				fact["command"] = map[string]interface{}{"command": "check input", "cwd": "/workspace", "exit_code": 0}
				if tc.exit {
					fact["process"] = map[string]interface{}{"started": true, "exit_code": 0}
				}
			}
			raw, _ := json.Marshal(map[string]interface{}{"evidence": fact})
			if _, err = s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
				t.Fatal(err)
			}
			input := RunCompletionInput{ExpectedPlanVersion: plan.Plan.Version, Status: "done", PlanPatch: []RunPlanStepInput{{StepID: plan.Plan.Steps[0].StepID, Status: "completed"}}, Assessments: []verification.Assessment{{StepID: plan.Plan.Steps[0].StepID, EvidenceRefs: []string{"observed"}, Result: "met", Reason: "The observed output supports this condition."}}}
			candidate, err := s.PrepareRunCompletion(ctx, owner.TenantID, owner.PersonID, run.ID, input)
			if (err == nil) != tc.want {
				t.Fatalf("candidate=%+v err=%v want=%v", candidate, err, tc.want)
			}
			if !tc.want {
				return
			}
			_, err = s.MaterializeRunFinalization(ctx, RunFinalization{Identity: *owner, TaskID: task.ID, RunID: run.ID, RunStatus: "done", Completion: candidate, AssistantContent: "Inspected.", Event: Event{Type: "run.finished", Payload: []byte(`{"outcome":{"status":"done"}}`)}})
			if err != nil {
				t.Fatal(err)
			}
			var checks int
			if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events WHERE run_id=? AND type='evidence.recorded' AND json_extract(payload_json,'$.evidence.kind')='verification'`, run.ID).Scan(&checks); err != nil || checks != 0 {
				t.Fatal("adoption invented a verification execution")
			}
			projection, err := s.ResolveConditionAssessments(ctx, owner.TenantID, run.ID, nil)
			if err != nil || !projection.ReviewComplete {
				t.Fatalf("accepted observation lost its durable binding: %+v %v", projection, err)
			}
		})
	}
}

func TestObservationAdoptionRejectsChangedInputsAndUnconfirmedFacts(t *testing.T) {
	for _, scenario := range []string{"changed input", "not invoked", "unknown effect", "outside window"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, owner, task, run := newRecoveryFixture(t)
			fact := map[string]interface{}{"version": 1, "tool_call_id": "source", "tool_name": "terminal", "kind": "command", "status": "succeeded", "invoked": true, "started_at_unix_nano": 10, "finished_at_unix_nano": 11, "command": map[string]interface{}{"command": "check input", "cwd": "/workspace", "exit_code": 0}, "process": map[string]interface{}{"started": true, "exit_code": 0}}
			appendFact := func() {
				raw, _ := json.Marshal(map[string]interface{}{"evidence": fact})
				if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Inspect", Status: "in_progress", SuccessCriteria: "input satisfies request", VerificationRequired: scenario != "outside window"}})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "not invoked":
				fact["invoked"] = false
			case "unknown effect":
				fact["effect_state"] = "unknown"
			}
			if scenario == "outside window" {
				plan, err = s.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []RunPlanStepInput{{StepID: plan.Plan.Steps[0].StepID, Status: "completed"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			appendFact()
			if scenario == "changed input" {
				raw := []byte(`{"evidence":{"tool_call_id":"edit","tool_name":"write_file","kind":"mutation","status":"succeeded","started_at_unix_nano":12,"finished_at_unix_nano":13,"files":[{"path":"/workspace/input.txt","operation":"write","before_sha256":"old","after_sha256":"new"}]}}`)
				if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
					t.Fatal(err)
				}
			}
			input := RunCompletionInput{Status: "done", ExpectedPlanVersion: plan.Plan.Version, PlanPatch: []RunPlanStepInput{{StepID: plan.Plan.Steps[0].StepID, Status: "completed"}}, Assessments: []verification.Assessment{{StepID: plan.Plan.Steps[0].StepID, EvidenceRefs: []string{"source"}, Result: "met", Reason: "Observed condition"}}}
			if _, err := s.PrepareRunCompletion(ctx, owner.TenantID, owner.PersonID, run.ID, input); err == nil {
				t.Fatal("invalid observation satisfied completion")
			}
			var count int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events WHERE run_id=? AND type='evidence.adopted'`, run.ID).Scan(&count); err != nil || count != 0 {
				t.Fatal("rejected preview leaked an association", err)
			}
		})
	}
}

func TestOneObservationCanSupportIndependentStepsInTheSameWorkUnit(t *testing.T) {
	ctx := context.Background()
	s, owner, task, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Inspect source", Status: "in_progress", SuccessCriteria: "know contents"}, {Step: "Report finding", Status: "pending", SuccessCriteria: "report contents"}})
	if err != nil {
		t.Fatal(err)
	}
	fact := map[string]interface{}{"version": 1, "tool_call_id": "read-source", "tool_name": "read_file", "kind": "observation", "status": "succeeded", "invoked": true, "started_at_unix_nano": 10, "finished_at_unix_nano": 11, "observation": verification.Observation{Kind: "file", Target: "/workspace/source.txt"}}
	raw, _ := json.Marshal(map[string]interface{}{"evidence": fact})
	if _, err = s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	input := RunCompletionInput{Status: "done", ExpectedPlanVersion: plan.Plan.Version}
	for _, step := range plan.Plan.Steps {
		input.PlanPatch = append(input.PlanPatch, RunPlanStepInput{StepID: step.StepID, Status: "completed"})
		input.Assessments = append(input.Assessments, verification.Assessment{StepID: step.StepID, EvidenceRefs: []string{"read-source"}, Result: "met", Reason: "Observation supports this condition"})
	}
	candidate, err := s.PrepareRunCompletion(ctx, owner.TenantID, owner.PersonID, run.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.MaterializeRunFinalization(ctx, RunFinalization{Identity: *owner, TaskID: task.ID, RunID: run.ID, RunStatus: "done", Completion: candidate, AssistantContent: "Inspected.", Event: Event{Type: "run.finished", Payload: []byte(`{"outcome":{"status":"done"}}`)}}); err != nil {
		t.Fatal(err)
	}
	projection, err := s.ResolveConditionAssessments(ctx, owner.TenantID, run.ID, nil)
	if err != nil || len(projection.Conditions) != 2 || !projection.ReviewComplete {
		t.Fatalf("shared source lost one binding: %+v %v", projection, err)
	}
}

func TestFailedExecutedObservationCanExplainAnUnfinishedRequiredCondition(t *testing.T) {
	ctx := context.Background()
	s, owner, task, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Required check", Status: "in_progress", SuccessCriteria: "check passes", VerificationRequired: true}})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"evidence":{"version":1,"tool_call_id":"failed-check","tool_name":"terminal","kind":"command","status":"failed","invoked":true,"started_at_unix_nano":10,"finished_at_unix_nano":11,"command":{"command":"check input","cwd":"/workspace","exit_code":1},"process":{"started":true,"exit_code":1}}}`)
	if _, err = s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	input := RunCompletionInput{Status: "blocked", ExpectedPlanVersion: plan.Plan.Version, PlanPatch: []RunPlanStepInput{{StepID: plan.Plan.Steps[0].StepID, Status: "pending"}}, Assessments: []verification.Assessment{{StepID: plan.Plan.Steps[0].StepID, EvidenceRefs: []string{"failed-check"}, Result: "not_met", Reason: "Observed check failure leaves required remediation open"}}}
	candidate, err := s.PrepareRunCompletion(ctx, owner.TenantID, owner.PersonID, run.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.MaterializeRunFinalization(ctx, RunFinalization{Identity: *owner, TaskID: task.ID, RunID: run.ID, RunStatus: "blocked", Completion: candidate, AssistantContent: "Required check failed; remediation remains.", Event: Event{Type: "run.finished", Payload: []byte(`{"outcome":{"status":"blocked"}}`)}}); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.LatestRunPlan(ctx, owner.TenantID, run.ID)
	if saved.Steps[0].Status != "pending" {
		t.Fatal("negative finding erased unfinished work")
	}
	// Older finalizers omitted this cursor. Read-back must use the terminal
	// receipt rather than absorb a later successful observation into it.
	if _, err := s.db.ExecContext(ctx, `UPDATE run_work_units SET finished_cursor=0 WHERE run_id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	late := []byte(`{"evidence":{"version":1,"tool_call_id":"later","tool_name":"terminal","kind":"command","status":"succeeded","invoked":true,"started_at_unix_nano":20,"finished_at_unix_nano":21,"command":{"command":"later unrelated check","exit_code":0},"process":{"started":true,"exit_code":0}}}`)
	if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: late}); err != nil {
		t.Fatal(err)
	}
	state, err := s.ResolveConditionAssessments(ctx, owner.TenantID, run.ID, nil)
	if err != nil || state.State != "not_met" || len(state.Blockers) == 0 {
		units, _ := s.ListRunWorkUnits(ctx, owner.TenantID, run.ID)
		t.Fatalf("negative finding lost: %+v %v; units=%+v; plan=%+v", state, err, units, saved)
	}
}

func TestOneConditionCanUseSeveralFileObservationsWithoutLosingTheirScopes(t *testing.T) {
	ctx := context.Background()
	s, owner, task, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "", []RunPlanStepInput{{Step: "Review both sources", Status: "in_progress", SuccessCriteria: "Both requested sources support the finding"}})
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range []string{"/workspace/source-a.txt", "/workspace/source-b.txt"} {
		dependencies := []string{path}
		fact := map[string]interface{}{"version": 1, "tool_call_id": fmt.Sprintf("batch:batch:%d", i+1), "tool_name": "read_file", "kind": "observation", "status": "succeeded", "invoked": true, "started_at_unix_nano": 10 + i, "finished_at_unix_nano": 11 + i, "observation": verification.Observation{Kind: "file", Target: path, LocalDependencies: &dependencies, ResolvedLocalDependencies: dependencies}}
		raw, _ := json.Marshal(map[string]interface{}{"evidence": fact})
		if _, err = s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}
	stepID := plan.Plan.Steps[0].StepID
	candidate, err := s.PrepareRunCompletion(ctx, owner.TenantID, owner.PersonID, run.ID, RunCompletionInput{ExpectedPlanVersion: plan.Plan.Version, Status: "done", PlanPatch: []RunPlanStepInput{{StepID: stepID, Status: "completed"}}, Assessments: []verification.Assessment{{StepID: stepID, EvidenceRefs: []string{"batch:batch:1", "batch:batch:2"}, Result: "met", Reason: "Both returned file contents support the requested finding."}}})
	if err != nil {
		t.Fatalf("one logical condition rejected distinct observed files: %v", err)
	}
	if _, err = s.MaterializeRunFinalization(ctx, RunFinalization{Identity: *owner, TaskID: task.ID, RunID: run.ID, RunStatus: "done", Completion: candidate, AssistantContent: "Reviewed both.", Event: Event{Type: "run.finished", Payload: []byte(`{"outcome":{"status":"done"}}`)}}); err != nil {
		t.Fatal(err)
	}
	checks, err := s.CurrentRunObservationChecks(ctx, owner.TenantID, run.ID)
	if err != nil || len(checks) != 2 {
		t.Fatalf("observed sources lost: %+v %v", checks, err)
	}
	for _, check := range checks {
		if check.Binding.Target != stepID || check.Observation.Target == stepID || check.Binding.LocalDependencies == nil || len(*check.Binding.LocalDependencies) != 1 || (*check.Binding.LocalDependencies)[0] != check.Observation.Target {
			t.Fatalf("logical and observed scopes collapsed: %+v", check)
		}
	}
}

func TestFileObservationAssociationVersionsKeepRecordedTargets(t *testing.T) {
	ctx := context.Background()
	s, _, task, run := newRecoveryFixture(t)
	const path = "/workspace/recorded.txt"
	raw, _ := json.Marshal(map[string]interface{}{"evidence": map[string]interface{}{"version": 1, "tool_call_id": "file-source", "tool_name": "read_file", "kind": "observation", "status": "succeeded", "invoked": true, "started_at_unix_nano": 10, "finished_at_unix_nano": 11, "observation": verification.Observation{Kind: "file", Target: path}}})
	event, err := s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, version := range []int{0, 1, 2, 3} {
		check, err := observationCheckTx(ctx, tx, run.ID, observationAdoption{Version: version, SourceCursor: event.Cursor, SourceCallID: "file-source", StepID: "logical-step", Criterion: "Review the source"})
		if version == 0 || version == 3 {
			if err == nil {
				t.Fatal("unsupported association version accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		want := path
		if version == 2 {
			want = "logical-step"
		}
		if check.Binding.Target != want || check.Observation.Target != path {
			t.Fatalf("v%d changed source/condition semantics: %+v", version, check)
		}
	}
}

func TestCompletionSkipsAuxiliaryObservationsOnVerifiedStepsAndNarrowsMixedFreshObservations(t *testing.T) {
	ctx := context.Background()
	s, owner, task, run := newRecoveryFixture(t)
	plan, err := s.SyncRunPlan(ctx, owner.TenantID, run.ID, "two steps", []RunPlanStepInput{
		{Step: "Inspect target", Status: "completed", SuccessCriteria: "Target file inspected", VerificationRequired: false},
		{Step: "Verify target", Status: "in_progress", SuccessCriteria: "Tests pass", VerificationRequired: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	step1 := plan.Plan.Steps[0].StepID
	step2 := plan.Plan.Steps[1].StepID

	// Record a terminal command (toolcall-1-0) and a fresh read_file (toolcall-2-0) before a mutation to unrelated.go.
	zero := 0
	rawTerm, _ := json.Marshal(map[string]interface{}{
		"evidence": map[string]interface{}{
			"version": 1, "tool_call_id": "toolcall-1-0", "tool_name": "terminal", "kind": "command",
			"status": "succeeded", "invoked": true, "started_at_unix_nano": 10, "finished_at_unix_nano": 15,
			"command": map[string]interface{}{"command": "git status", "cwd": "/workspace"},
			"process": map[string]interface{}{"started": true, "exit_code": &zero},
		},
	})
	if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: rawTerm}); err != nil {
		t.Fatal(err)
	}
	deps := []string{"/workspace/inspected.go"}
	rawRead, _ := json.Marshal(map[string]interface{}{
		"evidence": map[string]interface{}{
			"version": 1, "tool_call_id": "toolcall-2-0", "tool_name": "read_file", "kind": "observation",
			"status": "succeeded", "invoked": true, "started_at_unix_nano": 16, "finished_at_unix_nano": 18,
			"observation": verification.Observation{
				Kind:                      "file",
				Target:                    "/workspace/inspected.go",
				CWD:                       "/workspace",
				LocalDependencies:         &deps,
				ResolvedLocalDependencies: deps,
			},
		},
	})
	if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: rawRead}); err != nil {
		t.Fatal(err)
	}
	// Record a mutation to /workspace/other.go at t=25 (making toolcall-1-0 stale because terminal has nil LocalDependencies, while toolcall-2-0 stays fresh).
	rawMut := []byte(`{"evidence":{"tool_call_id":"toolcall-3-0","tool_name":"write_file","kind":"mutation","status":"succeeded","started_at_unix_nano":20,"finished_at_unix_nano":25,"files":[{"path":"/workspace/other.go","operation":"write","before_sha256":"old","after_sha256":"new"}]}}`)
	if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: rawMut}); err != nil {
		t.Fatal(err)
	}

	// ResolveVerificationBinding when replaces points to an observation (toolcall-1-0) with explicit step_id binds cleanly to step2.
	resolved, err := s.ResolveVerificationBinding(ctx, owner.TenantID, run.ID, verification.Binding{
		StepID:            step2,
		Replaces:          "toolcall-1-0",
		Reason:            "run verify on step2",
		LocalDependencies: &deps,
	}, "/workspace")
	if err != nil || resolved == nil || resolved.StepID != step2 || resolved.Replaces != "" {
		t.Fatalf("expected replaces on non-verify observation with explicit step_id to bind to step2, got %+v / %v", resolved, err)
	}

	// Record a passing verify check (toolcall-10-0) on step2 at t=30..35.
	resolved.ResolvedLocalDependencies = deps
	rawVerify, _ := json.Marshal(map[string]interface{}{
		"evidence": map[string]interface{}{
			"version": 1, "tool_call_id": "toolcall-10-0", "tool_name": "verify", "kind": "verification",
			"status": "succeeded", "invoked": true, "started_at_unix_nano": 30, "finished_at_unix_nano": 35,
			"command": map[string]interface{}{"command": "go test ./...", "cwd": "/workspace", "binding": resolved},
			"process": map[string]interface{}{"started": true, "exit_code": &zero},
		},
	})
	if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: rawVerify}); err != nil {
		t.Fatal(err)
	}

	// ResolveVerificationBinding when replaces points to a Plan-step check (toolcall-10-0) without step_id and cwd changes inherits step2 cleanly.
	resolvedDifferentCWD, err := s.ResolveVerificationBinding(ctx, owner.TenantID, run.ID, verification.Binding{
		Replaces: "toolcall-10-0",
		Reason:   "re-run step2 from repo subdirectory",
	}, "/workspace/sub")
	if err != nil || resolvedDifferentCWD == nil || resolvedDifferentCWD.StepID != step2 || resolvedDifferentCWD.Replaces != "" {
		t.Fatalf("expected replaces on Plan-step check with omitted step_id and changed cwd to inherit step2, got %+v / %v", resolvedDifferentCWD, err)
	}

	// PrepareRunCompletion where step1 cites [toolcall-1-0 (stale), toolcall-2-0 (fresh)], step2 cites [toolcall-1-0, toolcall-2-0, toolcall-10-0 (verify)],
	// and an unstepped caveat note cites a command observation (toolcall-1-0).
	candidate, err := s.PrepareRunCompletion(ctx, owner.TenantID, owner.PersonID, run.ID, RunCompletionInput{
		ExpectedPlanVersion: plan.Plan.Version,
		Status:              "done",
		PlanPatch:           []RunPlanStepInput{{StepID: step2, Status: "completed"}},
		Assessments: []verification.Assessment{
			{StepID: step1, EvidenceRefs: []string{"toolcall-1-0", "toolcall-2-0"}, Result: "met", Reason: "Inspected target file."},
			{StepID: step2, EvidenceRefs: []string{"toolcall-1-0", "toolcall-2-0", "toolcall-10-0"}, Result: "met", Reason: "Verified target."},
			{EvidenceRefs: []string{"toolcall-1-0"}, Result: "unknown", Reason: "Boundary note referencing command observation."},
		},
	})
	if err != nil || candidate == nil {
		t.Fatalf("PrepareRunCompletion rejected mixed fresh observations or auxiliary refs on verified step: %v", err)
	}

	// Non-done completion (waiting_user) drops unverified "met" or "unknown" assessments citing stale evidence instead of deadlocking.
	rawMut2 := []byte(`{"evidence":{"tool_call_id":"toolcall-11-0","tool_name":"write_file","kind":"mutation","status":"succeeded","started_at_unix_nano":40,"finished_at_unix_nano":45,"files":[{"path":"/workspace/inspected.go","operation":"write","before_sha256":"old","after_sha256":"new"}]}}`)
	if _, err := s.AppendEvent(ctx, Event{RunID: run.ID, TaskID: task.ID, Type: "evidence.recorded", Payload: rawMut2}); err != nil {
		t.Fatal(err)
	}
	waitingCandidate, err := s.PrepareRunCompletion(ctx, owner.TenantID, owner.PersonID, run.ID, RunCompletionInput{
		ExpectedPlanVersion: plan.Plan.Version,
		Status:              "waiting_user",
		Assessments: []verification.Assessment{
			{StepID: step1, EvidenceRefs: []string{"toolcall-2-0"}, Result: "unknown", Reason: "Step 1 read is now stale after mutation."},
			{StepID: step2, EvidenceRefs: []string{"toolcall-10-0"}, Result: "met", Reason: "Step 2 was verified before mutation."},
		},
	})
	if err != nil || waitingCandidate == nil {
		t.Fatalf("PrepareRunCompletion(waiting_user) deadlocked on stale assessment: %v", err)
	}
}
