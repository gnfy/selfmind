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
