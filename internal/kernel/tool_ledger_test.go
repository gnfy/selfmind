package kernel

import (
	"context"
	"selfmind/internal/kernel/llm"
	"testing"
)

// The classifier must fail SAFE: anything not proven read-only or idempotent
// is a side effect requiring verification — an unknown/new tool never earns a
// blind re-run by omission.
func TestClassifyToolRetry(t *testing.T) {
	cases := map[string]ToolRetryClass{
		"read_file":        ToolRetryReadOnly,
		"search_files":     ToolRetryReadOnly,
		"tool_output_view": ToolRetryReadOnly,
		"write_file":       ToolRetryIdempotent,
		"patch":            ToolRetryIdempotent,
		"update_plan":      ToolRetryIdempotent,
		"terminal":         ToolRetrySideEffect,
		"verify":           ToolRetrySideEffect,
		"execute_code":     ToolRetrySideEffect,
		"watch_external":   ToolRetrySideEffect,
		"web_request":      ToolRetrySideEffect, // unknown tool → safest class
		"":                 ToolRetrySideEffect,
	}
	for name, want := range cases {
		if got := ClassifyToolRetry(name); got != want {
			t.Errorf("ClassifyToolRetry(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDispatchRetryClassificationUsesTrustedRegistration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata ToolExecutionMetadata
		want     ToolRetryClass
	}{
		{"batch_read", ToolExecutionMetadata{Origin: "builtin", ReadOnly: true}, ToolRetryReadOnly},
		{"new_local_observer", ToolExecutionMetadata{Origin: "builtin", ReadOnly: true}, ToolRetryReadOnly},
		{"new_unknown_tool", ToolExecutionMetadata{Origin: "builtin"}, ToolRetrySideEffect},
		{"read_file", ToolExecutionMetadata{Origin: "external", ReadOnly: true}, ToolRetrySideEffect},
		{"write_file", ToolExecutionMetadata{Origin: "external"}, ToolRetrySideEffect},
		{"read_file", ToolExecutionMetadata{}, ToolRetrySideEffect},
		{"write_file", ToolExecutionMetadata{Origin: "builtin"}, ToolRetryIdempotent},
	} {
		t.Run(tc.name+tc.metadata.Origin, func(t *testing.T) {
			backend := registeredRetryBackend{metadata: tc.metadata}
			ledger := &capturingToolLedger{}
			ctx := WithToolLedger(WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{RunID: "run-retry"}), ledger)
			agent := &Agent{backend: backend}
			result := agent.executeSingleToolCall(ctx, "default", nil, 0, llm.ToolCall{ID: "call", Function: tc.name, Args: `{"_read_only":true,"retry_class":"read_only"}`})
			if !result.success || ledger.entry.RetryClass != tc.want || result.retryClass != tc.want {
				t.Fatalf("result=%+v ledger=%+v want=%s", result, ledger.entry, tc.want)
			}
			if countsTowardPlanEvidence(tc.name, result.retryClass) != (tc.want != ToolRetryReadOnly) {
				t.Fatal("planning and ledger classification disagree")
			}
		})
	}
}

type registeredRetryBackend struct {
	successfulToolBackend
	metadata ToolExecutionMetadata
}

func (b registeredRetryBackend) ToolExecutionMetadata(string, map[string]interface{}) ToolExecutionMetadata {
	return b.metadata
}
