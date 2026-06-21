package kernel

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ToolCall represents a parsed tool call from LLM output.
type ToolCall struct {
	Name string
	Args string
}

// toolCallRe matches tool call patterns in LLM response text
var toolCallRe = regexp.MustCompile(`\[TOOL:([^:\]]+)(?::([^\]]+))?\]`)
var xmlToolCallRe = regexp.MustCompile(`(?is)<tool>\s*([a-zA-Z0-9_.-]+)\s*</tool>\s*(?:<parameter>\s*(.*?)\s*</parameter>)?`)
var xmlToolCallWithParameterRe = regexp.MustCompile(`(?is)<tool>\s*([a-zA-Z0-9_.-]+)\s*</tool>\s*<parameter>\s*(.*?)\s*</parameter>`)

// ExtractToolCalls extracts tool calls from LLM response text.
// Matches patterns like [TOOL:tool_name] or [TOOL:tool_name:{"key":"value"}]
func ExtractToolCalls(text string) []ToolCall {
	var calls []ToolCall
	matches := toolCallRe.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		if call, ok := normalizeLegacyToolCall(m[1], m[2]); ok {
			calls = append(calls, call)
		}
	}
	xmlMatches := xmlToolCallRe.FindAllStringSubmatch(text, -1)
	for _, m := range xmlMatches {
		args := ""
		if len(m) > 2 {
			args = m[2]
		}
		if call, ok := normalizeLegacyToolCall(m[1], args); ok {
			calls = append(calls, call)
		}
	}
	return calls
}

func StripLegacyToolMarkup(text string) string {
	text = toolCallRe.ReplaceAllString(text, "")
	text = xmlToolCallRe.ReplaceAllString(text, "")
	text = regexp.MustCompile(`(?is)</tool>`).ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

func ExtractReadyToolCalls(text string) []ToolCall {
	var calls []ToolCall
	matches := toolCallRe.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		if call, ok := normalizeLegacyToolCall(m[1], m[2]); ok {
			calls = append(calls, call)
		}
	}
	xmlMatches := xmlToolCallWithParameterRe.FindAllStringSubmatch(text, -1)
	for _, m := range xmlMatches {
		if call, ok := normalizeLegacyToolCall(m[1], m[2]); ok {
			calls = append(calls, call)
		}
	}
	return calls
}

func legacyToolMarkerIndex(text string) int {
	lower := strings.ToLower(text)
	idx := -1
	for _, marker := range []string{"[tool:", "<tool>"} {
		if i := strings.Index(lower, marker); i >= 0 && (idx < 0 || i < idx) {
			idx = i
		}
	}
	return idx
}

func normalizeLegacyToolCall(name, args string) (ToolCall, bool) {
	name = normalizeLegacyToolName(name)
	if name == "" {
		return ToolCall{}, false
	}
	return ToolCall{Name: name, Args: normalizeLegacyToolArgs(name, args)}, true
}

func normalizeLegacyToolName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "list_dir", "list_directory", "list_files", "ls", "dir":
		return "ls_r"
	case "read", "read_file", "cat":
		return "read_file"
	case "run_command", "execute_command", "shell", "bash":
		return "terminal"
	case "grep", "search", "search_file", "search_files":
		return "search_files"
	default:
		return strings.TrimSpace(name)
	}
}

func normalizeLegacyToolArgs(name, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &args); err != nil || args == nil {
		return raw
	}
	copyStringArg(args, "file_path", "path")
	copyStringArg(args, "filepath", "path")
	copyStringArg(args, "dir", "path")
	copyStringArg(args, "directory", "path")
	if name == "terminal" {
		copyStringArg(args, "cmd", "command")
	}
	data, err := json.Marshal(args)
	if err != nil {
		return raw
	}
	return string(data)
}

func copyStringArg(args map[string]interface{}, from, to string) {
	if _, ok := args[to]; ok {
		return
	}
	if v, ok := args[from].(string); ok && strings.TrimSpace(v) != "" {
		args[to] = strings.TrimSpace(v)
		delete(args, from)
	}
}
