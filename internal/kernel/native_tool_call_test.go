package kernel

import (
	"testing"

	"selfmind/internal/kernel/llm"
)

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
