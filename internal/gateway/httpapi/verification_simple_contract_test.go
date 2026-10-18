package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

// Exercise the published middleware with the production durable resolver:
// shorter input inherits established identity, never a guessed target.
func TestMinimalVerificationKeepsDurableObligation(t *testing.T) {
	ctx := context.Background()
	daemon, store, identity := newTaskViewServer(t)
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Check output", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "check output")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "check", []control.RunPlanStepInput{{Step: "Check output", Status: "in_progress", SuccessCriteria: "output is complete", VerificationRequired: true}})
	if err != nil {
		t.Fatal(err)
	}
	stepID, cwd := plan.Plan.Steps[0].StepID, t.TempDir()
	ctx = tools.WithRunPlanProjection(ctx, daemon.coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{}))
	calls := 0
	exec := tools.EvidenceMiddleware()(func(args map[string]interface{}) (kernel.ToolDispatchResult, error) {
		calls++
		code := 0
		var err error
		if args["command"] == "first method" {
			code, err = 1, errors.New("check exited 1")
		}
		return kernel.ToolDispatchResult{Process: &kernel.ToolProcessResult{Started: true, ExitCode: &code}}, err
	})
	check := func(id string, fields map[string]interface{}) (kernel.ToolDispatchResult, error) {
		t.Helper()
		args := map[string]interface{}{"_context": ctx, "_tool_name": "verify", "_tool_call_id": id, "command": "corrected method", "cwd": cwd}
		for key, value := range fields {
			args[key] = value
		}
		result, err := exec(args)
		for _, evidence := range result.Evidence {
			raw, _ := json.Marshal(map[string]interface{}{"evidence": evidence})
			if _, saveErr := store.AppendEvent(ctx, control.Event{RunID: run.ID, Type: "evidence.recorded", Payload: raw}); saveErr != nil {
				t.Fatal(saveErr)
			}
		}
		return result, err
	}
	first, err := check("first", map[string]interface{}{"command": "first method", "step_id": stepID})
	if err == nil || len(first.Evidence) != 1 || first.Evidence[0].Command.Binding.Criterion != "output is complete" {
		t.Fatalf("minimal Plan check lost its condition: %+v / %v", first, err)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err == nil {
		t.Fatal("a failed check completed the work")
	}
	for _, changed := range []map[string]interface{}{
		{"criterion": "weaker condition"}, {"target": "another artifact"}, {"cwd": t.TempDir()}, {"step_id": "foreign-step"},
	} {
		changed["replaces"], changed["reason"] = "first", "correct the method"
		result, err := check("invalid", changed)
		if err == nil || calls != 1 || result.Invoked == nil || *result.Invoked || len(result.Evidence) != 0 {
			t.Fatalf("changed identity reached execution: %+v / %v", result, err)
		}
	}
	corrected, err := check("corrected", map[string]interface{}{"replaces": "first", "reason": "correct the method"})
	if err != nil || calls != 2 || len(corrected.Evidence) != 1 {
		t.Fatalf("minimal correction failed: %+v / %v", corrected, err)
	}
	binding := corrected.Evidence[0].Command.Binding
	if binding.StepID != stepID || binding.Criterion != "output is complete" || binding.Target != first.Evidence[0].Command.Binding.Target || binding.Replaces != "first" {
		t.Fatalf("correction changed its obligation: %+v", binding)
	}
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "checked", []control.RunPlanStepInput{{StepID: stepID, Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateRunCompletion(ctx, identity.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
}
