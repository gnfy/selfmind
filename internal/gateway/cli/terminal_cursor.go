package cli

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
	"selfmind/internal/ui/components"
)

const terminalCursorTargetPrefix = "\x1b]777;selfmind-cursor="

var terminalCursorTargetPattern = regexp.MustCompile("\\x1b\\]777;selfmind-cursor=([0-9]+),([0-9]+),([0-9]+)\\x1b\\\\")

// cursorTargetWriter consumes the zero-width target carried by a rendered
// frame, writes the ordinary frame, then moves the terminal's real cursor back
// to the painted Composer caret. macOS input methods use that real cursor for
// preedit text and their candidate window even while the cursor is hidden.
type cursorTargetWriter struct {
	mu             sync.Mutex
	out            io.Writer
	cursorAnchored bool
	anchoredRowsUp int
}

func newCursorTargetWriter(out io.Writer) *cursorTargetWriter {
	return &cursorTargetWriter{out: out}
}

func (w *cursorTargetWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	matches := terminalCursorTargetPattern.FindAllSubmatch(p, -1)
	clean := terminalCursorTargetPattern.ReplaceAll(p, nil)
	if w.cursorAnchored {
		var restore strings.Builder
		if w.anchoredRowsUp > 0 {
			restore.WriteString(ansi.CursorDown(w.anchoredRowsUp))
		}
		restore.WriteByte('\r')
		if _, err := io.WriteString(w.out, restore.String()); err != nil {
			return 0, err
		}
		w.cursorAnchored = false
		w.anchoredRowsUp = 0
	}
	if _, err := w.out.Write(clean); err != nil {
		return 0, err
	}
	if len(matches) == 0 {
		return len(p), nil
	}
	match := matches[len(matches)-1]
	rowsUp, err := strconv.Atoi(string(match[1]))
	if err != nil {
		return 0, err
	}
	column, err := strconv.Atoi(string(match[2]))
	if err != nil {
		return 0, err
	}
	var target strings.Builder
	if rowsUp > 0 {
		target.WriteString(ansi.CursorUp(rowsUp))
	}
	target.WriteString(ansi.CursorHorizontalAbsolute(column + 1))
	if _, err := io.WriteString(w.out, target.String()); err != nil {
		return 0, err
	}
	w.cursorAnchored = true
	w.anchoredRowsUp = rowsUp
	return len(p), nil
}

// terminalCursorOutput preserves Bubble Tea's terminal-file detection while
// adding cursor targeting to stdout. Close deliberately leaves process stdout
// open; Bubble Tea owns terminal restoration, not the wrapper.
type terminalCursorOutput struct {
	*cursorTargetWriter
	file *os.File
}

func newTerminalCursorOutput(file *os.File) *terminalCursorOutput {
	return &terminalCursorOutput{cursorTargetWriter: newCursorTargetWriter(file), file: file}
}

func (w *terminalCursorOutput) Read(p []byte) (int, error) { return w.file.Read(p) }
func (w *terminalCursorOutput) Close() error               { return nil }
func (w *terminalCursorOutput) Fd() uintptr                { return w.file.Fd() }

func (m *uiModel) targetComposerCursor(view string) string {
	marker := strings.Index(view, components.TerminalCursorMarker)
	if marker < 0 {
		return view
	}
	prefix := view[:marker]
	lineStart := strings.LastIndex(prefix, "\n") + 1
	column := ansi.StringWidth(prefix[lineStart:])
	row := strings.Count(prefix, "\n")
	view = strings.Replace(view, components.TerminalCursorMarker, "", 1)
	rowsUp := strings.Count(view, "\n") - row
	if rowsUp < 0 {
		rowsUp = 0
	}
	m.terminalCursorFrame++
	return view + fmt.Sprintf("%s%d,%d,%d\x1b\\", terminalCursorTargetPrefix, rowsUp, column, m.terminalCursorFrame)
}
