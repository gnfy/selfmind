package kernel

import (
	"fmt"
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

func TestPackageToolResultSummarizesPatchPreview(t *testing.T) {
	raw := `{"Success":true,"Diff":"","FilesModified":["/tmp/app.go"],"FilesCreated":["/tmp/new.go"],"FilesDeleted":["/tmp/old.go"]}`

	env := packageToolResult("patch", raw)

	if strings.Contains(env.Preview, `"Success"`) || strings.Contains(env.Preview, `"FilesModified"`) {
		t.Fatalf("preview should not expose raw JSON: %q", env.Preview)
	}
	for _, want := range []string{"modified /tmp/app.go", "created /tmp/new.go", "deleted /tmp/old.go"} {
		if !strings.Contains(env.Preview, want) {
			t.Fatalf("preview %q missing %q", env.Preview, want)
		}
	}
	if env.ModelContent != raw {
		t.Fatalf("small raw JSON should still reach model unchanged")
	}
}

func TestPackageToolErrorGuidesModelToDiagnose(t *testing.T) {
	env := packageToolError("terminal", errTest("exit status 1"))

	if !strings.Contains(env.Preview, "Error executing terminal") {
		t.Fatalf("preview should stay user-readable: %q", env.Preview)
	}
	if strings.Contains(env.Preview, "SelfMind diagnostic instruction") {
		t.Fatalf("preview should not expose model-only recovery instruction: %q", env.Preview)
	}
	if !strings.Contains(env.ModelContent, "SelfMind diagnostic instruction") {
		t.Fatalf("model content should include recovery instruction: %q", env.ModelContent)
	}
	if !strings.Contains(env.ModelContent, "inspect relevant context") {
		t.Fatalf("model content should nudge diagnosis before retry: %q", env.ModelContent)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

// TestPackageToolErrorUserRejection ensures a user rejection is presented to
// the model as a decision with a do-not-retry instruction, not as a
// diagnosable failure — otherwise the model retries a variant of the rejected
// command and spawns a fresh approval (observed live via /reject on WeChat).
func TestPackageToolErrorUserRejection(t *testing.T) {
	rejected := packageToolError("terminal", fmt.Errorf("operation rejected: user said no"))
	if !strings.Contains(rejected.ModelContent, "Do NOT retry") {
		t.Fatalf("rejection should carry a do-not-retry instruction, got: %s", rejected.ModelContent)
	}
	if strings.Contains(rejected.ModelContent, "corrected next step") {
		t.Fatalf("rejection must not carry the diagnose-and-retry instruction: %s", rejected.ModelContent)
	}

	cancelled := packageToolError("terminal", fmt.Errorf("operation cancelled by user"))
	if !strings.Contains(cancelled.ModelContent, "Do NOT retry") {
		t.Fatalf("cancellation should carry a do-not-retry instruction, got: %s", cancelled.ModelContent)
	}

	ordinary := packageToolError("terminal", fmt.Errorf("exit status 1"))
	if !strings.Contains(ordinary.ModelContent, "corrected next step") {
		t.Fatalf("ordinary failures keep the diagnostic instruction, got: %s", ordinary.ModelContent)
	}
}
