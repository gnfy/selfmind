package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// receiptScriptedProvider plays a model that answers each call from `script`, with
// the plan receipts it was shown so far. It records every update_plan result
// the model saw, in order, and every argument object it sent.
type receiptScriptedProvider struct {
	submittedFinalProvider
	mu       sync.Mutex
	script   func(n int32, receipt []map[string]interface{}) []llm.ToolCall
	observe  func(llm.ChatRequest)
	received []string // update_plan results, as shown to the model before call n+2
	sent     []string // update_plan arguments, in call order
}

func (p *receiptScriptedProvider) StreamChat(_ context.Context, request llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	if p.observe != nil {
		p.observe(request)
	}
	n := p.calls.Add(1)
	var receipt []map[string]interface{}
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Name != "update_plan" {
			continue
		}
		p.mu.Lock()
		p.received = append(p.received, request.Messages[i].Content)
		p.mu.Unlock()
		break
	}
	// The latest accepted receipt: the newest update_plan message that is a plan.
	for i := len(request.Messages) - 1; i >= 0 && receipt == nil; i-- {
		if request.Messages[i].Name != "update_plan" {
			continue
		}
		var result struct {
			Plan []map[string]interface{} `json:"plan"`
		}
		if json.Unmarshal([]byte(request.Messages[i].Content), &result) == nil && len(result.Plan) > 0 {
			receipt = result.Plan
		}
	}
	calls := p.script(n, receipt)
	p.mu.Lock()
	for _, call := range calls {
		if call.Function == "update_plan" {
			p.sent = append(p.sent, call.Args)
		}
	}
	p.mu.Unlock()
	out := make(chan llm.StreamEvent, 1)
	out <- llm.StreamEvent{ToolCalls: calls}
	close(out)
	return out, nil
}

func planCall(n int32, entries ...map[string]interface{}) llm.ToolCall {
	args, _ := json.Marshal(map[string]interface{}{"plan": entries})
	return llm.ToolCall{ID: fmt.Sprintf("plan-%d", n), Function: "update_plan", Args: string(args)}
}

func finishCall() llm.ToolCall {
	args, _ := json.Marshal(map[string]interface{}{"status": "done", "summary": "Source inspected", "final_response": submittedFinalBody})
	return llm.ToolCall{ID: "finish", Function: "finish_run", Args: string(args)}
}

type planFlowDaemon struct {
	daemon *Server
	store  *control.Store
	owner  *control.IdentityContext
	ws     *control.Workspace
}

func newPlanFlowDaemon(t *testing.T, provider llm.Provider) planFlowDaemon {
	t.Helper()
	store := controltest.NewStore(t)
	registry := tools.NewRegistry()
	registry.UseResultMiddleware(tools.EvidenceMiddleware())
	dispatcher := tools.NewDispatcherWithRegistry(registry)
	dispatcher.RegisterTool(tools.NewReadFileTool())
	dispatcher.RegisterTool(tools.NewBatchReadTool(dispatcher.Dispatch, dispatcher.ToolExecutionMetadata))
	dispatcher.RegisterTool(tools.NewUpdatePlanTool())
	dispatcher.RegisterTool(tools.NewFinishRunTool())
	dispatcher.InjectMiddleware(tools.WorkspaceScopeMiddleware())
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test", 8, 1, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), ExecutionRuntime: newTestExecutionRuntime(t), DefaultTenantID: "default"}
	owner, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	ws := humanWaitWorkspace(t, store, owner, "plan-flow")
	if err := os.WriteFile(filepath.Join(ws.LocalPath, "input.txt"), []byte("READY"), 0600); err != nil {
		t.Fatal(err)
	}
	return planFlowDaemon{daemon: daemon, store: store, owner: owner, ws: ws}
}

func (d planFlowDaemon) run(t *testing.T) api.MessageResponse {
	t.Helper()
	response, code := d.daemon.ProcessMessage(context.Background(), api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "plan-flow", WorkspaceID: d.ws.ID,
		Content: "Inspect input.txt and report the scope", ApprovalMode: "full-auto",
	})
	if code != 200 || response.Content != submittedFinalBody {
		t.Fatalf("scripted run did not complete: %d %+v", code, response)
	}
	return response
}

func (d planFlowDaemon) count(t *testing.T, taskID, eventType, contains string) int {
	t.Helper()
	events, err := d.store.ListTaskEvents(context.Background(), taskID, 300)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, event := range events {
		if event.Type == eventType && strings.Contains(string(event.Payload), contains) {
			n++
		}
	}
	return n
}

// The planned steps every flow below starts from.
func firstSnapshot(n int32) llm.ToolCall {
	return planCall(n,
		map[string]interface{}{"step": "Inspect sources", "status": "in_progress", "success_criteria": "Source was read"},
		map[string]interface{}{"step": "Review the scope", "status": "pending", "success_criteria": "Scope is understood"})
}

// A model that sends only ids and statuses for unchanged steps, as the update
// contract now asks, keeps every runtime guarantee: the receipt it reads back
// is complete, a progress update still shares its response with a later read,
// and the plan the runtime stores is unchanged by what the model left out.
func TestCompactPlanInputKeepsTheCompleteReceiptAndBoundaryOfARealRun(t *testing.T) {
	provider := &receiptScriptedProvider{}
	provider.script = func(n int32, receipt []map[string]interface{}) []llm.ToolCall {
		ids := func() (string, string) { return receipt[0]["step_id"].(string), receipt[1]["step_id"].(string) }
		switch n {
		case 1:
			return []llm.ToolCall{firstSnapshot(n)}
		case 2:
			a, b := ids()
			return []llm.ToolCall{
				planCall(n, map[string]interface{}{"step_id": a, "status": "in_progress"}, map[string]interface{}{"step_id": b, "status": "completed"}),
				{ID: "read", Function: "read_file", Args: `{"path":"input.txt"}`},
			}
		default:
			a, b := ids()
			return []llm.ToolCall{
				planCall(n, map[string]interface{}{"step_id": a, "status": "completed"}, map[string]interface{}{"step_id": b, "status": "completed"}),
				finishCall(),
			}
		}
	}
	flow := newPlanFlowDaemon(t, provider)
	response := flow.run(t)
	if provider.calls.Load() != 3 {
		t.Fatalf("model calls = %d, want 3", provider.calls.Load())
	}
	// The read in the compact progress update's response ran, and nothing was held.
	if reads := flow.count(t, response.Task.ID, "tool.started", `"tool":"read_file"`); reads != 1 {
		t.Fatalf("a compact progress update discarded or replayed its sibling read: %d", reads)
	}
	if held := flow.count(t, response.Task.ID, "tool.completed", "plan_boundary_held"); held != 0 {
		t.Fatalf("a compact update was read as a changed boundary: %d held calls", held)
	}
	// Every receipt the model read back after a compact update is the full plan.
	if len(provider.received) < 2 {
		t.Fatalf("receipts seen: %d", len(provider.received))
	}
	for i, content := range provider.received {
		var receipt struct {
			Plan         []map[string]interface{} `json:"plan"`
			WorkUnits    []map[string]interface{} `json:"work_units"`
			PlanVersion  int                      `json:"plan_version"`
			ClosureFacts *struct {
				Available   bool     `json:"available"`
				PlanVersion int      `json:"plan_version"`
				OpenStepIDs []string `json:"open_step_ids"`
			} `json:"closure_facts"`
		}
		if err := json.Unmarshal([]byte(content), &receipt); err != nil || len(receipt.Plan) != 2 {
			t.Fatalf("receipt %d is not a complete plan: %v\n%s", i, err, content)
		}
		for _, entry := range receipt.Plan {
			for _, field := range []string{"step_id", "step", "status", "success_criteria"} {
				if text, _ := entry[field].(string); strings.TrimSpace(text) == "" {
					t.Fatalf("receipt %d lost %s after compact input: %v", i, field, entry)
				}
			}
		}
		if len(receipt.WorkUnits) == 0 {
			t.Fatalf("receipt %d lost its work-unit goal: %s", i, content)
		}
		if receipt.ClosureFacts == nil || !receipt.ClosureFacts.Available || receipt.ClosureFacts.PlanVersion != receipt.PlanVersion {
			t.Fatalf("model did not receive current closure facts: %s", content)
		}
		var open []string
		for _, step := range receipt.Plan {
			if step["status"] == "pending" || step["status"] == "in_progress" {
				open = append(open, step["step_id"].(string))
			}
		}
		if strings.Join(open, ",") != strings.Join(receipt.ClosureFacts.OpenStepIDs, ",") {
			t.Fatalf("closure facts disagree with accepted Plan: %s", content)
		}
	}
	plan, err := flow.store.LatestRunPlan(context.Background(), flow.owner.TenantID, response.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Inspect sources": "Source was read", "Review the scope": "Scope is understood"}
	for _, step := range plan.Steps {
		if step.Status != "completed" || want[step.Step] != step.SuccessCriteria {
			t.Fatalf("what the model left out was not preserved: %+v", step)
		}
	}
	// And the report sees what the contract asks for: two compact updates, none restated.
	report, err := flow.daemon.dailyQualityReport(context.Background(), flow.owner, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "across 2 updates; 1 first snapshots and 0 unreadable inputs excluded") || !strings.Contains(report, "Plan input restating stored fields: 0% of plan argument characters") {
		t.Fatalf("report does not show compact input:\n%s", report)
	}
}

// Two refused update_plan calls and the correction that follows, as a real run
// records them: the refusals tell the model what to fix, and the daily report
// reads the same events as failed steps, an immediate same-tool retry, and the
// share of plan input that only repeated stored text.
func TestRefusedPlanCallsReachTheModelAndTheDailyReportOfARealRun(t *testing.T) {
	provider := &receiptScriptedProvider{}
	provider.script = func(n int32, receipt []map[string]interface{}) []llm.ToolCall {
		var a, b string
		if receipt != nil {
			a, b = receipt[0]["step_id"].(string), receipt[1]["step_id"].(string)
		}
		switch n {
		case 1:
			return []llm.ToolCall{firstSnapshot(n)}
		case 2: // `criterion` is not a plan field; success_criteria is
			return []llm.ToolCall{planCall(n, map[string]interface{}{"step_id": a, "status": "in_progress", "criterion": "Source was read"}, map[string]interface{}{"step_id": b, "status": "pending"})}
		case 3: // an id the run never issued
			return []llm.ToolCall{planCall(n, map[string]interface{}{"step_id": a, "status": "in_progress"}, map[string]interface{}{"step_id": "step_made_up", "status": "pending"})}
		case 4: // accepted; repeats the first step's text
			return []llm.ToolCall{planCall(n, map[string]interface{}{"step_id": a, "status": "in_progress", "step": "Inspect sources"}, map[string]interface{}{"step_id": b, "status": "pending"})}
		default: // accepted; repeats the second step's criterion, then finishes
			return []llm.ToolCall{
				planCall(n, map[string]interface{}{"step_id": a, "status": "completed"}, map[string]interface{}{"step_id": b, "status": "completed", "success_criteria": "Scope is understood"}),
				finishCall(),
			}
		}
	}
	flow := newPlanFlowDaemon(t, provider)
	response := flow.run(t)
	if provider.calls.Load() != 5 {
		t.Fatalf("model calls = %d, want 5", provider.calls.Load())
	}
	plan, err := flow.store.LatestRunPlan(context.Background(), flow.owner.TenantID, response.Run.ID)
	if err != nil || len(plan.Steps) != 2 {
		t.Fatalf("durable plan: %+v %v", plan, err)
	}
	a, b := plan.Steps[0].StepID, plan.Steps[1].StepID

	// What the model was shown after each refusal.
	if len(provider.received) < 4 {
		t.Fatalf("results shown to the model: %d", len(provider.received))
	}
	unknown, stale := provider.received[1], provider.received[2]
	for _, want := range []string{"unknown parameter: plan[0].criterion", "did you mean success_criteria?"} {
		if !strings.Contains(unknown, want) {
			t.Errorf("the unknown-field refusal lacks %q:\n%s", want, unknown)
		}
	}
	for _, want := range []string{
		`plan[1].step_id "step_made_up" is stale for the current run.`,
		fmt.Sprintf(`Current steps (step_id [status] step): %s [in_progress] "Inspect sources"; %s [pending] "Review the scope".`, a, b),
	} {
		if !strings.Contains(stale, want) {
			t.Errorf("the stale-id refusal lacks %q:\n%s", want, stale)
		}
	}

	report, err := flow.daemon.dailyQualityReport(context.Background(), flow.owner, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Failed model steps: 2 (",
		"contract 2 (",
		"retried with the same tool at once: 1 more steps (",
		"Failed steps by tool: update_plan 2 (contract 2; retried 1; ",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("daily report lacks %q:\n%s", want, report)
		}
	}
	// The refused-before-dispatch call has no start event; the stale-id call, the
	// repeated step text and the repeated criterion do.
	characters := len(a) + len("in_progress") + len("step_made_up") + len("pending") + // call 3
		len(a) + len("in_progress") + len("Inspect sources") + len(b) + len("pending") + // call 4
		len(a) + len("completed") + len(b) + len("completed") + len("Scope is understood") // call 5
	restated := len("Inspect sources") + len("Scope is understood")
	want := fmt.Sprintf("Plan input restating stored fields: %d%% of plan argument characters (%d of %d; 2 of 6 entries) across 3 updates; 1 first snapshots and 0 unreadable inputs excluded",
		restated*100/characters, restated, characters)
	if !strings.Contains(report, want) {
		t.Errorf("plan input line: want %q in:\n%s", want, report)
	}
}

// A model that answers a first plan with id-only entries, as the compact-update
// wording once led it to, gets one refusal that says what is true (the run has
// no plan, so ids are ignored) and what to send, and the correction goes through.
// The refusal is a contract failure in the daily report, not an unclassified one.
func TestFirstPlanWithoutStepTextIsRefusedWithItsRepairInARealRun(t *testing.T) {
	provider := &receiptScriptedProvider{}
	provider.script = func(n int32, receipt []map[string]interface{}) []llm.ToolCall {
		switch n {
		case 1: // ids the model made up, and no text for the first two steps
			return []llm.ToolCall{planCall(n,
				map[string]interface{}{"step_id": "s1", "status": "completed"},
				map[string]interface{}{"step_id": "s2", "status": "in_progress", "success_criteria": "Source was read"},
				map[string]interface{}{"step": "Review the scope", "status": "pending", "success_criteria": "Scope is understood"})}
		case 2:
			return []llm.ToolCall{firstSnapshot(n)}
		default: // ids now exist: compact updates are what the contract asks for
			a, b := receipt[0]["step_id"].(string), receipt[1]["step_id"].(string)
			return []llm.ToolCall{
				planCall(n, map[string]interface{}{"step_id": a, "status": "completed"}, map[string]interface{}{"step_id": b, "status": "completed"}),
				finishCall(),
			}
		}
	}
	flow := newPlanFlowDaemon(t, provider)
	response := flow.run(t)
	if provider.calls.Load() != 3 {
		t.Fatalf("model calls = %d, want 3", provider.calls.Load())
	}
	if len(provider.received) < 1 {
		t.Fatal("the model was never shown the refusal")
	}
	for _, want := range []string{
		"plan[0].step is required: this run has no plan yet",
		"ids are ignored in a first plan",
		"Send the complete plan again: every step with its text, and without step_id.",
	} {
		if !strings.Contains(provider.received[0], want) {
			t.Errorf("the refusal the model saw lacks %q:\n%s", want, provider.received[0])
		}
	}
	if strings.Contains(provider.received[0], "error_code:") || strings.Contains(provider.received[0], "error_class:") {
		t.Errorf("model refusal leaked the internal recovery protocol: %s", provider.received[0])
	}
	events, err := flow.store.ListTaskEvents(context.Background(), response.Task.ID, 300)
	if err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, event := range events {
		if event.Type == "tool.completed" && strings.Contains(string(event.Payload), `"error_code":"plan_step_text_required"`) {
			refused++
			if !strings.Contains(string(event.Payload), `"error_category":"invalid_input"`) {
				t.Errorf("the refusal is not recorded as invalid_input: %s", event.Payload)
			}
		}
	}
	if refused != 1 {
		t.Fatalf("recorded refusals = %d, want 1", refused)
	}
	plan, err := flow.store.LatestRunPlan(context.Background(), flow.owner.TenantID, response.Run.ID)
	if err != nil || len(plan.Steps) != 2 || plan.Steps[0].Step != "Inspect sources" || plan.Steps[1].Status != "completed" {
		t.Fatalf("the corrected plan was not stored and completed: %+v %v", plan, err)
	}
	report, err := flow.daemon.dailyQualityReport(context.Background(), flow.owner, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Failed model steps: 1 (",
		"contract 1 (",
		"retried with the same tool at once: 1 more steps (",
		"Failed steps by tool: update_plan 1 (contract 1; retried 1;",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("daily report lacks %q:\n%s", want, report)
		}
	}
}
