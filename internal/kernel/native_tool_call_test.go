package kernel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
)

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
