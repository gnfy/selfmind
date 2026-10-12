package httpapi

import (
	"strings"
	"testing"

	"selfmind/internal/control"
)

func TestPerformanceReportUsesActualFirstCallsAndDeclaredCacheUsage(t *testing.T) {
	events := []control.Event{
		{Type: "provider.call.usage", Payload: []byte(`{"first_call":false,"status":"deferred","provider_dispatched":false,"input_tokens":100}`)},
		{Type: "provider.call.usage", Payload: []byte(`{"first_call":true,"iteration":0,"input_tokens":100,"cache_read_input_tokens":20,"cache_usage_reported":true}`)},
		{Type: "provider.call.usage", Payload: []byte(`{"first_call":false,"iteration":1,"input_tokens":100,"cache_read_input_tokens":90,"cache_usage_reported":true}`)},
		{Type: "provider.call.usage", Payload: []byte(`{"iteration":1,"input_tokens":10000,"cache_read_input_tokens":10000}`)},
		{Type: "provider.call.usage", Payload: []byte(`{"first_call":true,"iteration":47,"input_tokens":100,"cache_read_input_tokens":0}`)},
		{Type: "context.recall", Payload: []byte(`{"elapsed_ms":3010,"expansion_ms":3000,"expansion_status":"timeout"}`)},
		{Type: "context.recall", Payload: []byte(`{"elapsed_ms":10,"expansion_ms":0,"expansion_status":"skipped_exact_replay"}`)},
		{Type: "run.prepared", Payload: []byte(`{"duration_ms":3050}`)},
		{Type: "agent.step", Payload: []byte(`{"outcome":"execute_tools","detail":"update_plan,read_file","invoked_tools":["update_plan"],"held_tools":["read_file"]}`)},
		{Type: "agent.step", Payload: []byte(`{"outcome":"execute_tools","detail":"skill_view","invoked_tools":["skill_view"],"held_tools":[]}`)},
		{Type: "agent.step", Payload: []byte(`{"outcome":"execute_tools","detail":"read_file","invoked_tools":[],"held_tools":["read_file"]}`)},
		{Type: "agent.step", Payload: []byte(`{"outcome":"execute_tools","detail":"update_plan,finish_run"}`)},
	}
	p := collectDailyPerformance(events)
	if p.First.Calls != 1 || p.First.Read != 20 || p.First.Unavailable != 1 || p.Later.Read != 90 || p.BookkeepingSteps != 1 || p.ExpansionMS != 3000 || p.RecallMS != 3020 || p.PreparationMS != 3050 {
		t.Fatalf("stats=%+v", p)
	}
	for _, want := range []string{"first 20%", "later 90%", "timeout 1", "skipped_exact_replay 1", "historical missing markers excluded"} {
		if !strings.Contains(p.render(), want) {
			t.Fatalf("missing %q in %s", want, p.render())
		}
	}
	if p.HeldCalls != 2 || p.UnreportedToolSteps != 1 {
		t.Fatalf("held or historical dispatch facts were misclassified: %+v", p)
	}
}
