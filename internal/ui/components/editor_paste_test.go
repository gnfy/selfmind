package components

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/platform/pastetoken"
)

func pasteEditor(chars, lines int) *Editor {
	return &Editor{textarea: textarea.New(), largePasteChars: chars, largePasteLines: lines}
}

func pasteMsg(text string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text), Paste: true}
}

// crDocument reproduces what a terminal bracketed paste actually delivers in
// Windows Terminal/WSL: lines separated by bare CR, no LF anywhere.
func crDocument(lines int) string {
	rows := make([]string, 0, lines)
	for i := 0; i < lines; i++ {
		rows = append(rows, fmt.Sprintf("| row %02d | some release detail column | value-%02d |", i, i))
	}
	return strings.Join(rows, "\r")
}

// TestLargePasteRoundTripsThroughExpandValue is the regression this whole batch
// exists for: a large paste showed as a token whose label carried "[80 lines]",
// the pattern-based expansion could not match it, and the daemon then rejected
// the message ("the pasted content was not expanded by the client"). Every large
// paste failed, deterministically.
func TestLargePasteRoundTripsThroughExpandValue(t *testing.T) {
	e := pasteEditor(1000, 10)
	e.textarea.SetValue("这个文档看一下")
	doc := crDocument(20)

	e.Update(pasteMsg(doc))

	display := e.Value()
	if !strings.Contains(display, "[[ paste:0 ") {
		t.Fatalf("large paste did not collapse to a token: %q", display)
	}
	if strings.Contains(display, "row 05") {
		t.Fatalf("display leaked the payload instead of a compact token: %q", display)
	}
	if !pastetoken.ContainsUnresolved(display) {
		t.Fatalf("display token must be detectable as unresolved: %q", display)
	}

	expanded := e.ExpandValue()
	if pastetoken.ContainsUnresolved(expanded) {
		t.Fatalf("token survived expansion — the daemon would reject this: %q", expanded)
	}
	if !strings.Contains(expanded, "这个文档看一下") {
		t.Fatalf("expanded value lost the typed prefix: %q", expanded)
	}
	if !strings.Contains(expanded, "| row 00 |") || !strings.Contains(expanded, "| row 19 |") {
		t.Fatalf("expanded value lost pasted content: %q", truncateForLog(expanded))
	}
	if strings.Contains(expanded, "\r") {
		t.Fatalf("expanded value kept CR separators: %q", truncateForLog(expanded))
	}
	if got := strings.Count(expanded, "\n"); got != 19 {
		t.Fatalf("expanded value has %d line breaks, want 19", got)
	}
	if stranded := e.UnresolvedToken(); stranded != "" {
		t.Fatalf("UnresolvedToken = %q, want empty for a registered token", stranded)
	}
}

// TestPasteTokenLabelCountsTerminalLines pins the second half of the defect:
// line counting looked only for "\n", so a CR-separated document was labelled
// "1 lines" and the configured line threshold could never fire.
func TestPasteTokenLabelCountsTerminalLines(t *testing.T) {
	for _, sep := range []string{"\r", "\r\n", "\n"} {
		e := pasteEditor(0, 10) // char threshold disabled: only the line rule can fire
		body := strings.Join([]string{"alpha", "beta", "gamma", "delta", "epsilon",
			"zeta", "eta", "theta", "iota", "kappa", "lambda", "mu"}, sep)

		e.Update(pasteMsg(body))

		display := e.Value()
		if !strings.Contains(display, "(12 lines)") {
			t.Fatalf("separator %q: label = %q, want 12 lines", sep, display)
		}
		if inner := strings.TrimSuffix(strings.TrimPrefix(display, "[[ "), " ]]"); strings.ContainsAny(inner, "[]") {
			t.Fatalf("separator %q: label carries a bracket: %q", sep, display)
		}
		if expanded := e.ExpandValue(); pastetoken.ContainsUnresolved(expanded) {
			t.Fatalf("separator %q: token survived expansion: %q", sep, expanded)
		}
	}
}

// TestCJKPasteTokenSurvivesRuneBoundaries pins the byte-slicing trap: the
// preview used to cut the head at a byte offset, splitting a CJK character. The
// textarea coerced the stray byte to U+FFFD while the registry kept the raw
// byte, so the token no longer equalled itself and the payload was stranded even
// with exact-match expansion.
func TestCJKPasteTokenSurvivesRuneBoundaries(t *testing.T) {
	e := pasteEditor(1000, 10)
	e.textarea.SetValue("这几个服务发布一下")
	rows := []string{"# GCP发布准备 RUQX-380", "", "| 服务 | 版本 | 说明 |"}
	for i := 0; i < 14; i++ {
		rows = append(rows, fmt.Sprintf("| lid-tm-服务-%02d | v2026080411%02d | https://github.com/channelwill/lid-tm-service-%02d/releases/tag/v202608031139 |", i, i, i))
	}
	doc := strings.Join(rows, "\r")

	e.Update(pasteMsg(doc))

	display := e.Value()
	if strings.ContainsRune(display, '�') {
		t.Fatalf("token label carries a broken rune: %q", display)
	}
	expanded := e.ExpandValue()
	if pastetoken.ContainsUnresolved(expanded) {
		t.Fatalf("CJK token survived expansion: %q", truncateForLog(expanded))
	}
	if !strings.Contains(expanded, "# GCP发布准备 RUQX-380") || !strings.Contains(expanded, "lid-tm-服务-13") {
		t.Fatalf("expanded value lost CJK payload: %q", truncateForLog(expanded))
	}
}

// TestSmallPasteStaysInline keeps the threshold behavior: a short paste is
// ordinary text and must not be hidden behind a placeholder.
func TestSmallPasteStaysInline(t *testing.T) {
	e := pasteEditor(1000, 10)
	e.Update(pasteMsg("line one\rline two"))

	value := e.Value()
	if pastetoken.ContainsUnresolved(value) {
		t.Fatalf("small paste was tokenized: %q", value)
	}
	if !strings.Contains(value, "line one\nline two") {
		t.Fatalf("small paste lost its normalized line break: %q", value)
	}
}

// TestImageTokenWithBracketsInFileNameExpands covers the neighbouring risk the
// paste bug also exposed: "Screenshot [1].png" is a normal Windows file name and
// used to produce a token that no expansion pattern could match.
func TestImageTokenWithBracketsInFileNameExpands(t *testing.T) {
	e := pasteEditor(1000, 10)
	e.textarea.SetValue("看这张图")
	path := "/mnt/c/Users/u/Pictures/Screenshot [1].png"

	token := e.AttachImage(path)
	if strings.ContainsAny(strings.TrimSuffix(strings.TrimPrefix(token, "[[ "), " ]]"), "[]") {
		t.Fatalf("token keeps brackets from the file name: %q", token)
	}

	expanded := e.ExpandValue()
	if !strings.Contains(expanded, path) {
		t.Fatalf("expanded = %q, want the untouched path %q", expanded, path)
	}
	if pastetoken.ContainsUnresolved(expanded) {
		t.Fatalf("image token survived expansion: %q", expanded)
	}
}

// TestEditedTokenIsReportedNotSilentlySubmitted: once a person edits the token
// text its payload is unrecoverable. The composer must say so instead of sending
// literal placeholder text for the daemon to reject.
func TestEditedTokenIsReportedNotSilentlySubmitted(t *testing.T) {
	e := pasteEditor(1000, 10)
	e.Update(pasteMsg(crDocument(20)))

	edited := strings.Replace(e.Value(), "paste:0", "paste:1", 1)
	e.SetValue(edited)

	stranded := e.UnresolvedToken()
	if stranded == "" {
		t.Fatal("an edited token must be reported as unresolved")
	}
	if !strings.Contains(stranded, "paste:1") {
		t.Fatalf("UnresolvedToken = %q, want the edited token", stranded)
	}
}

func TestComposerSessionHistoryRestoresRichDraft(t *testing.T) {
	e := pasteEditor(1000, 10)
	e.SeedHistory(nil, 64*1024)
	e.textarea.SetValue("analyse these")
	e.Update(pasteMsg(crDocument(20)))
	e.AttachImage("/tmp/reference.png")

	wantDisplay := e.Value()
	wantExpanded := e.ExpandValue()
	submission := e.Submit(ComposerHistorySessionOnly)
	if submission.Persist {
		t.Fatal("a rich draft must not be written to cross-session history")
	}
	if e.Value() != "" {
		t.Fatalf("submitted composer = %q, want empty", e.Value())
	}

	e.HandleKey(tea.KeyMsg{Type: tea.KeyUp})
	if got := e.Value(); got != wantDisplay {
		t.Fatalf("recalled display = %q, want %q", got, wantDisplay)
	}
	if got := e.ExpandValue(); got != wantExpanded {
		t.Fatalf("recalled expanded input lost payload ownership: %q", got)
	}
}

func truncateForLog(s string) string {
	if len(s) <= 160 {
		return s
	}
	return s[:160] + "…"
}
