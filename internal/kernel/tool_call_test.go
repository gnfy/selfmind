package kernel

import (
	"strings"
	"testing"
)

func TestExtractToolCallsAcceptsXMLFallbackAndNormalizesAliases(t *testing.T) {
	text := `I need to inspect the project.
<tool>list_dir</tool>
<parameter>{"path":"/repo","recursive":true}</parameter>
</tool>
<tool>read_file</tool>
<parameter>{"file_path":"/repo/README.md"}</parameter>`

	calls := ExtractToolCalls(text)
	if len(calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2: %+v", len(calls), calls)
	}
	if calls[0].Name != "ls_r" || calls[0].Args != `{"path":"/repo","recursive":true}` {
		t.Fatalf("first call = %+v", calls[0])
	}
	if calls[1].Name != "read_file" || calls[1].Args != `{"path":"/repo/README.md"}` {
		t.Fatalf("second call = %+v", calls[1])
	}
}

func TestStripLegacyToolMarkupRemovesXMLBlocks(t *testing.T) {
	text := "Before\n<tool>read_file</tool>\n<parameter>{\"path\":\"README.md\"}</parameter>\n</tool>\nAfter"
	got := StripLegacyToolMarkup(text)
	want := "Before\n\n\nAfter"
	if got != want {
		t.Fatalf("StripLegacyToolMarkup() = %q, want %q", got, want)
	}
}

func TestBracketToolCallWithCommandJSON(t *testing.T) {
	text := `Before [TOOL:terminal:{"command":"pwd; command -v go || true; ls -la","cwd":"/repo","timeout":20}] After`
	calls := ExtractToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0].Name != "terminal" || !strings.Contains(calls[0].Args, "command -v go") {
		t.Fatalf("unexpected call: %+v", calls[0])
	}
	got := StripLegacyToolMarkup(text)
	if got != "Before  After" {
		t.Fatalf("StripLegacyToolMarkup() = %q", got)
	}
}

func TestExtractReadyToolCallsWaitsForXMLParameters(t *testing.T) {
	if calls := ExtractReadyToolCalls("<tool>list_dir</tool>"); len(calls) != 0 {
		t.Fatalf("tool should not be ready without parameters: %+v", calls)
	}
	calls := ExtractReadyToolCalls(`<tool>list_dir</tool><parameter>{"path":"/repo"}</parameter>`)
	if len(calls) != 1 || calls[0].Name != "ls_r" || calls[0].Args != `{"path":"/repo"}` {
		t.Fatalf("unexpected ready calls: %+v", calls)
	}
}
