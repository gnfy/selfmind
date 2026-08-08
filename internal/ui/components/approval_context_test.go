package components

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

// TestApprovalPromptRendersDecisionContext pins the batch-A panel contract: the
// person can see WHERE the operation runs, HOW BIG the change is, and WHAT a
// "remember this" answer would authorize — without any of it displacing the
// answer options.
func TestApprovalPromptRendersDecisionContext(t *testing.T) {
	prompt := NewApprovalPromptDetailed(ApprovalDetails{
		Tool:          "terminal",
		Target:        "python3 scripts/report.py --out build/report.html",
		Reason:        "arbitrary code execution requires approval",
		Environment:   "envsnap_1_789c4317",
		Cwd:           "/mnt/d/wwwroot/ai/selfmind",
		ChangeSummary: "2 files +48/-12",
		GrantClass:    `"python3" commands`,
	})
	view := prompt.View(90)

	for _, want := range []string{
		"terminal",
		"change: 2 files +48/-12",
		"/mnt/d/wwwroot/ai/selfmind",
		"env envsnap_1_789c4317",
		"reason: arbitrary code execution requires approval",
		`remembering allows: "python3" commands`,
		"Yes, run it once",
		"No, and tell the agent what to do instead",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("panel is missing %q:\n%s", want, view)
		}
	}
	// Absent context must not leave empty label rows behind.
	bare := NewApprovalPrompt("read_file", "notes.md", "").View(90)
	for _, unwanted := range []string{"where:", "change:", "reason:", "remembering allows:"} {
		if strings.Contains(bare, unwanted) {
			t.Fatalf("panel with no context must omit %q:\n%s", unwanted, bare)
		}
	}
}

// TestApprovalPromptWrapsLongCommand proves a long command is WRAPPED rather
// than middle-truncated: a command whose middle is replaced by "…" cannot be
// judged, which defeats the panel.
func TestApprovalPromptWrapsLongCommand(t *testing.T) {
	long := "bash -lc 'GOWORK=off go test ./internal/tools/... ./internal/gateway/httpapi/... -run TestApprovalRequestCarriesExecutionContext -count=1'"
	view := NewApprovalPrompt("terminal", long, "").View(80)

	if !strings.Contains(view, "bash -lc 'GOWORK=off go test") {
		t.Fatalf("panel should show the command head:\n%s", view)
	}
	if !strings.Contains(view, "-count=1'") {
		t.Fatalf("panel should reach the command tail instead of truncating it:\n%s", view)
	}
	// The box must stay intact: every rendered line has the same display width.
	lines := strings.Split(view, "\n")
	width := runewidth.StringWidth(stripANSIForTest(lines[0]))
	for i, line := range lines {
		if got := runewidth.StringWidth(stripANSIForTest(line)); got != width {
			t.Fatalf("line %d width = %d, want %d (box broken):\n%s", i, got, width, view)
		}
	}
}

// TestApprovalPromptShowsTriageUnavailable pins the A3 surface: when automatic
// triage could not rule, the panel says so, because a broken judge and a strict
// judge otherwise look identical to the person answering.
func TestApprovalPromptShowsTriageUnavailable(t *testing.T) {
	notice := NewApprovalPromptDetailed(ApprovalDetails{
		Tool: "terminal", Target: "ls", TriageUnavailable: true,
	}).View(90)
	if !strings.Contains(notice, "automatic triage unavailable") {
		t.Fatalf("panel should explain the fail-safe ask:\n%s", notice)
	}
	quiet := NewApprovalPromptDetailed(ApprovalDetails{Tool: "terminal", Target: "ls"}).View(90)
	if strings.Contains(quiet, "automatic triage unavailable") {
		t.Fatalf("a deliberate escalation must not claim triage was unavailable:\n%s", quiet)
	}
}

func TestApprovalPromptRendersBoundedCodePreview(t *testing.T) {
	view := NewApprovalPromptDetailed(ApprovalDetails{
		Tool:        "execute_code",
		Target:      "python script",
		CodePreview: "print('first')\nprint('second')",
		CodeSHA256:  "0123456789abcdef",
		CodeLines:   2,
		CodeBytes:   31,
	}).View(90)
	for _, want := range []string{"code: print('first') print('second')", "2 lines", "31 bytes", "sha256 0123456789ab"} {
		if !strings.Contains(view, want) {
			t.Fatalf("execute-code approval is missing %q:\n%s", want, view)
		}
	}
}

func TestWrapDisplayBoundsLinesAndNormalizesWhitespace(t *testing.T) {
	lines := WrapDisplay("alpha beta gamma delta epsilon zeta eta theta", 12, 2)
	if len(lines) != 2 {
		t.Fatalf("WrapDisplay lines = %d, want 2: %q", len(lines), lines)
	}
	for _, line := range lines {
		if len([]rune(line)) > 12 {
			t.Fatalf("line %q exceeds the width bound", line)
		}
	}
	if !strings.Contains(lines[1], "…") {
		t.Fatalf("overflowing text must be marked truncated: %q", lines)
	}
	// Newlines and tabs collapse so a multi-line payload cannot break the box.
	if got := WrapDisplay("one\n\ttwo   three", 40, 2); len(got) != 1 || got[0] != "one two three" {
		t.Fatalf("WrapDisplay normalization = %q", got)
	}
	if got := WrapDisplay("   ", 20, 2); got != nil {
		t.Fatalf("blank input should render nothing, got %q", got)
	}
	// A CJK run must break on display columns, not rune count.
	wide := WrapDisplay(strings.Repeat("审批", 10), 8, 3)
	for _, line := range wide {
		if got := runewidth.StringWidth(line); got > 8 {
			t.Fatalf("wide line %q width = %d, want <= 8", line, got)
		}
	}
}
