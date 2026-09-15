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
