package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
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
	}
	p := collectDailyPerformance(events)
	if p.First.Calls != 1 || p.First.Read != 20 || p.First.Unavailable != 1 || p.Later.Read != 90 || p.ExpansionMS != 3000 || p.RecallMS != 3020 || p.PreparationMS != 3050 {
		t.Fatalf("stats=%+v", p)
	}
	for _, want := range []string{"first 20%", "later 90%", "timeout 1", "skipped_exact_replay 1", "historical missing markers excluded"} {
		if !strings.Contains(p.render(), want) {
			t.Fatalf("missing %q in %s", want, p.render())
		}
	}
}

// The live agent.step event is never stored, so these counters must come from
// the durable tool.completed facts of each Run's own model steps.
func TestPerformanceReportRebuildsToolStepsFromDurableEvents(t *testing.T) {
	usage := func(run string, delegated bool) control.Event {
		return control.Event{RunID: run, Type: "provider.call.usage", Payload: []byte(fmt.Sprintf(`{"role":"coding_agent","input_tokens":10,"delegated":%t}`, delegated))}
	}
	done := func(run, tool, extra string) control.Event {
		return control.Event{RunID: run, Type: "tool.completed", Payload: []byte(fmt.Sprintf(`{"tool":%q%s}`, tool, extra))}
	}
	held := `,"invoked":false,"error_code":"plan_boundary_held"`
	events := []control.Event{
		usage("a", false),
		done("a", "update_plan", `,"invoked":true`), // step 1: Plan only
		// A second Run interleaves; one of its results lacks the dispatcher fact, so the
		// whole step is reported as unclassifiable instead of guessed.
		usage("b", false),
		done("b", "update_plan", `,"invoked":true`),
		done("b", "finish_run", ""),
		usage("a", false),
		done("a", "update_plan", `,"invoked":true`), // step 2: the sibling was held, the Plan update still ran
		done("a", "read_file", `,"invoked":true,"delegated":true`),
		done("a", "read_file", held),
		usage("b", false),
		usage("a", false),
		done("a", "update_plan", `,"invoked":true`), // step 3: a Skill read is real work
		usage("a", true),                            // a sub-agent's model call does not end the Run's step
		done("a", "skill_view", `,"invoked":true`),
		done("a", "read_file", `,"nested":true,"parent_tool":"batch_read"`), // child of one batch_read call
		usage("a", false),
		done("a", "read_file", `,"invoked":false,"error_code":"tool_budget_exhausted"`), // refused, not held by the Plan boundary
		usage("a", false),
		done("a", "update_plan", `,"invoked":true`), // final step has no later model call
		done("a", "finish_run", `,"invoked":true`),
		{Type: "tool.completed", Payload: []byte(`{"tool":"update_plan","invoked":true}`)}, // no Run: not a step
	}
	got := collectDailyPerformance(events)
	if got.BookkeepingSteps != 3 || got.HeldCalls != 1 || got.UnreportedToolSteps != 1 {
		t.Fatalf("durable step facts were misread: %+v", got)
	}
	line := "Bookkeeping-only model steps: 3 (invoked update_plan/finish_run; Skill reads are not bookkeeping); held calls: 1; dispatch metadata missing: 1 steps"
	if !strings.Contains(got.render(), line) {
		t.Fatalf("report line changed:\n%s", got.render())
	}
}

type heldPlanProvider struct{ submittedFinalProvider }

// Four model steps: a Plan, a Plan whose changed boundary holds its sibling,
// one real read, then the final Plan with finish_run.
func (p *heldPlanProvider) StreamChat(_ context.Context, request llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	n := p.calls.Add(1)
	plan := []map[string]interface{}{{"step": "Inspect sources", "status": "in_progress", "success_criteria": "Source was read"}, {"step": "Review the reported scope", "status": "pending", "success_criteria": "The requested scope is understood"}}
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Name != "update_plan" {
			continue
		}
		var result struct {
			Plan []map[string]interface{} `json:"plan"`
		}
		if json.Unmarshal([]byte(request.Messages[i].Content), &result) == nil {
			plan = result.Plan
		}
		break
	}
	var calls []llm.ToolCall
	switch n {
	case 2:
		plan[0]["status"], plan[1]["status"] = "completed", "in_progress"
		// A genuine scope/text change still holds work; mere progress now does not.
		plan[1]["step"] = "Review the updated reported scope"
	case 4:
		plan[0]["status"], plan[1]["status"] = "completed", "completed"
	}
	if n != 3 {
		args, _ := json.Marshal(map[string]interface{}{"plan": plan})
		calls = append(calls, llm.ToolCall{ID: fmt.Sprintf("plan-%d", n), Function: "update_plan", Args: string(args)})
	}
	if n == 2 || n == 3 {
		calls = append(calls, llm.ToolCall{ID: fmt.Sprintf("read-%d", n), Function: "read_file", Args: `{"path":"input.txt"}`})
	}
	if n == 4 {
		args, _ := json.Marshal(map[string]interface{}{"status": "done", "summary": "Source inspected", "final_response": submittedFinalBody})
		calls = append(calls, llm.ToolCall{ID: "finish", Function: "finish_run", Args: string(args)})
	}
	out := make(chan llm.StreamEvent, 1)
	out <- llm.StreamEvent{ToolCalls: calls}
	close(out)
	return out, nil
}

// The same real run, read back through the daily report path: events are
// stored by the daemon, listed per person, then counted.
func TestDailyReportCountsBookkeepingAndHeldCallsOfARealRun(t *testing.T) {
	store := controltest.NewStore(t)
	provider := &heldPlanProvider{}
	dispatcher := tools.NewDispatcherWithRegistry(tools.NewRegistry())
	dispatcher.RegisterTool(tools.NewReadFileTool())
	dispatcher.RegisterTool(tools.NewUpdatePlanTool())
	dispatcher.RegisterTool(tools.NewFinishRunTool())
	dispatcher.InjectMiddleware(tools.WorkspaceScopeMiddleware())
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test", 8, 1, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), ExecutionRuntime: newTestExecutionRuntime(t), DefaultTenantID: "default"}
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	ws := humanWaitWorkspace(t, store, owner, "held")
	if err := os.WriteFile(filepath.Join(ws.LocalPath, "input.txt"), []byte("READY"), 0600); err != nil {
		t.Fatal(err)
	}
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "held", WorkspaceID: ws.ID, Content: "Inspect input.txt and report the scope", ApprovalMode: "full-auto"})
	if code != 200 || response.Content != submittedFinalBody || provider.calls.Load() != 4 {
		t.Fatalf("scripted run: %d %+v calls=%d", code, response, provider.calls.Load())
	}
	events, err := store.ListTaskEvents(ctx, response.Task.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	var heldCompletions, readStarts int
	for _, event := range events {
		switch {
		case event.Type == "tool.completed" && strings.Contains(string(event.Payload), `"error_code":"plan_boundary_held"`):
			heldCompletions++
		case event.Type == "tool.started" && strings.Contains(string(event.Payload), `"tool":"read_file"`):
			readStarts++
		}
	}
	if heldCompletions != 1 || readStarts != 1 {
		t.Fatalf("the sibling of the changed Plan was not held exactly once: held=%d dispatched reads=%d", heldCompletions, readStarts)
	}
	report, err := daemon.dailyQualityReport(ctx, owner, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := "Bookkeeping-only model steps: 3 (invoked update_plan/finish_run; Skill reads are not bookkeeping); held calls: 1; dispatch metadata missing: 0 steps"
	if !strings.Contains(report, want) {
		t.Fatalf("daily report lost the step facts of a real run; want %q in:\n%s", want, report)
	}
}
