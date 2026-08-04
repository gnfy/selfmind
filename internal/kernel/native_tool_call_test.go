package kernel

import (
	"context"
	"errors"
	"testing"
	"time"

	"selfmind/internal/kernel/llm"
)

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
