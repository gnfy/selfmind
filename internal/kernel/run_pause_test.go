package kernel

import (
	"context"
	"testing"

	"selfmind/internal/kernel/llm"
)

type pauseFailure struct{}

func (pauseFailure) Error() string { return "continuation was refused" }
func (pauseFailure) ToolRunPause() (string, string, bool) {
	return "selection_refused", "Review the effects before resuming.", false
}

type pauseBackend struct{ calls int }

func (b *pauseBackend) Dispatch(string, map[string]interface{}) (string, error) {
	b.calls++
	return "", pauseFailure{}
}
func (*pauseBackend) GetToolDefinitions() []map[string]interface{} { return nil }

func TestTypedRunPauseStopsRemainingCallsAndHandsOff(t *testing.T) {
	backend := &pauseBackend{}
	agent := &Agent{backend: backend}
	results := agent.executeToolCalls(context.Background(), "test", nil, []llm.ToolCall{
		{ID: "claim", Function: "work_select", Args: `{"action":"resume","run_id":"old"}`},
		{ID: "mutation", Function: "write_file", Args: `{"path":"receipt.txt","content":"wrong"}`},
	})
	if backend.calls != 1 || len(results) != 2 || results[1].success || results[1].msg.ToolCallID != "mutation" {
		t.Fatalf("pause lost call pairing or allowed dispatch: calls=%d results=%+v", backend.calls, results)
	}
	handoff, ok := lifecycleHandoffFromToolResults(results)
	if !ok || handoff.Status != "waiting_user" || handoff.CompletionReason != "selection_refused" {
		t.Fatalf("missing pause: %+v", handoff)
	}
}

type finishBoundaryBackend struct{ calls []string }

func (b *finishBoundaryBackend) Dispatch(name string, _ map[string]interface{}) (string, error) {
	b.calls = append(b.calls, name)
	if name == "finish_run" {
		return `{"status":"done","summary":"verified"}`, nil
	}
	return "read after finish", nil
}
func (*finishBoundaryBackend) GetToolDefinitions() []map[string]interface{} { return nil }

func TestSuccessfulFinishStopsLaterCallsInSameBatch(t *testing.T) {
	backend := &finishBoundaryBackend{}
	agent := &Agent{backend: backend}
	results := agent.executeToolCalls(context.Background(), "test", nil, []llm.ToolCall{
		{ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"verified"}`},
		{ID: "later", Function: "read_file", Args: `{"path":"value.txt"}`},
	})
	if len(backend.calls) != 1 || backend.calls[0] != "finish_run" || len(results) != 2 ||
		!results[0].success || results[1].success || results[1].msg.ToolCallID != "later" {
		t.Fatalf("completed run dispatched a later call: calls=%v results=%+v", backend.calls, results)
	}
}
