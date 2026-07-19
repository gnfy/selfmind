package kernel

import "selfmind/internal/kernel/llm"

// ToolCall represents a parsed tool call from LLM output.
type ToolCall struct {
	Name string
	Args string
}

// ExtractToolCalls extracts tool calls from LLM response text.
// Matches patterns like [TOOL:tool_name] or [TOOL:tool_name:{"key":"value"}]
func ExtractToolCalls(text string) []ToolCall {
	legacy := llm.ExtractLegacyToolCalls(text)
	calls := make([]ToolCall, 0, len(legacy))
	for _, call := range legacy {
		calls = append(calls, ToolCall{Name: call.Name, Args: call.Args})
	}
	return calls
}

func StripLegacyToolMarkup(text string) string {
	return llm.StripLegacyToolMarkup(text)
}

func ExtractReadyToolCalls(text string) []ToolCall {
	legacy := llm.ExtractReadyLegacyToolCalls(text)
	calls := make([]ToolCall, 0, len(legacy))
	for _, call := range legacy {
		calls = append(calls, ToolCall{Name: call.Name, Args: call.Args})
	}
	return calls
}

func legacyToolMarkerIndex(text string) int {
	return llm.LegacyToolMarkerIndex(text)
}
