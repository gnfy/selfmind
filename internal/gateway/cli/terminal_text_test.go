package cli

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestSanitizeTerminalTextHandlesTerminalControls(t *testing.T) {
	in := "old progress\rnew progress\nabc\bD\tend\x1b]8;;https://example.com\x07link\x1b]8;;\x07\x00"
	got := sanitizeTerminalText(in)
	if strings.ContainsAny(got, "\r\b\x00\x1b") {
		t.Fatalf("terminal controls leaked into display text: %q", got)
	}
	if !strings.Contains(got, "new progress") || strings.Contains(got, "old progress") {
		t.Fatalf("carriage-return overwrite was not applied: %q", got)
	}
	if !strings.Contains(got, "abD") || !strings.Contains(got, "link") {
		t.Fatalf("printable terminal content was lost: %q", got)
	}
}

func TestWrapTextHardWrapsLongTokens(t *testing.T) {
	got := wrapText(strings.Repeat("x", 37), 10)
	for _, line := range strings.Split(got, "\n") {
		if width := ansi.StringWidth(line); width > 10 {
			t.Fatalf("line width = %d, want <= 10: %q", width, line)
		}
	}
}

func TestPrepareTerminalCellMatchesPhysicalRows(t *testing.T) {
	rendered := "\x1b[38;5;2m" + strings.Repeat("界", 24) + "\x1b[0m"
	got := prepareTerminalCell(rendered, 20)
	lines := strings.Split(got, "\n")
	if len(lines) < 3 {
		t.Fatalf("expected physical wrapping, got %d rows: %q", len(lines), got)
	}
	for _, line := range lines {
		if width := ansi.StringWidth(line); width > 20 {
			t.Fatalf("physical row width = %d, want <= 20: %q", width, line)
		}
		if !strings.HasSuffix(line, terminalReset) {
			t.Fatalf("physical row is missing a terminal reset: %q", line)
		}
	}
}

func TestCommandPresentationHidesHeredocBody(t *testing.T) {
	command := "python3 - <<'PY'\nprint('a very long body')\nPY"
	got := commandToolAction(command, true)
	if got != "Ran Python script" {
		t.Fatalf("command action = %q", got)
	}
	if strings.Contains(got, "print") || strings.Contains(got, "PY") {
		t.Fatalf("heredoc body leaked into the header: %q", got)
	}
}

func TestCommandOutputPreviewKeepsHeadAndTail(t *testing.T) {
	rows := boundedCommandOutputRows("one\ntwo\nthree\nfour\nfive\nsix\nseven", 80, 5)
	got := strings.Join(rows, "|")
	for _, want := range []string{"one", "two", "3 more lines", "six", "seven"} {
		if !strings.Contains(got, want) {
			t.Fatalf("preview %q missing %q", got, want)
		}
	}
}

func TestCommittedLongCommandMatchesTerminalPhysicalRows(t *testing.T) {
	model := &uiModel{width: 48}
	msg := ChatMessage{
		Role:      "tool",
		ToolName:  "terminal",
		ToolArgs:  `{"command":"python3 - <<'PY'\nprint('this body must not become a tool title')\nPY"}`,
		Content:   "first\n" + strings.Repeat("x", 120) + "\nlast",
		Duration:  0.2,
		IsRunning: false,
	}

	model.commit(&msg)
	if len(model.pendingPrintln) != 1 {
		t.Fatalf("committed cells = %d, want 1", len(model.pendingPrintln))
	}
	rendered := model.pendingPrintln[0]
	plain := stripANSI(rendered)
	if strings.Contains(plain, "this body must not become") {
		t.Fatalf("heredoc body leaked into committed title: %q", plain)
	}
	if !strings.Contains(plain, "Ran Python script") || !strings.Contains(plain, "last") {
		t.Fatalf("committed preview lost semantic title or tail: %q", plain)
	}
	for _, line := range strings.Split(rendered, "\n") {
		if width := ansi.StringWidth(line); width > model.commitWidth() {
			t.Fatalf("committed physical row width = %d, want <= %d: %q", width, model.commitWidth(), line)
		}
		if !strings.HasSuffix(line, terminalReset) {
			t.Fatalf("committed physical row is missing reset: %q", line)
		}
	}
}
