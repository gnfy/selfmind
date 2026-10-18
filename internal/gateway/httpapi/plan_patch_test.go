package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/tools"
)

func TestPlanPatchPublishesCompleteAcceptedState(t *testing.T) {
	ctx := kernel.WithRunExecutionState(context.Background(), kernel.NewRunExecutionState())
	daemon, store, identity := newTaskViewServer(t)
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Inspect, change and report", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "Inspect, change and report")
	if err != nil {
		t.Fatal(err)
	}
	ctx = tools.WithRunPlanProjection(ctx, daemon.coordinator().newRunPlanProjection(identity, run, kernel.ToolInvocationScope{}))
	registry := tools.NewRegistry()
	registry.Register(tools.NewUpdatePlanTool())
	first, err := registry.DispatchResult("update_plan", map[string]interface{}{"_context": ctx, "plan": []interface{}{
		map[string]interface{}{"step": "Inspect the source", "status": "in_progress"},
		map[string]interface{}{"step": "Apply the change", "status": "pending", "success_criteria": "the requested change is present"},
		map[string]interface{}{"step": "Report the result", "status": "pending"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var initial struct {
		Plan    []tools.PlanStep `json:"plan"`
		Version int              `json:"plan_version"`
	}
	if err = json.Unmarshal([]byte(first.Output), &initial); err != nil || len(initial.Plan) != 3 {
		t.Fatalf("initial plan: %s %v", first.Output, err)
	}
	updated, err := registry.DispatchResult("update_plan", map[string]interface{}{"_context": ctx, "mode": "patch", "plan": []interface{}{
		map[string]interface{}{"step_id": initial.Plan[0].StepID, "status": "completed"},
		map[string]interface{}{"step_id": initial.Plan[1].StepID, "status": "in_progress"},
	}})
	if err != nil {
		t.Fatalf("cannot submit changed steps without restating the full Plan: %v", err)
	}
	var accepted struct {
		Plan    []tools.PlanStep `json:"plan"`
		Version int              `json:"plan_version"`
	}
	if err = json.Unmarshal([]byte(updated.Output), &accepted); err != nil || len(accepted.Plan) != 3 {
		t.Fatalf("incomplete accepted receipt: %s %v", updated.Output, err)
	}
	durable, err := store.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Version != initial.Version+1 || durable.Version != accepted.Version || len(durable.Steps) != 3 {
		t.Fatalf("version/state diverged: %+v %+v", accepted, durable)
	}
	for i, status := range []string{"completed", "in_progress", "pending"} {
		if accepted.Plan[i].Status != status || durable.Steps[i].Status != status || accepted.Plan[i].StepID != initial.Plan[i].StepID {
			t.Fatalf("step %d drifted: %+v", i, accepted)
		}
	}
	if accepted.Plan[1].SuccessCriteria != initial.Plan[1].SuccessCriteria || accepted.Plan[2].Step != initial.Plan[2].Step {
		t.Fatal("patch erased unchanged obligations")
	}
	// A concurrent accepted snapshot does not silently become this response's base.
	newer, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "other accepted change", []control.RunPlanStepInput{
		{StepID: initial.Plan[0].StepID, Status: "completed"},
		{StepID: initial.Plan[1].StepID, Status: "in_progress"},
		{StepID: initial.Plan[2].StepID, Status: "completed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.DispatchResult("update_plan", map[string]interface{}{"_context": ctx, "mode": "patch", "plan": []interface{}{map[string]interface{}{"step_id": initial.Plan[1].StepID, "status": "completed"}}})
	var coded interface{ ToolErrorCode() string }
	if !errors.As(err, &coded) || coded.ToolErrorCode() != "plan_version_changed" {
		t.Fatalf("stale runtime version was not refused: %v", err)
	}
	retained, err := store.LatestRunPlan(ctx, identity.TenantID, run.ID)
	if err != nil || retained.Version != newer.Plan.Version || retained.Steps[1].Status != "in_progress" {
		t.Fatal("stale patch changed current state")
	}

}

func TestPlanPatchAndSnapshotConvergeAcrossTheProductionMessagePath(t *testing.T) {
	for _, patch := range []bool{false, true} {
		name := "snapshot"
		if patch {
			name = "patch"
		}
		t.Run(name, func(t *testing.T) {
			provider := &receiptScriptedProvider{}
			provider.script = func(n int32, receipt []map[string]interface{}) []llm.ToolCall {
				switch n {
				case 1:
					return []llm.ToolCall{planCall(n,
						map[string]interface{}{"step": "Inspect sources", "status": "in_progress", "success_criteria": "Source was read"},
						map[string]interface{}{"step": "Review the scope", "status": "pending", "success_criteria": "Scope is understood"},
						map[string]interface{}{"step": "Report scope", "status": "pending"})}
				case 2:
					return []llm.ToolCall{{ID: "read-input", Function: "read_file", Args: `{"path":"input.txt"}`}}
				case 3:
					entries := []map[string]interface{}{{"step_id": receipt[0]["step_id"], "status": "completed"}, {"step_id": receipt[1]["step_id"], "status": "in_progress"}}
					if !patch {
						entries = append(entries, map[string]interface{}{"step_id": receipt[2]["step_id"], "status": "pending"})
					}
					return []llm.ToolCall{patchFlowCall(n, patch, entries), {ID: "review-input", Function: "read_file", Args: `{"path":"notes.txt"}`}}
				default:
					entries := []map[string]interface{}{{"step_id": receipt[1]["step_id"], "status": "completed"}, {"step_id": receipt[2]["step_id"], "status": "completed"}}
					if !patch {
						entries = append([]map[string]interface{}{{"step_id": receipt[0]["step_id"], "status": "completed"}}, entries...)
					}
					return []llm.ToolCall{patchFlowCall(n, patch, entries), finishCall()}
				}
			}
			flow := newPlanFlowDaemon(t, provider)
			if err := os.WriteFile(filepath.Join(flow.ws.LocalPath, "notes.txt"), []byte("Review scope note"), 0600); err != nil {
				t.Fatal(err)
			}
			response := flow.run(t)
			if provider.calls.Load() != 4 {
				t.Fatalf("extra reconciliation or final generation: %d model calls", provider.calls.Load())
			}
			if n := flow.count(t, response.Task.ID, "tool.started", `"tool":"read_file"`); n != 2 {
				t.Fatalf("progress held or repeated an independent read: %d", n)
			}
			events, err := flow.store.ListTaskEvents(context.Background(), response.Task.ID, 300)
			if err != nil {
				t.Fatal(err)
			}
			sort.Slice(events, func(i, j int) bool { return events[i].Cursor < events[j].Cursor })
			versions := 0
			for _, e := range events {
				if e.Type != "plan.updated" {
					continue
				}
				var p struct {
					Plan    []tools.PlanStep `json:"plan"`
					Version int              `json:"plan_version"`
				}
				if err = json.Unmarshal(e.Payload, &p); err != nil || len(p.Plan) != 3 {
					t.Fatalf("UI would receive a partial Plan: %s %v", e.Payload, err)
				}
				versions++
				if p.Version != versions {
					t.Fatalf("accepted versions diverged: %d after %d", p.Version, versions-1)
				}
			}
			if versions != 3 {
				t.Fatalf("visible progress missing: %d", versions)
			}
			p, err := flow.store.LatestRunPlan(context.Background(), flow.owner.TenantID, response.Run.ID)
			if err != nil || len(p.Steps) != 3 || p.Version != 3 {
				t.Fatalf("durable Plan differs: %+v %v", p, err)
			}
			for _, s := range p.Steps {
				if s.Status != "completed" {
					t.Fatal("finish ignored an untouched open step")
				}
			}
		})
	}
}

func patchFlowCall(n int32, patch bool, entries []map[string]interface{}) llm.ToolCall {
	args := map[string]interface{}{"plan": entries}
	if patch {
		args["mode"] = "patch"
	}
	raw, _ := json.Marshal(args)
	return llm.ToolCall{ID: fmt.Sprintf("plan-%d", n), Function: "update_plan", Args: string(raw)}
}
