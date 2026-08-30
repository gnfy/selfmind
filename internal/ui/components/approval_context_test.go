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
		"Would you like to run the following command?",
		"$ python3 scripts/report.py",
		"change: 2 files +48/-12",
		"/mnt/d/wwwroot/ai/selfmind",
		"env envsnap_1_789c4317",
		"reason: arbitrary code execution requires approval",
		`remembering allows: "python3" commands`,
		"Yes, proceed",
		"No, continue without running it",
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

func TestApprovalPromptShowsExactRuleInsteadOfBroaderClass(t *testing.T) {
	view := NewApprovalPromptDetailed(ApprovalDetails{
		Tool:       "terminal",
		Target:     "git status --short",
		GrantClass: `"git" commands`,
		Options: []ApprovalOption{
			{Label: "Yes, proceed", Key: "y", Decision: "approved"},
			{
				Label:     "Yes, and don't ask again for commands that start with `git status` in this run",
				Key:       "r",
				Decision:  "approved",
				Scope:     "run",
				RuleLabel: "commands that start with `git status`",
			},
			{Label: "No, continue without running it", Key: "n", Decision: "rejected"},
		},
	}).View(90)
	if !strings.Contains(view, "commands that start with `git status`") {
		t.Fatalf("exact rule is not visible:\n%s", view)
	}
	if strings.Contains(view, `remembering allows: "git" commands`) {
		t.Fatalf("broader class must not compete with the exact rule:\n%s", view)
	}
}

// TestApprovalPromptWrapsLongCommandLosslessly reproduces the real approval
// defect: a wide terminal was still forced through a 76-column, three-line
// target window, which replaced the middle of the command with an ellipsis.
// Approval is an authorization surface, so the redacted target may wrap but
// must remain lossless at every supported width.
func TestApprovalPromptWrapsLongCommandLosslessly(t *testing.T) {
	command := `for t in lid-tm-nginx-section-cd-develop lid-tm-nginx-api-cd-develop; do echo "--- $t"; gcloud builds triggers describe "$t" --project=trackingmore-dev --region=global --format="value(name,substitutions)" 2>&1 || true; done`
	for _, width := range []int{80, 240} {
		view := stripANSIForTest(NewApprovalPrompt("terminal", command, "").View(width))
		if strings.Contains(view, "…") {
			t.Fatalf("width %d: approval truncated the command:\n%s", width, view)
		}
		if unwrapped := strings.ReplaceAll(view, "\n", ""); !strings.Contains(unwrapped, command) {
			t.Fatalf("width %d: wrapped approval does not contain the full command:\n%s", width, view)
		}
		for i, line := range strings.Split(view, "\n") {
			if got := runewidth.StringWidth(line); got > width {
				t.Fatalf("width %d: line %d overflows (%d columns):\n%s", width, i, got, view)
			}
		}
	}
}

func TestApprovalTargetWrapPreservesCommandWhitespace(t *testing.T) {
	command := "printf '%s  %s' alpha beta\nprintf '\\t%s' gamma"
	if got := strings.Join(wrapDisplayLosslessly(command, 200), "\n"); got != command {
		t.Fatalf("lossless approval wrap = %q, want %q", got, command)
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
