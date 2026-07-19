package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLegacyToolCompatibilityNormalizesAliasesAndArrayArgs(t *testing.T) {
	text := `Before [TOOL:run_command:{"cmd":"printf ok","paths":["a]b","c"]}] After`
	calls := ExtractLegacyToolCalls(text)
	if len(calls) != 1 || calls[0].Name != "terminal" {
		t.Fatalf("calls = %#v", calls)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Args), &args); err != nil {
		t.Fatalf("args are not valid JSON: %v (%q)", err, calls[0].Args)
	}
	if args["command"] != "printf ok" {
		t.Fatalf("command alias was not normalized: %#v", args)
	}
	if got := StripLegacyToolMarkup(text); got != "Before  After" {
		t.Fatalf("stripped = %q", got)
	}
}

func TestIncompleteLegacyToolCallIsNotReady(t *testing.T) {
	text := `[TOOL:terminal:{"command":"echo hi","args":[1,2]}`
	if calls := ExtractReadyLegacyToolCalls(text); len(calls) != 0 {
		t.Fatalf("incomplete call must not be ready: %#v", calls)
	}
	if LegacyToolMarkerIndex(strings.ToUpper(text)) != 0 {
		t.Fatal("marker lookup must be case insensitive")
	}
}

func TestXMLLegacyToolCompatibility(t *testing.T) {
	text := `<tool>read</tool><parameter>{"file_path":"README.md"}</parameter>`
	calls := ExtractReadyLegacyToolCalls(text)
	if len(calls) != 1 || calls[0].Name != "read_file" || !strings.Contains(calls[0].Args, `"path":"README.md"`) {
		t.Fatalf("calls = %#v", calls)
	}
}
