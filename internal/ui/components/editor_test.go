package components

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
	"selfmind/internal/ui/common"
	"selfmind/internal/ui/layout"
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

func TestEditorInputRowsFollowTerminalHeight(t *testing.T) {
	e := &Editor{textarea: textarea.New()}
	e.textarea.SetValue(strings.Join([]string{"1", "2", "3", "4", "5", "6", "7", "8"}, "\n"))

	e.SetLayout(80, 9)
	if got := e.visibleInputLineCount(); got != 3 {
		t.Fatalf("height 9 rows = %d, want 3", got)
	}
	e.SetLayout(80, 3)
	if got := e.visibleInputLineCount(); got != 2 {
		t.Fatalf("height 3 rows = %d, want minimum 2", got)
	}
	e.SetLayout(80, 60)
	if got := e.visibleInputLineCount(); got != 6 {
		t.Fatalf("height 60 rows = %d, want cap 6", got)
	}
}

func TestEditorDrawUsesOpenBoundariesAndOverflowMetadata(t *testing.T) {
	c := &common.Common{Styles: common.DefaultStyles()}
	e := NewEditor(c, nil)
	e.SetCursorVisible(false)
	e.SetLayout(32, 12) // four visible input rows
	e.SetValue(strings.Join([]string{"one", "two", "three", "four", "five", "six"}, "\n"))

	rendered := e.Draw(layout.Rect{W: 32, H: e.PreferredHeight()})
	plain := stripANSIForTest(rendered)
	lines := strings.Split(plain, "\n")
	if !strings.Contains(lines[0], "Lines 3–6/6") {
		t.Fatalf("top boundary = %q, want visible row range", lines[0])
	}
	if lines[len(lines)-1] != strings.Repeat("─", 32) {
		t.Fatalf("bottom boundary = %q", lines[len(lines)-1])
	}
	if strings.Contains(rendered, "\x1b[48;") {
		t.Fatalf("composer painted a background: %q", rendered)
	}
}

func TestEditorDrawShowsAdaptiveInputAndImageHints(t *testing.T) {
	c := &common.Common{Styles: common.DefaultStyles()}
	e := NewEditor(c, nil)
	e.SetCursorVisible(false)
	e.SetLayout(100, 24)

	empty := stripANSIForTest(e.Draw(layout.Rect{W: 100, H: e.PreferredHeight()}))
	if !strings.Contains(strings.Split(empty, "\n")[0], "Ctrl+J newline · Ctrl+V image") {
		t.Fatalf("empty composer boundary missing input hints: %q", strings.Split(empty, "\n")[0])
	}

	token := e.AttachImage("/tmp/reference.png")
	attached := stripANSIForTest(e.Draw(layout.Rect{W: 100, H: e.PreferredHeight()}))
	if !strings.Contains(strings.Split(attached, "\n")[0], "1 image · Ctrl+J newline · Ctrl+V more") {
		t.Fatalf("attached composer boundary missing live image state: %q", strings.Split(attached, "\n")[0])
	}

	e.SetValue(strings.Replace(e.Value(), token, "", 1))
	removed := stripANSIForTest(e.Draw(layout.Rect{W: 100, H: e.PreferredHeight()}))
	if strings.Contains(strings.Split(removed, "\n")[0], "1 image") || !strings.Contains(strings.Split(removed, "\n")[0], "Ctrl+V image") {
		t.Fatalf("deleted image token left stale composer state: %q", strings.Split(removed, "\n")[0])
	}
}

func TestEditorCtrlJInsertsNewlineWhileEnterSubmits(t *testing.T) {
	c := &common.Common{Styles: common.DefaultStyles()}
	e := NewEditor(c, nil)
	e.SetValue("first line")

	if result := e.HandleKey(tea.KeyMsg{Type: tea.KeyCtrlJ}); result.Action != ComposerActionHandled {
		t.Fatalf("Ctrl+J action = %v, want handled", result.Action)
	}
	if got := e.Value(); got != "first line\n" {
		t.Fatalf("Ctrl+J value = %q, want trailing newline", got)
	}
	if result := e.HandleKey(tea.KeyMsg{Type: tea.KeyEnter}); result.Action != ComposerActionSubmit {
		t.Fatalf("Enter action = %v, want submit", result.Action)
	}
}

func TestEditorHintNeverWidensNarrowComposer(t *testing.T) {
	c := &common.Common{Styles: common.DefaultStyles()}
	e := NewEditor(c, nil)
	e.SetCursorVisible(false)
	e.AttachImage("/tmp/reference.png")

	const width = 24
	rendered := stripANSIForTest(e.Draw(layout.Rect{W: width, H: e.PreferredHeight()}))
	for _, line := range strings.Split(rendered, "\n") {
		if got := runewidth.StringWidth(line); got > width {
			t.Fatalf("narrow composer line width = %d, want <= %d: %q", got, width, line)
		}
	}
}

func TestEditorDrawLabelsRecalledHistoryWithoutSideRails(t *testing.T) {
	c := &common.Common{Styles: common.DefaultStyles()}
	e := NewEditor(c, nil)
	e.SetCursorVisible(false)
	e.SetLayout(40, 24)
	e.SeedHistory([]string{"first", "second"}, 1024)
	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})

	plain := stripANSIForTest(e.Draw(layout.Rect{W: 40, H: e.PreferredHeight()}))
	lines := strings.Split(plain, "\n")
	if !strings.Contains(lines[0], "History 2/2") {
		t.Fatalf("history boundary = %q", lines[0])
	}
	if strings.HasPrefix(lines[1], "│") || strings.HasSuffix(lines[1], "│") {
		t.Fatalf("composer body has side rails: %q", lines[1])
	}
}

func TestEditorWrapMatchesTextareaRowOffset(t *testing.T) {
	ta := textarea.New()
	ta.Prompt = "" // match the editor's configuration: full width is text
	ta.ShowLineNumbers = false
	ta.SetWidth(20)

	value := strings.Repeat("架构对比分析", 6) // CJK, soft-wraps at width 20
	ta.SetValue(value)                   // cursor lands at the end

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

func TestComposerHistoryOwnsArrowsAcrossRecalledSlashCommand(t *testing.T) {
	e := &Editor{textarea: textarea.New()}
	e.SetCommandHints([]CommandHint{{Name: "/model"}, {Name: "/memory"}})
	e.SeedHistory([]string{"ordinary question", "/model"}, 1024)

	if result := e.HandleKey(tea.KeyMsg{Type: tea.KeyUp}); !result.Handled() {
		t.Fatal("first Up should be owned by composer history")
	}
	if got := e.Value(); got != "/model" {
		t.Fatalf("first Up recalled %q, want /model", got)
	}
	if e.SuggestionsVisible() {
		t.Fatal("a recalled slash command must not reopen completion")
	}

	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "ordinary question" {
		t.Fatalf("second Up recalled %q, want ordinary question", got)
	}

	e.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got := e.Value(); got != "/model" {
		t.Fatalf("first Down recalled %q, want /model", got)
	}
	e.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got := e.Value(); got != "" {
		t.Fatalf("Down past newest left %q, want an empty composer", got)
	}
}

func TestComposerManualSlashCompletionOwnsArrows(t *testing.T) {
	e := &Editor{textarea: textarea.New(), historyIndex: -1}
	e.SetCommandHints([]CommandHint{{Name: "/model"}, {Name: "/memory"}})
	e.SeedHistory([]string{"ordinary question"}, 1024)
	e.SetValue("/m")

	e.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got := e.Value(); got != "/m" {
		t.Fatalf("completion navigation changed the draft to %q", got)
	}
	e.HandleKey(tea.KeyMsg{Type: tea.KeyTab})
	if got := e.Value(); got != "/memory " {
		t.Fatalf("Down then Tab completed %q, want /memory", got)
	}
}

func TestComposerEscapeDismissesCompletionUntilTheTokenChanges(t *testing.T) {
	e := &Editor{textarea: textarea.New(), historyIndex: -1}
	e.textarea.Focus()
	e.SetCommandHints([]CommandHint{{Name: "/model"}, {Name: "/memory"}})
	e.SetValue("/m")
	if !e.SuggestionsVisible() {
		t.Fatal("precondition: /m should open completion")
	}

	if result := e.HandleKey(tea.KeyMsg{Type: tea.KeyEsc}); !result.Handled() {
		t.Fatal("Esc should be consumed when it dismisses completion")
	}
	if e.SuggestionsVisible() {
		t.Fatal("dismissed completion reopened for the unchanged token")
	}

	e.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if got := e.Value(); got != "/mo" {
		t.Fatalf("edited value = %q, want /mo", got)
	}
	if !e.SuggestionsVisible() {
		t.Fatal("editing the token should make completion eligible again")
	}
}

func TestComposerHistoryStartsOnlyFromAnEmptyDraft(t *testing.T) {
	e := &Editor{textarea: textarea.New(), historyIndex: -1}
	e.textarea.Focus()
	e.SeedHistory([]string{"previous request"}, 1024)
	e.SetValue("current draft")

	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "current draft" {
		t.Fatalf("Up replaced a non-empty fresh draft with %q", got)
	}
}

func TestComposerHistoryYieldsWhenRecalledCursorMovesInside(t *testing.T) {
	e := &Editor{textarea: textarea.New(), historyIndex: -1}
	e.textarea.SetWidth(40)
	e.SeedHistory([]string{"older", "line one\nline two"}, 1024)

	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	e.textarea.CursorUp()
	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "line one\nline two" {
		t.Fatalf("Up at an interior cursor recalled %q, want the current entry", got)
	}
}

func TestComposerHistoryEvictsTheOldestDraftsByByteBudget(t *testing.T) {
	e := &Editor{textarea: textarea.New(), historyIndex: -1}
	e.SeedHistory([]string{"one", "two", "three"}, 8)

	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "three" {
		t.Fatalf("latest recall = %q, want three", got)
	}
	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "two" {
		t.Fatalf("older recall = %q, want two", got)
	}
	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "two" {
		t.Fatalf("evicted entry was still reachable as %q", got)
	}
}

func TestComposerPersistentSubmissionReturnsOnlySafeText(t *testing.T) {
	e := &Editor{textarea: textarea.New(), historyIndex: -1}
	e.SeedHistory(nil, 1024)
	e.SetValue("  inspect this  ")

	submission := e.Submit(ComposerHistoryPersistent)
	if !submission.Persist || submission.PersistentText != "inspect this" {
		t.Fatalf("submission persistence = %v/%q", submission.Persist, submission.PersistentText)
	}
	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "  inspect this  " {
		t.Fatalf("session recall = %q, want the original draft", got)
	}
}

func TestComposerCompletionSubmissionUsesCanonicalText(t *testing.T) {
	e := &Editor{textarea: textarea.New(), historyIndex: -1}
	e.SetCommandHints([]CommandHint{{Name: "/model"}, {Name: "/memory"}})
	e.SeedHistory(nil, 1024)
	e.SetValue("/m")

	if result := e.HandleKey(tea.KeyMsg{Type: tea.KeyEnter}); result.Action != ComposerActionSubmit {
		t.Fatalf("Enter action = %v, want submit", result.Action)
	}
	submission := e.Submit(ComposerHistoryPersistent)
	if !submission.Persist || submission.PersistentText != "/model" {
		t.Fatalf("canonical persistence = %v/%q", submission.Persist, submission.PersistentText)
	}
}

func TestComposerHistoryFoldsAdjacentDuplicateDrafts(t *testing.T) {
	e := &Editor{textarea: textarea.New(), historyIndex: -1}
	e.SeedHistory([]string{"repeat", "repeat"}, 1024)

	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	e.HandleKey(tea.KeyMsg{Type: tea.KeyDown})
	if got := e.Value(); got != "" {
		t.Fatalf("duplicate created a second navigation slot: %q", got)
	}
}

func TestComposerSecureSubmissionNeverEntersHistory(t *testing.T) {
	e := NewEditor(&common.Common{Styles: common.DefaultStyles()}, nil)
	e.SeedHistory(nil, 1024)
	e.SetSecure(true)
	e.SetValue("hunter2")

	submission := e.Submit(ComposerHistoryPersistent)
	if submission.Persist {
		t.Fatal("secure input was marked for persistence")
	}
	e.SetSecure(false)
	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "" {
		t.Fatalf("secure input entered session history as %q", got)
	}
}

func TestComposerClearCanKeepDraftInSessionHistory(t *testing.T) {
	e := &Editor{textarea: textarea.New(), historyIndex: -1}
	e.SeedHistory(nil, 1024)
	e.SetValue("cancelled draft")

	if !e.Clear(ComposerHistorySessionOnly) {
		t.Fatal("Clear should report that it cleared a draft")
	}
	if got := e.Value(); got != "" {
		t.Fatalf("cleared value = %q", got)
	}
	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != "cancelled draft" {
		t.Fatalf("cleared draft recall = %q", got)
	}
}

func newHintEditor(commands, skills []CommandHint) *Editor {
	e := &Editor{textarea: textarea.New()}
	e.SetCommandHints(commands)
	if skills != nil {
		e.SetSkillFilter(func(query string) []CommandHint {
			var matches []CommandHint
			for _, hint := range skills {
				if strings.HasPrefix(hint.Name, "$"+query) {
					matches = append(matches, hint)
				}
			}
			return matches
		})
	}
	return e
}

// Completion is prefix-directed: `/` offers commands and `$` offers Skills, and
// neither pool leaks into the other's popup.
func TestSuggestionPoolFollowsThePrefix(t *testing.T) {
	e := newHintEditor(
		[]CommandHint{{Name: "/skills", Description: "manage skills"}},
		[]CommandHint{{Name: "$grilling", Description: "stress-test a plan", Insert: "/external:grilling"}},
	)

	e.textarea.SetValue("/sk")
	matches := e.matchingCommands()
	if len(matches) != 1 || matches[0].Name != "/skills" {
		t.Fatalf("slash prefix matched %+v", matches)
	}

	e.textarea.SetValue("$gr")
	matches = e.matchingCommands()
	if len(matches) != 1 || matches[0].Name != "$grilling" {
		t.Fatalf("dollar prefix matched %+v", matches)
	}

	e.textarea.SetValue("gr")
	if matches := e.matchingCommands(); len(matches) != 0 {
		t.Fatalf("bare text opened a popup: %+v", matches)
	}
}

// A Skill hint completes to the reference that resolves it, not to the label the
// row shows.
func TestAcceptSuggestionWritesTheInsertionText(t *testing.T) {
	e := newHintEditor(nil, []CommandHint{
		{Name: "$grilling", Description: "stress-test a plan", Insert: "/external:grilling"},
	})
	e.textarea.SetValue("$gr")

	if !e.AcceptSuggestion() {
		t.Fatal("suggestion was not applied")
	}
	if got := e.textarea.Value(); got != "/external:grilling " {
		t.Fatalf("completed to %q", got)
	}
}

// Every match stays selectable: the popup windows its rows rather than dropping
// candidates, so a Skill past the visible rows is still reachable.
func TestSuggestionsKeepEveryMatchSelectable(t *testing.T) {
	var skills []CommandHint
	for _, suffix := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		skills = append(skills, CommandHint{Name: "$flow-" + suffix, Insert: "/user:flow-" + suffix})
	}
	e := newHintEditor(nil, skills)
	e.textarea.SetValue("$flow")

	if got := len(e.matchingCommands()); got != len(skills) {
		t.Fatalf("candidate list truncated to %d of %d", got, len(skills))
	}
	// Walk one past the visible window and complete: the tenth entry must be
	// reachable even though only suggestionRows are drawn.
	for i := 0; i < len(skills)-1; i++ {
		e.MoveSuggestion(1)
	}
	if !e.AcceptSuggestion() {
		t.Fatal("suggestion was not applied")
	}
	if got := e.textarea.Value(); got != "/user:flow-j " {
		t.Fatalf("completed to %q", got)
	}
}
