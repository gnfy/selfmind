package llm

import (
	"encoding/json"
	"regexp"
	"strings"
)

// LegacyToolCall is the normalized output of a text-only provider's tool
// protocol. Native providers should populate StreamEvent.ToolCalls instead.
type LegacyToolCall struct {
	Name string
	Args string
}

type legacyMarkupSpan struct {
	start int
	end   int
	call  LegacyToolCall
}

var (
	xmlToolCallRe              = regexp.MustCompile(`(?is)<tool>\s*([a-zA-Z0-9_.-]+)\s*</tool>\s*(?:<parameter>\s*(.*?)\s*</parameter>)?`)
	xmlToolCallWithParameterRe = regexp.MustCompile(`(?is)<tool>\s*([a-zA-Z0-9_.-]+)\s*</tool>\s*<parameter>\s*(.*?)\s*</parameter>`)
	danglingToolCloseRe        = regexp.MustCompile(`(?is)</tool>`)
)

// ExtractLegacyToolCalls normalizes completed fallback tool calls emitted as
// text. This compatibility layer is deliberately inside llm: the agent loop
// should not know vendor- or fallback-protocol spellings.
func ExtractLegacyToolCalls(text string) []LegacyToolCall {
	calls := make([]LegacyToolCall, 0)
	for _, span := range legacyBracketSpans(text) {
		calls = append(calls, span.call)
	}
	for _, match := range xmlToolCallRe.FindAllStringSubmatch(text, -1) {
		args := ""
		if len(match) > 2 {
			args = match[2]
		}
		if call, ok := normalizeLegacyToolCall(match[1], args); ok {
			calls = append(calls, call)
		}
	}
	return calls
}

// ExtractReadyLegacyToolCalls is used while a stream is still open. XML calls
// require a closed parameter element; bracket calls require their closing ]
// and, for JSON arguments, balanced JSON containers.
func ExtractReadyLegacyToolCalls(text string) []LegacyToolCall {
	calls := make([]LegacyToolCall, 0)
	for _, span := range legacyBracketSpans(text) {
		calls = append(calls, span.call)
	}
	for _, match := range xmlToolCallWithParameterRe.FindAllStringSubmatch(text, -1) {
		if call, ok := normalizeLegacyToolCall(match[1], match[2]); ok {
			calls = append(calls, call)
		}
	}
	return calls
}

// StripLegacyToolMarkup removes completed fallback calls from assistant prose.
func StripLegacyToolMarkup(text string) string {
	spans := legacyBracketSpans(text)
	if len(spans) > 0 {
		var out strings.Builder
		last := 0
		for _, span := range spans {
			out.WriteString(text[last:span.start])
			last = span.end
		}
		out.WriteString(text[last:])
		text = out.String()
	}
	text = xmlToolCallRe.ReplaceAllString(text, "")
	text = danglingToolCloseRe.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

// LegacyToolMarkerIndex returns the first possible fallback-protocol marker.
func LegacyToolMarkerIndex(text string) int {
	lower := strings.ToLower(text)
	idx := -1
	for _, marker := range []string{"[tool:", "<tool>"} {
		if i := strings.Index(lower, marker); i >= 0 && (idx < 0 || i < idx) {
			idx = i
		}
	}
	return idx
}

// legacyBracketSpans scans balanced JSON instead of using a [^]] regexp.
// Arrays in arguments such as {"paths":["a","b"]} therefore remain intact.
func legacyBracketSpans(text string) []legacyMarkupSpan {
	lower := strings.ToLower(text)
	spans := make([]legacyMarkupSpan, 0)
	for cursor := 0; cursor < len(text); {
		rel := strings.Index(lower[cursor:], "[tool:")
		if rel < 0 {
			break
		}
		start := cursor + rel
		pos := start + len("[tool:")
		nameEnd := pos
		for nameEnd < len(text) && text[nameEnd] != ':' && text[nameEnd] != ']' {
			nameEnd++
		}
		if nameEnd >= len(text) {
			break
		}
		name := text[pos:nameEnd]
		args := ""
		end := -1
		if text[nameEnd] == ']' {
			end = nameEnd + 1
		} else {
			argStart := nameEnd + 1
			argEnd, close := legacyBracketArgEnd(text, argStart)
			if close >= 0 {
				args = text[argStart:argEnd]
				end = close + 1
			}
		}
		if end < 0 {
			cursor = start + len("[tool:")
			continue
		}
		if call, ok := normalizeLegacyToolCall(name, args); ok {
			spans = append(spans, legacyMarkupSpan{start: start, end: end, call: call})
		}
		cursor = end
	}
	return spans
}

func legacyBracketArgEnd(text string, start int) (argEnd, close int) {
	if start >= len(text) {
		return -1, -1
	}
	if text[start] != '{' && text[start] != '[' {
		if i := strings.IndexByte(text[start:], ']'); i >= 0 {
			return start + i, start + i
		}
		return -1, -1
	}
	stack := []byte{text[start]}
	inString := false
	escaped := false
	for i := start + 1; i < len(text); i++ {
		ch := text[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, ch)
		case '}', ']':
			if len(stack) == 0 || !matchingJSONDelimiter(stack[len(stack)-1], ch) {
				return -1, -1
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				if i+1 < len(text) && text[i+1] == ']' {
					return i + 1, i + 1
				}
				return -1, -1
			}
		}
	}
	return -1, -1
}

func matchingJSONDelimiter(open, close byte) bool {
	return open == '{' && close == '}' || open == '[' && close == ']'
}

func normalizeLegacyToolCall(name, args string) (LegacyToolCall, bool) {
	name = normalizeLegacyToolName(name)
	if name == "" {
		return LegacyToolCall{}, false
	}
	return LegacyToolCall{Name: name, Args: normalizeLegacyToolArgs(name, args)}, true
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
	if value, ok := args[from].(string); ok && strings.TrimSpace(value) != "" {
		args[to] = strings.TrimSpace(value)
		delete(args, from)
	}
}
