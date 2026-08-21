package kernel

import "testing"

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
