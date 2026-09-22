package kernel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
)

func TestParseToolCallArgsStrictRejectsMalformedObject(t *testing.T) {
	if _, err := parseToolCallArgsStrict(`{"plan":[}`); err == nil || !strings.Contains(err.Error(), "valid JSON object") {
		t.Fatalf("malformed arguments were not diagnosed: %v", err)
	}
	if _, err := parseToolCallArgsStrict(`null`); err == nil {
		t.Fatal("null arguments were accepted as an empty invocation")
	}
	args, err := parseToolCallArgsStrict(`{"plan":[]}`)
	if err != nil || args["plan"] == nil {
		t.Fatalf("valid arguments changed: args=%v err=%v", args, err)
	}
}

func TestExternalWatchHandoffIsolatesLaterNonWatcherCalls(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "read", Function: "read_file"},
		{ID: "watch-1", Function: "watch_external"},
		{ID: "patch", Function: "patch"},
		{ID: "watch-2", Function: "watch_external"},
		{ID: "verify", Function: "verify"},
	}
	got, dropped := isolateExternalWatchHandoffCalls(calls)
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
	want := []string{"read_file", "watch_external", "watch_external"}
	if len(got) != len(want) {
		t.Fatalf("kept calls = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Function != want[i] {
			t.Fatalf("kept[%d] = %q, want %q", i, got[i].Function, want[i])
		}
	}
}

func TestLifecycleHandoffAcceptsOnlySuccessfulBuiltInWatcherResults(t *testing.T) {
	raw := `{"watch_id":"watch_1","registered":true,"message":"Watcher is running.","lifecycle_handoff":{"status":"waiting_external","summary":"Watching CI.","done":["Registered watcher."],"next_steps":["Wait for notification."]}}`
	handoff, ok := lifecycleHandoffFromToolResults([]toolExecutionResult{{
		toolName: "watch_external", rawResult: raw, success: true,
	}})
	if !ok || handoff.Status != "waiting_external" || handoff.Message != "Watcher is running." {
		t.Fatalf("handoff = %+v, ok=%v", handoff, ok)
	}

	for _, result := range []toolExecutionResult{
		{toolName: "mcp_watch_external", rawResult: raw, success: true},
		{toolName: "watch_external", rawResult: raw, success: false},
		{toolName: "watch_external", rawResult: `{"watch_id":"watch_1","registered":true,"message":"x","lifecycle_handoff":{"status":"done","summary":"x"}}`, success: true},
	} {
		if _, ok := lifecycleHandoffFromToolResults([]toolExecutionResult{result}); ok {
			t.Fatalf("untrusted or invalid handoff was accepted: %+v", result)
		}
	}
}

func TestModelVisibleSkillToolResultRemovesControlIdentity(t *testing.T) {
	raw := `{"success":true,"activation_id":"a1","work_unit_id":"wu","work_unit_sequence":2,"skill_key":"secret-key","name":"flow","version_hash":"secret-version","package_hash":"secret-package","instructions":"do it","linked_files":["references/a.md"],"delivery_mode":"full","delivered_main_hash":"secret-delivery","notice":"bounded"}`
	visible := modelVisibleSkillToolResult("skill_select", raw)
	for _, want := range []string{`"activation_id":"a1"`, `"name":"flow"`, `"instructions":"do it"`, `"delivery_mode":"full"`} {
		if !strings.Contains(visible, want) {
			t.Fatalf("model result lost %s: %s", want, visible)
		}
	}
	for _, hidden := range []string{"work_unit", "skill_key", "version_hash", "package_hash", "delivered_main_hash", "secret-"} {
		if strings.Contains(visible, hidden) {
			t.Fatalf("model result leaked %q: %s", hidden, visible)
		}
	}
}

type cancelledOutcomeLedger struct {
	recorded bool
	ctxErr   error
}

type capturingToolLedger struct{ entry ToolLedgerEntry }

func (l *capturingToolLedger) ClaimDispatch(_ context.Context, entry ToolLedgerEntry) (ToolDispatchDecision, error) {
	l.entry = entry
	return ToolDispatchDecision{Execute: true, Status: "started"}, nil
}

func (*capturingToolLedger) RecordOutcome(context.Context, string, string, bool) error { return nil }

type argumentPreparationFailure struct{}

func (argumentPreparationFailure) Error() string             { return "unknown parameter: content.options.extra" }
func (argumentPreparationFailure) ToolErrorCode() string     { return "tool_arguments_invalid" }
func (argumentPreparationFailure) ToolErrorCategory() string { return "invalid_input" }
func (argumentPreparationFailure) ModelSafeMessage() string {
	return "unknown parameter: content.options.extra"
}
func (argumentPreparationFailure) ToolRecoveryHint() string {
	return "Correct the arguments to match the published tool schema."
}
func (argumentPreparationFailure) ToolFailurePhase() string { return "preparation" }
func (argumentPreparationFailure) ToolRetryability() string { return "corrected_input" }
func (argumentPreparationFailure) ToolEffectState() string  { return "not_dispatched" }
func (argumentPreparationFailure) ToolStateChanged() bool   { return false }
func (argumentPreparationFailure) ToolAlternatives() []string {
	return []string{"inspect_tool_schema", "correct_arguments"}
}

type rejectingArgumentPreparerBackend struct {
	prepareCalls  int
	dispatchCalls int
	state         string
	acceptAll     bool
}

func (b *rejectingArgumentPreparerBackend) PrepareToolArguments(_ string, args map[string]interface{}) (map[string]interface{}, error) {
	b.prepareCalls++
	if valid, _ := args["valid"].(bool); valid || b.acceptAll {
		return args, nil
	}
	return nil, argumentPreparationFailure{}
}
func (b *rejectingArgumentPreparerBackend) Dispatch(string, map[string]interface{}) (string, error) {
	b.dispatchCalls++
	return "ok", nil
}
func (b *rejectingArgumentPreparerBackend) GetToolDefinitions() []map[string]interface{} { return nil }
func (b *rejectingArgumentPreparerBackend) ToolExecutionMetadata(string, map[string]interface{}) ToolExecutionMetadata {
	return ToolExecutionMetadata{Origin: "builtin", Category: "filesystem", RiskLevel: "high", ReadOnly: false}
}
func (b *rejectingArgumentPreparerBackend) ToolPreparationState(string) string { return b.state }

func TestArgumentPreparationRefusalPrecedesLedgerAndStartedEvent(t *testing.T) {
	ctx := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{RunID: "run-invalid-args"})
	ledger := &capturingToolLedger{}
	ctx = WithToolLedger(ctx, ledger)
	backend := &rejectingArgumentPreparerBackend{}
	events := make(chan string, 8)

	result := (&Agent{backend: backend}).executeSingleToolCall(ctx, "default", events, 0, llm.ToolCall{
		ID: "call-invalid-args", Function: "write_file", Args: `{"path":"result.txt","content":{"options":{"extra":true}}}`,
	})

	if result.success || backend.prepareCalls != 1 || backend.dispatchCalls != 0 {
		t.Fatalf("invalid arguments reached dispatch: result=%+v prepare=%d dispatch=%d", result, backend.prepareCalls, backend.dispatchCalls)
	}
	if ledger.entry.ToolCallID != "" {
		t.Fatalf("invalid arguments claimed durable dispatch: %+v", ledger.entry)
	}
	if !strings.Contains(result.msg.Content, "failure_phase: preparation") ||
		!strings.Contains(result.msg.Content, "effect_state: not_dispatched") {
		t.Fatalf("refusal lost preparation facts: %s", result.msg.Content)
	}
	for _, event := range drainAgentEvents(events) {
		if event.Type == "tool.started" {
			t.Fatalf("invalid arguments emitted tool.started: %+v", event)
		}
		if event.Type == "tool.completed" && event.Payload["invoked"] != false {
			t.Fatalf("refusal did not record invoked=false: %+v", event)
		}
	}
}

func TestArgumentPreparationBlocksOnlyIdenticalMalformedRepeat(t *testing.T) {
	ctx := WithRecoveryPolicy(context.Background(), NewStrategyRecoveryPolicy())
	backend := &rejectingArgumentPreparerBackend{}
	agent := &Agent{backend: backend}
	invalid := llm.ToolCall{ID: "invalid-1", Function: "write_file", Args: `{"extra":true}`}

	first := agent.executeSingleToolCall(ctx, "default", nil, 0, invalid)
	invalid.ID = "invalid-2"
	second := agent.executeSingleToolCall(ctx, "default", nil, 0, invalid)
	corrected := agent.executeSingleToolCall(ctx, "default", nil, 0, llm.ToolCall{
		ID: "valid-1", Function: "write_file", Args: `{"valid":true}`,
	})

	if first.success || second.success || !corrected.success {
		t.Fatalf("unexpected results: first=%+v second=%+v corrected=%+v", first, second, corrected)
	}
	if backend.prepareCalls != 2 || backend.dispatchCalls != 1 {
		t.Fatalf("identical refusal was re-prepared or correction was blocked: prepare=%d dispatch=%d", backend.prepareCalls, backend.dispatchCalls)
	}
	if !strings.Contains(second.msg.Content, "error_code: tool_arguments_repeated") {
		t.Fatalf("identical malformed repeat lost typed refusal: %s", second.msg.Content)
	}
}

func TestArgumentPreparationBlocksIdenticalMalformedJSON(t *testing.T) {
	ctx := WithRecoveryPolicy(context.Background(), NewStrategyRecoveryPolicy())
	backend := &rejectingArgumentPreparerBackend{}
	agent := &Agent{backend: backend}
	invalid := llm.ToolCall{ID: "json-1", Function: "write_file", Args: `{"path":`}

	first := agent.executeSingleToolCall(ctx, "default", nil, 0, invalid)
	invalid.ID = "json-2"
	second := agent.executeSingleToolCall(ctx, "default", nil, 0, invalid)
	corrected := agent.executeSingleToolCall(ctx, "default", nil, 0, llm.ToolCall{
		ID: "json-valid", Function: "write_file", Args: `{"valid":true}`,
	})

	if first.success || second.success || !corrected.success {
		t.Fatalf("unexpected results: first=%+v second=%+v corrected=%+v", first, second, corrected)
	}
	if backend.prepareCalls != 1 || backend.dispatchCalls != 1 {
		t.Fatalf("malformed JSON was reprocessed or correction was blocked: prepare=%d dispatch=%d", backend.prepareCalls, backend.dispatchCalls)
	}
	if !strings.Contains(second.msg.Content, "error_code: tool_arguments_repeated") {
		t.Fatalf("identical malformed JSON repeat lost typed refusal: %s", second.msg.Content)
	}
}

func TestArgumentPreparationRetriesSameCallAfterCatalogueChange(t *testing.T) {
	ctx := WithRecoveryPolicy(context.Background(), NewStrategyRecoveryPolicy())
	backend := &rejectingArgumentPreparerBackend{state: "catalogue-1"}
	agent := &Agent{backend: backend}
	call := llm.ToolCall{ID: "catalogue-1", Function: "dynamic_tool", Args: `{"new_field":true}`}
	first := agent.executeSingleToolCall(ctx, "default", nil, 0, call)

	backend.state = "catalogue-2"
	backend.acceptAll = true
	call.ID = "catalogue-2"
	second := agent.executeSingleToolCall(ctx, "default", nil, 0, call)

	if first.success || !second.success || backend.prepareCalls != 2 || backend.dispatchCalls != 1 {
		t.Fatalf("catalogue change did not release exact call: first=%+v second=%+v prepare=%d dispatch=%d",
			first, second, backend.prepareCalls, backend.dispatchCalls)
	}
}

type successfulToolBackend struct{}

func (successfulToolBackend) Dispatch(string, map[string]interface{}) (string, error) {
	return "ok", nil
}
func (successfulToolBackend) GetToolDefinitions() []map[string]interface{} { return nil }

func (l *cancelledOutcomeLedger) ClaimDispatch(context.Context, ToolLedgerEntry) (ToolDispatchDecision, error) {
	return ToolDispatchDecision{Execute: true, Status: "started"}, nil
}

func (l *cancelledOutcomeLedger) RecordOutcome(ctx context.Context, _, _ string, ok bool) error {
	l.recorded = true
	l.ctxErr = ctx.Err()
	if ok {
		return errors.New("cancelled tool unexpectedly succeeded")
	}
	return nil
}

type cancellationBackend struct{ entered chan struct{} }

func (b cancellationBackend) Dispatch(_ string, args map[string]interface{}) (string, error) {
	ctx, _ := args["_context"].(context.Context)
	close(b.entered)
	<-ctx.Done()
	return "", ctx.Err()
}

func (cancellationBackend) GetToolDefinitions() []map[string]interface{} { return nil }

type externalStorageErrorBackend struct{}

func (externalStorageErrorBackend) Dispatch(string, map[string]interface{}) (string, error) {
	return "", errors.New(`mcp tool "query" failed: no such table: users`)
}

func (externalStorageErrorBackend) GetToolDefinitions() []map[string]interface{} { return nil }

func (externalStorageErrorBackend) ToolExecutionMetadata(string, map[string]interface{}) ToolExecutionMetadata {
	return ToolExecutionMetadata{Origin: "external", Category: "database", RiskLevel: "high"}
}

func TestCancelledToolClosesLedgerWithCleanupContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTaskRuntimeContext(ctx, TaskRuntimeContext{RunID: "run-cancel"})
	ledger := &cancelledOutcomeLedger{}
	ctx = WithToolLedger(ctx, ledger)
	entered := make(chan struct{})
	agent := &Agent{backend: cancellationBackend{entered: entered}}

	done := make(chan toolExecutionResult, 1)
	go func() {
		done <- agent.executeSingleToolCall(ctx, "default", nil, 0, llm.ToolCall{
			ID: "call-cancel", Function: "terminal", Args: `{"command":"sleep 60"}`,
		})
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled tool did not return")
	}
	if !ledger.recorded {
		t.Fatal("cancelled tool left its durable ledger entry open")
	}
	if ledger.ctxErr != nil {
		t.Fatalf("ledger cleanup context was cancelled: %v", ledger.ctxErr)
	}
}

func TestToolLedgerClaimCarriesPlanEffectAndEnvironmentCorrelation(t *testing.T) {
	ctx := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{RunID: "run-correlated"})
	ctx = WithToolInvocationScope(ctx, ToolInvocationScope{ControlTenantID: "default", RunID: "run-correlated", EnvironmentGeneration: 7})
	state := NewRunExecutionState()
	ctx = WithRunExecutionState(ctx, state)
	UpdateRunExecutionPlan(ctx, 3, "step-verify")
	ledger := &capturingToolLedger{}
	ctx = WithToolLedger(ctx, ledger)
	agent := &Agent{backend: successfulToolBackend{}}
	result := agent.executeSingleToolCall(ctx, "default", nil, 0, llm.ToolCall{
		ID: "call-write", Function: "terminal", Args: `{"command":"touch result"}`,
	})
	if !result.success {
		t.Fatalf("tool result=%+v", result)
	}
	entry := ledger.entry
	if entry.EffectID != ToolEffectID("run-correlated", "call-write") || entry.PlanVersion != 3 ||
		entry.PlanStepID != "step-verify" || entry.Strategy != "mutate" || entry.EffectClass != "side_effect" ||
		entry.EnvironmentGeneration != 7 {
		t.Fatalf("correlated ledger entry=%+v", entry)
	}
}

func TestExternalToolStorageErrorRemainsActionableEvidence(t *testing.T) {
	agent := &Agent{backend: externalStorageErrorBackend{}}
	result := agent.executeSingleToolCall(context.Background(), "default", nil, 0, llm.ToolCall{
		ID: "call-external-query", Function: "mcp_query", Args: `{}`,
	})

	if !strings.Contains(result.msg.Content, "no such table: users") {
		t.Fatalf("external error evidence was hidden: %q", result.msg.Content)
	}
	if strings.Contains(result.msg.Content, "SelfMind storage-layer failure") || strings.Contains(result.msg.Content, "local storage") {
		t.Fatalf("external error was mislabeled as internal storage: %q", result.msg.Content)
	}
}

func TestShouldParallelizeToolCalls(t *testing.T) {
	tests := []struct {
		name  string
		calls []llm.ToolCall
		want  bool
	}{
		{
			name: "safe read-only batch",
			calls: []llm.ToolCall{
				{Function: "read_file"},
				{Function: "search_files"},
			},
			want: true,
		},
		{
			name: "terminal is sequential",
			calls: []llm.ToolCall{
				{Function: "read_file"},
				{Function: "terminal"},
			},
			want: false,
		},
		{
			name: "unknown is sequential",
			calls: []llm.ToolCall{
				{Function: "read_file"},
				{Function: "custom_tool"},
			},
			want: false,
		},
		{
			name: "single call is sequential",
			calls: []llm.ToolCall{
				{Function: "read_file"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldParallelizeToolCalls(tt.calls); got != tt.want {
				t.Fatalf("shouldParallelizeToolCalls() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLegacyToolCallsToLLM(t *testing.T) {
	calls := legacyToolCallsToLLM([]ToolCall{{Name: "read_file", Args: `{"path":"a.txt"}`}}, 2)
	if len(calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(calls))
	}
	if calls[0].ID == "" || calls[0].Function != "read_file" || calls[0].Args != `{"path":"a.txt"}` {
		t.Fatalf("unexpected call: %+v", calls[0])
	}
}

func TestEmitStructuredToolEventSuppressesUnchangedPlan(t *testing.T) {
	events := make(chan string, 2)
	args := map[string]interface{}{
		"plan": []interface{}{map[string]interface{}{"step": "Inspect", "status": "in_progress"}},
	}
	emitStructuredToolEvent(events, "update_plan", args, `{"changed":false}`, nil)
	if len(events) != 0 {
		t.Fatalf("unchanged plan emitted %d event(s)", len(events))
	}
	emitStructuredToolEvent(events, "update_plan", args, `{"changed":true}`, nil)
	if len(events) != 1 {
		t.Fatalf("changed plan emitted %d event(s), want 1", len(events))
	}
}

func TestPlanUpdatedEventUsesServerIssuedPlanResult(t *testing.T) {
	events := make(chan string, 1)
	args := map[string]interface{}{
		"plan": []interface{}{map[string]interface{}{"step": "Inspect", "status": "in_progress"}},
	}
	emitStructuredToolEvent(events, "update_plan", args,
		`{"changed":true,"plan_version":4,"plan":[{"step_id":"step-issued","step":"Inspect","status":"in_progress","success_criteria":"state captured"}],"work_units":[{"id":"wu-issued","sequence":1}]}`, nil)
	event, ok := DecodeAgentEvent(<-events)
	if !ok || len(event.Plan) != 1 || event.Plan[0].StepID != "step-issued" || event.Plan[0].SuccessCriteria != "state captured" || event.Payload["plan_version"] != float64(4) && event.Payload["plan_version"] != 4 {
		t.Fatalf("plan event=%+v ok=%v", event, ok)
	}
	if _, ok := event.Payload["work_units"]; !ok {
		t.Fatalf("plan event lost durable work-unit projection: %+v", event)
	}
}
