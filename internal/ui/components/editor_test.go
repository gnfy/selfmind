package components

import (
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
)

func TestEditorCursorPartsUseCursorOffset(t *testing.T) {
	before, cursor, after := editorCursorParts(
		"abc",
		10,
		textarea.LineInfo{StartColumn: 0, ColumnOffset: 1},
	)

	if before != "a" || cursor != "b" || after != "c" {
		t.Fatalf("parts = %q/%q/%q, want a/b/c", before, cursor, after)
	}
}

func TestEditorCursorPartsAtEndAddVisibleCursorCell(t *testing.T) {
	before, cursor, after := editorCursorParts(
		"abc",
		10,
		textarea.LineInfo{StartColumn: 0, ColumnOffset: 3},
	)

	if before != "abc" || cursor != " " || after != "" {
		t.Fatalf("parts = %q/%q/%q, want abc/space/empty", before, cursor, after)
	}
}

func TestEditorSuggestionNavigation(t *testing.T) {
	e := &Editor{textarea: textarea.New()}
	e.SetCommandHints([]CommandHint{{Name: "/model"}, {Name: "/memory"}, {Name: "/compact"}})

	e.textarea.SetValue("/m") // matches /model, /memory (not /compact)
	if !e.SuggestionsVisible() {
		t.Fatal("expected suggestions for /m")
	}
	// Down moves to the second match; wrap-around must hold.
	e.MoveSuggestion(1)
	e.MoveSuggestion(1) // wraps back to 0 (/model) with 2 matches
	if idx := e.clampedHintIndex(2); idx != 0 {
		t.Fatalf("wrap: idx=%d want 0", idx)
	}
	e.MoveSuggestion(1) // -> /memory
	if !e.AcceptSuggestion() {
		t.Fatal("AcceptSuggestion returned false")
	}
	if got := stripTrailingPasteNewlines(e.textarea.Value()); got != "/memory " {
		t.Fatalf("accepted = %q, want %q", e.textarea.Value(), "/memory ")
	}
	// After a space the popup must close.
	if e.SuggestionsVisible() {
		t.Fatal("popup should close after completion (value has a space)")
	}
}
