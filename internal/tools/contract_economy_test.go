package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFinishContractRequiresExplicitStatusAndOneBody(t *testing.T) {
	tool := NewFinishRunTool()
	if _, err := tool.Execute(map[string]interface{}{"summary": "finished"}); err == nil {
		t.Fatal("direct execution inferred done from missing status")
	}
	registry := NewRegistry()
	registry.Register(tool)
	body := "配置已更新。\n\n检查通过；仍需用户确认部署。"
	for _, status := range []string{"done", "waiting_user", "blocked"} {
		result, err := registry.DispatchResult("finish_run", map[string]interface{}{"status": status, "final_response": body})
		if err != nil {
			t.Fatalf("body-only %s: %v", status, err)
		}
		var receipt map[string]interface{}
		if err := json.Unmarshal([]byte(result.Output), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt["final_response"] != body || receipt["summary"] == "" || receipt["status"] != status {
			t.Fatalf("lost explicit status or body: %#v", receipt)
		}
	}
	for _, args := range []map[string]interface{}{
		{"status": "done"}, {"status": "done", "summary": " ", "final_response": "\n"},
	} {
		if _, err := registry.DispatchResult("finish_run", args); err == nil {
			t.Fatalf("accepted empty outcome: %v", args)
		}
	}
	projection := &durablePlanProjectionTest{completionErr: errors.New("unresolved evidence")}
	if _, err := tool.Execute(map[string]interface{}{"status": "done", "final_response": body, "_context": WithRunPlanProjection(context.Background(), projection)}); err == nil {
		t.Fatal("body-only outcome bypassed completion authority")
	}
	long := strings.Repeat("完整结果。", 200)
	raw, err := tool.Execute(map[string]interface{}{"status": "blocked", "final_response": long})
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]interface{}
	_ = json.Unmarshal([]byte(raw), &receipt)
	if receipt["final_response"] != long || len([]rune(receipt["summary"].(string))) > 240 {
		t.Fatal("summary bound altered full body or exceeded display budget")
	}
}

func TestArgumentRefusalIsBoundedAndDoesNotExecute(t *testing.T) {
	registry := NewRegistry()
	probe := &resultProbeTool{BaseTool: BaseTool{name: "closed", schema: ToolSchema{Type: "object", AdditionalProperties: rejectAdditionalProperties()}}}
	registry.Register(probe)
	args := map[string]interface{}{}
	for i := 0; i < 100; i++ {
		args[fmt.Sprintf("wrong_%03d", i)] = true
	}
	_, err := registry.DispatchResult("closed", args)
	if err == nil || probe.calls != 0 || strings.Count(err.Error(), "unknown parameter:") != 8 {
		t.Fatalf("unbounded or invoked refusal: %v called=%v", err, probe.calls)
	}
}

func TestArgumentRefusalCollectsIndependentProblemsBeforeDispatch(t *testing.T) {
	registry := NewRegistry()
	registry.Register(NewUpdatePlanTool())
	args := map[string]interface{}{"unexpected": true, "plan": []interface{}{
		map[string]interface{}{"step": "inspect", "typo": true},
		map[string]interface{}{"verification_required": []interface{}{}, "status": "invented"},
	}}
	before, _ := json.Marshal(args)
	var first string
	for i := 0; i < 10; i++ {
		_, err := registry.PrepareToolArguments("update_plan", args)
		if err == nil {
			t.Fatal("invalid call accepted")
		}
		for _, want := range []string{"unknown parameter: unexpected", "missing required parameter: plan[0].status", "unknown parameter: plan[0].typo", "parameter plan[1].verification_required must be a boolean", "parameter plan[1].status must be one of"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("missing %q: %v", want, err)
			}
		}
		if i > 0 && err.Error() != first {
			t.Fatal("refusal depends on map iteration order")
		}
		first = err.Error()
	}
	after, _ := json.Marshal(args)
	if string(before) != string(after) {
		t.Fatal("argument errors silently repaired the call")
	}
}

func TestPlanPatchCannotFallBackToAnUnversionedSnapshotSink(t *testing.T) {
	calls := 0
	ctx := WithPlanProjectionSink(context.Background(), func(context.Context, []PlanStep) ([]PlanWorkUnitIdentity, error) { calls++; return nil, nil })
	for _, ctx := range []context.Context{context.Background(), ctx} {
		registry := NewRegistry()
		registry.Register(NewUpdatePlanTool())
		if _, err := registry.DispatchResult("update_plan", map[string]interface{}{"_context": ctx, "mode": "patch", "plan": []interface{}{map[string]interface{}{"step_id": "known", "step": "retained", "status": "completed"}}}); err == nil {
			t.Fatal("unversioned adapter interpreted a patch as the complete Plan")
		}
	}
	if calls != 0 {
		t.Fatal("unsupported patch reached the legacy sink")
	}
}
