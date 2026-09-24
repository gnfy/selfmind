package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The Composer caret is painted into the view, but macOS input methods anchor
// their candidate window to the terminal's real cursor. Every active-region
// frame must therefore carry the caret target through the renderer instead of
// leaving the real cursor on the status row below the Composer.
func TestHybridFrameCarriesComposerCursorTargetForIME(t *testing.T) {
	model := NewController("provider", "model", nil, "").model
	model.width, model.height = 100, 30
	model.editor.SetValue("wqi")

	view := model.viewActiveRegion()
	if !strings.Contains(view, "\x1b]777;selfmind-cursor=") {
		t.Fatalf("active frame has no terminal cursor target; the input method will anchor below the Composer:\n%s", stripANSI(view))
	}
}

func TestCursorTargetWriterPlacesTerminalCursorAtComposerCaret(t *testing.T) {
	var out bytes.Buffer
	writer := newCursorTargetWriter(&out)
	frame := "composer\r\nstatus\r" + terminalCursorTargetPrefix + "2,5,1\x1b\\"
	if n, err := writer.Write([]byte(frame)); err != nil || n != len(frame) {
		t.Fatalf("write = %d, %v", n, err)
	}
	wantSuffix := ansi.CursorUp(2) + ansi.CursorHorizontalAbsolute(6)
	if got := out.String(); strings.Contains(got, "selfmind-cursor") || !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("terminal output does not end at the Composer caret: %q", got)
	}
}

func TestCursorTargetWriterRestoresRendererOriginBeforeNextFrame(t *testing.T) {
	var out bytes.Buffer
	writer := newCursorTargetWriter(&out)
	first := "first\r" + terminalCursorTargetPrefix + "2,5,1\x1b\\"
	if _, err := writer.Write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	start := out.Len()
	second := ansi.CursorUp(4) + "second\r" + terminalCursorTargetPrefix + "2,6,2\x1b\\"
	if _, err := writer.Write([]byte(second)); err != nil {
		t.Fatal(err)
	}
	wantPrefix := ansi.CursorDown(2) + "\r" + ansi.CursorUp(4)
	if got := out.String()[start:]; !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("next frame started from the anchored caret instead of the renderer origin: %q", got)
	}
}

func TestCursorTargetWriterLeavesRendererAtBottomForTerminalRestore(t *testing.T) {
	var out bytes.Buffer
	writer := newCursorTargetWriter(&out)
	frame := "frame\r" + terminalCursorTargetPrefix + "2,5,1\x1b\\"
	if _, err := writer.Write([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	start := out.Len()
	if _, err := writer.Write([]byte(ansi.EraseEntireLine)); err != nil {
		t.Fatal(err)
	}
	want := ansi.CursorDown(2) + "\r" + ansi.EraseEntireLine
	if got := out.String()[start:]; got != want {
		t.Fatalf("terminal restore started away from the renderer bottom: got %q, want %q", got, want)
	}
}

func TestHybridCursorTargetTracksWideTextAndEveryFrame(t *testing.T) {
	model := NewController("provider", "model", nil, "").model
	model.width, model.height = 100, 30
	model.editor.SetValue("你a")

	first := model.viewActiveRegion()
	second := model.viewActiveRegion()
	firstMatch := terminalCursorTargetPattern.FindStringSubmatch(first)
	secondMatch := terminalCursorTargetPattern.FindStringSubmatch(second)
	if len(firstMatch) != 4 || len(secondMatch) != 4 {
		t.Fatalf("missing cursor target: first=%q second=%q", firstMatch, secondMatch)
	}
	if firstMatch[2] != "5" { // prompt width 2 + 你 width 2 + a width 1
		t.Fatalf("wide-character caret column = %s, want 5", firstMatch[2])
	}
	if firstMatch[3] == secondMatch[3] {
		t.Fatal("successive frames reused a cursor marker that Bubble Tea may skip")
	}
}
