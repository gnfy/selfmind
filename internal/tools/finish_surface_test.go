package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestFinishAdvertisesOneBodyAndRetainsLegacyArguments(t *testing.T) {
	r := NewRegistry()
	r.Register(NewFinishRunTool())
	function := r.ToolDefinitions()[0]["function"].(map[string]interface{})
	schema := function["parameters"].(map[string]interface{})
	if fmt.Sprint(schema["required"]) != "[status final_response]" {
		t.Fatalf("model contract must require its complete reply: %v", schema["required"])
	}
	properties := schema["properties"].(map[string]interface{})
	for _, name := range []string{"summary", "done", "files", "tests", "need_approve"} {
		if _, exists := properties[name]; exists {
			t.Errorf("model still has to duplicate %s", name)
		}
	}
	for _, args := range []map[string]interface{}{
		{"status": "waiting_user", "summary": "Legacy handoff", "next_steps": []interface{}{"User confirms target"}},
		{"status": "done", "final_response": "Complete reply."},
	} {
		if _, err := r.DispatchResult("finish_run", args); err != nil {
			t.Fatalf("compatible finish refused: %v", err)
		}
	}
	if _, err := r.DispatchResult("finish_run", map[string]interface{}{"status": "done", "final_response": "Ready", "invented": true}); err == nil || !strings.Contains(err.Error(), "unknown parameter") {
		t.Fatalf("compatibility accepted an unknown field: %v", err)
	}
}

type completionSinkProbe struct{ calls int }

func (s *completionSinkProbe) CompleteRun(_ context.Context, out map[string]interface{}, patch []PlanStep) (bool, error) {
	s.calls++
	out["completion_reason"] = "completed"
	return true, nil
}

func TestFinalBodyUsesDurableCompletionWithoutAnArtificialPlanPatch(t *testing.T) {
	sink := &completionSinkProbe{}
	ctx := WithRunCompletionSink(context.Background(), sink)
	_, err := NewFinishRunTool().Execute(map[string]interface{}{"status": "done", "final_response": "Observed the requested result.", "_context": ctx})
	if err != nil || sink.calls != 1 {
		t.Fatalf("body-only completion calls=%d err=%v; the finalizer must not need a dummy Plan patch", sink.calls, err)
	}
}

func TestReleasedFinishBookkeepingRetainsCoordinatorCompatibility(t *testing.T) {
	for _, name := range []string{"summary", "done", "files", "tests", "need_approve"} {
		t.Run(name, func(t *testing.T) {
			sink := &completionSinkProbe{}
			args := map[string]interface{}{"status": "done", "final_response": "Complete reply.", "_context": WithRunCompletionSink(context.Background(), sink)}
			switch name {
			case "summary":
				args[name] = "Legacy summary"
			case "need_approve":
				args[name] = false
			default:
				args[name] = []interface{}{}
			}
			if _, err := NewFinishRunTool().Execute(args); err != nil || sink.calls != 0 {
				t.Fatalf("released payload changed finalization path: calls=%d err=%v", sink.calls, err)
			}
		})
	}
}
