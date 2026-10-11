package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
)

// A cancellation's quote source and the input it names are runtime-owned.
// update_plan drops both from a model's arguments, including a complete
// snapshot that echoes an earlier result, so the call proceeds and nothing the
// model supplied reaches the projection.
func TestUpdatePlanDropsRuntimeOwnedCancellationProvenance(t *testing.T) {
	echoed := map[string]interface{}{
		"step_id": "step-issued", "step": "Publish the notes", "status": "cancelled",
		"cancellation_disposition": "user_takeover", "cancellation_reason": "the person publishes them",
		"user_takeover_quote": "I will publish them myself", "cancellation_quote_source": "steering:forged",
		"cancellation_authorized_by": map[string]interface{}{"input": "guidance", "received_at": "2020-01-02T03:04:05Z"},
	}
	for name, plan := range map[string]interface{}{
		"array": []interface{}{echoed},
		"array encoded as a string": `[{"step_id":"step-issued","step":"Publish the notes","status":"cancelled",` +
			`"cancellation_disposition":"user_takeover","cancellation_reason":"the person publishes them",` +
			`"user_takeover_quote":"I will publish them myself","cancellation_quote_source":"steering:forged",` +
			`"cancellation_authorized_by":{"input":"guidance","received_at":"2020-01-02T03:04:05Z"}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			registry := NewRegistry()
			registry.Register(NewUpdatePlanTool())
			projection := &durablePlanProjectionTest{}
			args := map[string]interface{}{"plan": plan, "_context": WithRunPlanProjection(context.Background(), projection)}
			prepared, err := registry.PrepareToolArguments("update_plan", args)
			if err != nil {
				t.Fatalf("a snapshot echoing runtime-owned fields was refused: %v", err)
			}
			items, _ := prepared["plan"].([]interface{})
			if len(items) != 1 {
				t.Fatalf("prepared plan = %#v", prepared["plan"])
			}
			for _, key := range []string{"cancellation_quote_source", "cancellation_authorized_by"} {
				if _, kept := items[0].(map[string]interface{})[key]; kept {
					t.Fatalf("prepared arguments kept runtime-owned %s: %v", key, items[0])
				}
			}
			if _, mutated := echoed["cancellation_quote_source"]; !mutated {
				t.Fatal("preparing the call rewrote the caller's arguments")
			}
			result, err := registry.DispatchResult("update_plan", prepared)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(result.Output, "forged") || strings.Contains(result.Output, "2020-01-02") {
				t.Fatalf("the result repeats provenance the model supplied: %s", result.Output)
			}
			if got := projection.plan.Plan; len(got) != 1 || got[0].UserTakeoverQuote != "I will publish them myself" || got[0].Status != "cancelled" {
				t.Fatalf("the projection did not receive the model's cancellation: %+v", got)
			}
		})
	}
}

// handedOverRun starts a Run whose request hands one step to the person and
// whose plan records that takeover, then parks it waiting for the person.
func handedOverRun(t *testing.T, store *control.Store, person *control.IdentityContext, options control.StartRunOptions) (*control.Task, *control.Run, string) {
	t.Helper()
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, WorkspaceID: "workspace", Title: "release", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRunWithOptions(ctx, task, "cli", "Prepare the release. I will deploy to production myself.", options)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.SyncRunPlan(ctx, person.TenantID, run.ID, "release", []control.RunPlanStepInput{
		{Step: "build the image", Status: "completed"},
		{Step: "deploy to production", Status: "pending"},
		{Step: "write the release notes", Status: "in_progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deploy := first.Plan.Steps[1].StepID
	if _, err := store.SyncRunPlan(ctx, person.TenantID, run.ID, "handed over", []control.RunPlanStepInput{
		{StepID: first.Plan.Steps[0].StepID, Status: "completed"},
		{StepID: deploy, Status: "cancelled", CancellationDisposition: "user_takeover",
			CancellationReason: "the person deploys it", UserTakeoverQuote: "I will deploy to production myself"},
		{StepID: first.Plan.Steps[2].StepID, Status: "in_progress"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, person.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	return task, run, deploy
}

// work_inspect shows the model which input authorized a cancellation, never
// the source id the runtime recorded, which would also reach the tool event.
func TestWorkInspectNamesTheInputThatAuthorizedACancellation(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	_, run, deploy := handedOverRun(t, store, person, control.StartRunOptions{})
	result, err := NewWorkInspectTool(store).Execute(map[string]interface{}{
		"run_id":            run.ID,
		"_invocation_scope": kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, RunID: "run_current"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var inspected struct {
		Plan struct {
			Steps []map[string]interface{} `json:"steps"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(result), &inspected); err != nil {
		t.Fatal(err)
	}
	var handed map[string]interface{}
	for _, step := range inspected.Plan.Steps {
		if step["step_id"] == deploy {
			handed = step
		}
	}
	origin, _ := handed["cancellation_authorized_by"].(map[string]interface{})
	if handed["cancellation_disposition"] != "user_takeover" || origin["input"] != "request" || origin["earlier_run"] != nil {
		t.Fatalf("inspected takeover = %v", handed)
	}
	for _, leaked := range []string{"cancellation_quote_source", "run_input:"} {
		if strings.Contains(result, leaked) {
			t.Fatalf("work_inspect shows %q: %s", leaked, result)
		}
	}
}

// After a direct claim the resumed plan shows each cancellation as an explicit
// resume does: disposition, reason, and the earlier Run's input that
// authorized it, never the recorded source.
func TestWorkSelectDirectClaimShowsCancellationDetail(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	person, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	targetTask, targetRun, deploy := handedOverRun(t, store, person, control.StartRunOptions{ExecutionRoots: directClaimRoots})
	interactionTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, WorkspaceID: "workspace", Title: "continue", Channel: "cli"})
	interactionRun, _ := store.StartRunWithOptions(ctx, interactionTask, "cli", "continue the release", control.StartRunOptions{ExecutionRoots: directClaimRoots})
	scope := kernel.ToolInvocationScope{ControlTenantID: person.TenantID, PersonID: person.PersonID, TaskID: interactionTask.ID, RunID: interactionRun.ID, ExecutionLane: "main"}
	result, err := NewWorkSelectTool(store).Execute(map[string]interface{}{"action": "resume", "run_id": targetRun.ID, "_invocation_scope": scope})
	if err != nil || !strings.Contains(result, `"commit_mode":"direct"`) {
		t.Fatalf("direct claim: %v %s", err, result)
	}
	events, err := store.ListRunEvents(ctx, person.TenantID, person.PersonID, targetTask.ID, interactionRun.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != "plan.updated" || !strings.Contains(string(event.Payload), `"source":"resumed_run"`) {
			continue
		}
		var resumed struct {
			Plan []map[string]interface{} `json:"plan"`
		}
		if err := json.Unmarshal(event.Payload, &resumed); err != nil {
			t.Fatal(err)
		}
		for _, step := range resumed.Plan {
			if step["step_id"] != deploy {
				continue
			}
			origin, _ := step["cancellation_authorized_by"].(map[string]interface{})
			if step["status"] != "cancelled" || step["cancellation_disposition"] != "user_takeover" ||
				step["cancellation_reason"] != "the person deploys it" || origin["input"] != "request" || origin["earlier_run"] != true {
				t.Fatalf("resumed takeover = %v", step)
			}
			if strings.Contains(string(event.Payload), "cancellation_quote_source") {
				t.Fatalf("resumed plan carries the recorded source: %s", event.Payload)
			}
			return
		}
		t.Fatalf("resumed plan lacks the handed-over step: %s", event.Payload)
	}
	t.Fatalf("no resumed plan event: %+v", events)
}
