package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVCRPlanStepIDsFollowCurrentRun(t *testing.T) {
	recorded := "step_11111111-1111-4111-8111-111111111111"
	current := "step_22222222-2222-4222-8222-222222222222"
	path := filepath.Join(t.TempDir(), "0000.json")
	provider := &vcrProvider{}
	request := func(id string) []Message {
		return []Message{{Role: "tool", Name: "update_plan", Content: `{"plan":[{"step_id":"` + id + `"}]}`}}
	}
	provider.save(context.Background(), path, cassette{Method: "stream", Events: []recordedEvent{{ToolCalls: []ToolCall{{ID: "call", Function: "update_plan", Args: `{"plan":[{"step_id":"` + recorded + `","status":"completed"}]}`}}}}}, request(recorded))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), recorded) {
		t.Fatal("cassette retained a step id from the recording run")
	}
	got, err := provider.load(context.Background(), path, request(current))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Events[0].ToolCalls[0].Args, current) {
		t.Fatalf("step was not rebound: %+v", got)
	}
}

func TestCassettesCarryNoVolatilePlanStepIDs(t *testing.T) {
	for _, file := range vcrCorpusFiles(t) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if id := vcrPlanStepIDPattern.Find(raw); len(id) > 0 {
			t.Errorf("%s contains recording-run plan step %q; re-record with request-aware placeholders", file, id)
		}
	}
}

func TestVCRCurrentPlanBindingSurvivesAgedHistoricalSteps(t *testing.T) {
	old := "step_11111111-1111-4111-8111-111111111111"
	recorded := "step_22222222-2222-4222-8222-222222222222"
	current := "step_33333333-3333-4333-8333-333333333333"
	request := func(id string) Message {
		return Message{Role: "tool", Name: "update_plan", Content: `{"plan":[{"step_id":"` + id + `"}]}`}
	}
	path := filepath.Join(t.TempDir(), "0000.json")
	provider := &vcrProvider{}
	provider.save(context.Background(), path, cassette{Method: "stream", Events: []recordedEvent{{ToolCalls: []ToolCall{{ID: "finish-plan", Function: "update_plan", Args: `{"plan":[{"step_id":"` + recorded + `","status":"completed"}]}`}}}}}, []Message{request(old), request(recorded)})
	replay, err := provider.load(context.Background(), path, []Message{request(current)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replay.Events[0].ToolCalls[0].Args, current) {
		t.Fatalf("aged historical ID shifted current plan binding: %+v", replay)
	}
}
