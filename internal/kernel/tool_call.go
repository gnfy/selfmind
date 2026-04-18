package kernel

import "regexp"

// ToolCall represents a parsed tool call from LLM output.
type ToolCall struct {
	Name string
	Args string
}

// toolCallRe matches tool call patterns in LLM response text
var toolCallRe = regexp.MustCompile(`\[TOOL:([^:\]]+)(?::([^\]]+))?\]`)

// ExtractToolCalls extracts tool calls from LLM response text.
// Matches patterns like [TOOL:tool_name] or [TOOL:tool_name:{"key":"value"}]
func ExtractToolCalls(text string) []ToolCall {
	var calls []ToolCall
	matches := toolCallRe.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		calls = append(calls, ToolCall{Name: m[1], Args: m[2]})
	}
	return calls
}
