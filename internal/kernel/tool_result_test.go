package kernel

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPackageToolResultSplitsPreviewAndModelContent(t *testing.T) {
	raw := strings.Repeat("中文输出", 30000)

	env := packageToolResult("read_file", raw)

	if !env.Truncated {
		t.Fatalf("expected large result to be marked truncated")
	}
	if env.Preview == "" || len(env.Preview) > toolResultPreviewBytes+16 {
		t.Fatalf("unexpected preview length/content: len=%d value=%q", len(env.Preview), env.Preview)
	}
	if !strings.Contains(env.ModelContent, "tool output truncated for model context") {
		t.Fatalf("model content should explain bounded context: %q", env.ModelContent)
	}
	if !utf8.ValidString(env.Preview) || !utf8.ValidString(env.ModelContent) {
		t.Fatalf("packaged result must stay valid UTF-8")
	}
	if env.Raw != raw {
		t.Fatalf("raw result should be preserved separately")
	}
}

func TestPackageToolResultSummarizesListFilesPreview(t *testing.T) {
	raw := `{"path":".","entries":["a.go","b.go"],"count":2,"scanned":10,"truncated":true,"skipped_dirs":1}`

	env := packageToolResult("ls_r", raw)

	if env.Preview != "2 entries · 10 scanned · 1 dirs skipped · truncated" {
		t.Fatalf("preview = %q", env.Preview)
	}
	if env.ModelContent != raw {
		t.Fatalf("small raw JSON should still reach model unchanged")
	}
}
