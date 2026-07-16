package cli

import (
	"strings"
	"unicode"

	"selfmind/internal/platform/textutil"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

const terminalReset = "\x1b[0m"

// stripANSI turns untrusted terminal output into stable printable text. In
// addition to CSI sequences it removes OSC/DCS escapes, applies carriage-return
// and backspace semantics, expands tabs, and drops remaining control bytes.
// Renderers can then add their own known-safe lipgloss styles.
func stripANSI(s string) string {
	return sanitizeTerminalText(s)
}

func sanitizeTerminalText(s string) string {
	s = textutil.CleanUTF8(ansi.Strip(s))
	s = strings.ReplaceAll(s, "\r\n", "\n")

	lines := make([]string, 0, strings.Count(s, "\n")+1)
	line := make([]rune, 0, 80)
	lineWidth := 0
	flush := func() {
		lines = append(lines, string(line))
		line = line[:0]
		lineWidth = 0
	}
	for _, r := range s {
		switch r {
		case '\n':
			flush()
		case '\r':
			// Progress reporters overwrite the current terminal row. Keeping the
			// latest frame is both readable and faithful to terminal behavior.
			line = line[:0]
			lineWidth = 0
		case '\b':
			if len(line) > 0 {
				line = line[:len(line)-1]
				lineWidth = runewidth.StringWidth(string(line))
			}
		case '\t':
			spaces := 4 - lineWidth%4
			line = append(line, []rune(strings.Repeat(" ", spaces))...)
			lineWidth += spaces
		default:
			if unicode.IsControl(r) {
				continue
			}
			line = append(line, r)
			lineWidth += runewidth.RuneWidth(r)
		}
	}
	flush()
	return strings.Join(lines, "\n")
}

// wrapText preserves known-safe ANSI styles while guaranteeing that even a
// single unbroken token is split at the terminal's physical display width.
func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Wrap(s, width, "")
}

// prepareTerminalCell is the final boundary before a finalized cell enters
// native scrollback. Bubble Tea tracks rendered rows; terminals wrap by display
// columns. Pre-wrapping here keeps those counts identical and prevents stale
// editor-background strips after long commands or here-docs.
func prepareTerminalCell(rendered string, width int) string {
	if width <= 0 {
		width = 80
	}
	rendered = ansi.Hardwrap(strings.TrimRight(rendered, "\n"), width, false)
	if rendered == "" {
		return ""
	}
	lines := strings.Split(rendered, "\n")
	for i := range lines {
		// Reset every physical row. This is deliberately redundant with
		// lipgloss and protects against a styled span ending at the wrap edge.
		lines[i] += terminalReset
	}
	return strings.Join(lines, "\n")
}

func physicalDisplayLines(s string, width int) []string {
	if width <= 0 {
		width = 80
	}
	s = strings.TrimRight(sanitizeTerminalText(s), "\n")
	if s == "" {
		return nil
	}
	wrapped := ansi.Wrap(s, width, "")
	return strings.Split(wrapped, "\n")
}
