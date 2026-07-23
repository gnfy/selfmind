package components

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
)

func TestEditorCursorPartsUseCursorOffset(t *testing.T) {
	before, cursor, after := editorCursorParts("abc", 10, 1)

	if before != "a" || cursor != "b" || after != "c" {
		t.Fatalf("parts = %q/%q/%q, want a/b/c", before, cursor, after)
	}
}

func TestEditorCursorPartsAtEndAddVisibleCursorCell(t *testing.T) {
	before, cursor, after := editorCursorParts("abc", 10, 3)

	if before != "abc" || cursor != " " || after != "" {
		t.Fatalf("parts = %q/%q/%q, want abc/space/empty", before, cursor, after)
	}
}

func TestEditorWrappedRowCountSoftWrap(t *testing.T) {
	// 20 double-width runes = 40 columns → 4 rows at width 10 (bubbles wrap
	// appends a trailing cursor-cell space, so a full last row spills one more).
	value := "计划分析仓库结构对比架构差异并输出结论"
	if got := editorWrappedRowCount(value, 10); got < 4 {
		t.Fatalf("wrapped rows = %d, want >= 4", got)
	}
	if got := editorWrappedRowCount("short", 40); got != 1 {
		t.Fatalf("wrapped rows = %d, want 1", got)
	}
	// Hard newlines still count.
	if got := editorWrappedRowCount("a\nb\nc", 40); got != 3 {
		t.Fatalf("wrapped rows = %d, want 3", got)
	}
}

func TestEditorVisibleInputLineCountUsesLayoutWidth(t *testing.T) {
	e := &Editor{textarea: textarea.New()}
	// Long single-line input: without a layout width we can only count hard
	// newlines (1); with one, soft-wrapped rows expand the composer height.
	e.textarea.SetValue(strings.Repeat("word ", 30)) // ~150 cols
	if got := e.visibleInputLineCount(); got != 1 {
		t.Fatalf("no-width count = %d, want 1", got)
	}
	e.SetLayoutWidth(54) // textW = 50 → 3 rows, capped at maxComposerInputLines
	if got := e.visibleInputLineCount(); got < 3 {
		t.Fatalf("wrapped count = %d, want >= 3", got)
	}
	e.textarea.SetValue(strings.Repeat("word ", 200))
	if got := e.visibleInputLineCount(); got != maxComposerInputLines {
		t.Fatalf("capped count = %d, want %d", got, maxComposerInputLines)
	}
}

func TestEditorWrapMatchesTextareaRowOffset(t *testing.T) {
	ta := textarea.New()
	ta.Prompt = "" // match the editor's configuration: full width is text
	ta.ShowLineNumbers = false
	ta.SetWidth(20)

	value := strings.Repeat("架构对比分析", 6) // CJK, soft-wraps at width 20
	ta.SetValue(value)                     // cursor lands at the end

	rows := wrapEditorLine([]rune(value), 20)
	li := ta.LineInfo()
	if li.RowOffset != len(rows)-1 {
		t.Fatalf("textarea RowOffset = %d, wrap clone last row = %d — wrap algorithms diverged", li.RowOffset, len(rows)-1)
	}
}

func TestEditorCursorAtTextBoundary(t *testing.T) {
	e := &Editor{textarea: textarea.New()}
	e.textarea.SetWidth(40)

	if !e.CursorAtTextBoundary() {
		t.Fatal("empty editor must be at boundary")
	}

	e.textarea.SetValue("line1\nline2") // SetValue leaves the cursor at the end
	if !e.CursorAtTextBoundary() {
		t.Fatal("cursor at end of text must be at boundary")
	}

	e.textarea.CursorUp() // interior line
	if e.CursorAtTextBoundary() {
		t.Fatal("cursor on interior position must not be at boundary")
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
