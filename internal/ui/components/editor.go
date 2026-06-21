package components

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"selfmind/internal/platform/config"
	"selfmind/internal/ui/common"
	"selfmind/internal/ui/layout"
)

// PasteSnippet stores the real content for a placeholder token.
type PasteSnippet struct {
	Token string // the placeholder, e.g. "[[ paste:0 PrivacyDistiller.. [80 lines] .. scrubPII ]]"
	Text  string // actual pasted content
}

// pasteTokenRe matches [[ paste:NNN ... ]] style placeholders in submitted text.
// Hermes format: [[ paste:0 PrivacyDistiller.. [80 lines] .. scrubPII ]]
var pasteTokenRe = regexp.MustCompile(`\[\[ paste:\d[^\]]*\]\]`)

// wsRe collapses whitespace in previews.
var wsRe = regexp.MustCompile(`\s+`)

const maxComposerInputLines = 4

// Editor wraps textarea + textinput with large-paste detection.
// When a multi-line paste exceeds the configured thresholds, the actual
// content is stored here and a placeholder token is shown in the textarea.
// Call Value() to get the display value (with placeholders).
// Call ExpandValue() to get the real content with placeholders replaced.
type Editor struct {
	common          *common.Common
	textarea        textarea.Model
	textinput       textinput.Model
	secure          bool
	commands        []CommandHint
	snippets        []PasteSnippet // stored snippets for large pastes
	largePasteChars int            // threshold in characters (from config, 0=disabled)
	largePasteLines int            // threshold in lines (from config, 0=disabled)
	cursorVisible   bool
}

type CommandHint struct {
	Name        string
	Description string
}

// NewEditor creates a new Editor component.
// editorCfg controls the large-paste detection thresholds (pass nil for defaults).
func NewEditor(c *common.Common, editorCfg *config.EditorConfig) *Editor {
	t := textarea.New()
	t.SetHeight(1)
	t.ShowLineNumbers = false
	t.Placeholder = "Ask SelfMind to inspect, change, test, or remember"
	t.Prompt = "" // Handled manually in Draw

	editorBG := lipgloss.Color("236")
	baseStyle := lipgloss.NewStyle().Background(editorBG)
	placeholderStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Background(editorBG)

	// Keep the textarea visually merged with the filled composer band.
	t.FocusedStyle.Base = baseStyle
	t.BlurredStyle.Base = baseStyle
	t.FocusedStyle.Text = c.Styles.Editor.Text
	t.BlurredStyle.Text = c.Styles.Editor.Text
	t.FocusedStyle.Placeholder = placeholderStyle
	t.BlurredStyle.Placeholder = placeholderStyle
	t.FocusedStyle.Prompt = baseStyle // Prompt handled manually
	t.BlurredStyle.Prompt = baseStyle
	t.FocusedStyle.CursorLine = baseStyle
	t.BlurredStyle.CursorLine = baseStyle

	t.Cursor.Style = c.Styles.Editor.Cursor
	t.Cursor.SetMode(cursor.CursorStatic)
	t.Cursor.Focus()

	// Override Enter so it does NOT insert newline — we use it for submit in controller.
	// Shift+Enter and Ctrl+J insert newlines instead.
	t.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "ctrl+j"),
		key.WithHelp("shift+enter", "new line"),
	)

	t.Focus()

	i := textinput.New()
	i.Placeholder = "   Enter secret value..."
	i.Prompt = ""
	i.EchoMode = textinput.EchoPassword
	i.EchoCharacter = '•'

	i.TextStyle = c.Styles.Editor.Text
	i.PlaceholderStyle = c.Styles.Subtle

	i.Cursor.Style = c.Styles.Editor.Cursor
	i.Cursor.SetMode(cursor.CursorStatic)
	i.Cursor.Focus()

	// Determine thresholds from config (0 = disabled).
	chars := 8000
	lines := 80
	if editorCfg != nil {
		if editorCfg.LargePasteChars > 0 {
			chars = editorCfg.LargePasteChars
		}
		if editorCfg.LargePasteLines > 0 {
			lines = editorCfg.LargePasteLines
		}
	}

	return &Editor{
		common:          c,
		textarea:        t,
		textinput:       i,
		largePasteChars: chars,
		largePasteLines: lines,
		cursorVisible:   true,
	}
}

// SetCommandHints sets slash-command suggestions supplied by the application.
func (e *Editor) SetCommandHints(commands []CommandHint) {
	e.commands = append([]CommandHint(nil), commands...)
}

// Update handles messages, intercepting paste events (both bracketed paste
// and Ctrl+V) for large-paste detection. When the pasted content exceeds
// LargePasteChars or LargePasteLines, a placeholder token is shown instead
// of the raw content, and the real content is stored for submit-time expansion.
func (e *Editor) Update(msg tea.Msg) tea.Cmd {
	if e.secure {
		var cmd tea.Cmd
		e.textinput, cmd = e.textinput.Update(msg)
		return cmd
	}

	// Detect paste events: Bubble Tea uses bracketed paste mode by default,
	// which creates KeyMsg{Paste: true} (not KeyCtrlV). We intercept here
	// before the textarea processes the paste so we can apply our threshold
	// check and replace large content with a compact placeholder token.
	if keyMsg, ok := msg.(tea.KeyMsg); ok && keyMsg.Paste {
		return e.handlePasteFromKey(keyMsg)
	}

	var cmd tea.Cmd
	e.textarea, cmd = e.textarea.Update(msg)
	return cmd
}

// handlePasteFromKey handles a paste event detected by Update.
// The pasted content comes from keyMsg.Runes (collected by Bubble Tea's
// bracketed paste handler). For all pastes we intercept and handle manually
// via SetValue so we can apply the threshold check for large-paste placeholders.
func (e *Editor) handlePasteFromKey(keyMsg tea.KeyMsg) tea.Cmd {
	// Extract pasted content from the key event (contains all runes from bracketed paste).
	pasted := string(keyMsg.Runes)
	if pasted == "" {
		return nil
	}

	// Clean trailing newlines (Hermes behavior).
	cleaned := stripTrailingPasteNewlines(pasted)

	// Count lines.
	lineCount := strings.Count(cleaned, "\n") + 1
	if lineCount == 0 {
		lineCount = 1
	}

	// Determine thresholds.
	chars := e.largePasteChars
	lines := e.largePasteLines

	// Check if this is a large paste (need both thresholds satisfied, or either if one is 0/disabled).
	isLarge := false
	if chars > 0 && lines > 0 {
		isLarge = len(cleaned) >= chars || lineCount >= lines
	} else if chars > 0 {
		isLarge = len(cleaned) >= chars
	} else if lines > 0 {
		isLarge = lineCount >= lines
	}
	// If both are 0, large paste is disabled — treat as small always.

	var display string
	if isLarge {
		// Large paste: store content, show placeholder token.
		idx := len(e.snippets)
		label := pasteTokenLabel(cleaned, lineCount)
		display = fmt.Sprintf("[[ paste:%d %s ]]", idx, label)
		e.snippets = append(e.snippets, PasteSnippet{Token: display, Text: cleaned})
	} else {
		// Small paste: show raw content.
		display = cleaned
	}

	// Append display content to current value. Add leading space if needed.
	currentValue := e.textarea.Value()
	lead := ""
	if len(currentValue) > 0 {
		last := rune(currentValue[len(currentValue)-1])
		if last != ' ' && last != '\t' && last != '\n' {
			lead = " "
		}
	}
	e.textarea.SetValue(currentValue + lead + display)

	return nil
}

// Value returns the current textarea/textinput value (may contain placeholders).
func (e *Editor) Value() string {
	if e.secure {
		return e.textinput.Value()
	}
	return e.textarea.Value()
}

// ExpandValue replaces paste placeholders with the actual clipboard content.
// Call this before submitting.
func (e *Editor) ExpandValue() string {
	val := e.Value()
	if len(e.snippets) == 0 {
		return val
	}
	// Build a map from token → text for efficient replacement.
	snipMap := make(map[string]string)
	for _, s := range e.snippets {
		snipMap[s.Token] = s.Text
	}
	// Replace all occurrences of each placeholder.
	result := pasteTokenRe.ReplaceAllStringFunc(val, func(match string) string {
		if text, ok := snipMap[match]; ok {
			return text
		}
		return match
	})
	return result
}

// Reset clears the editor and all stored paste snippets.
func (e *Editor) Reset() {
	e.textarea.Reset()
	e.textinput.Reset()
	e.snippets = nil
}

// SetValue sets the textarea/textinput value without affecting paste snippets.
func (e *Editor) SetValue(s string) {
	if e.secure {
		e.textinput.SetValue(s)
	} else {
		e.textarea.SetValue(s)
	}
}

// SetCursorVisible controls the manually rendered textarea cursor blink.
func (e *Editor) SetCursorVisible(visible bool) {
	e.cursorVisible = visible
}

// SetSecure toggles secure (password) input mode.
func (e *Editor) SetSecure(secure bool) {
	e.secure = secure
	if secure {
		e.textinput.Focus()
		e.textarea.Blur()
	} else {
		e.textarea.Focus()
		e.textinput.Blur()
	}
}

// IsSecure returns whether the editor is in secure input mode.
func (e *Editor) IsSecure() bool {
	return e.secure
}

// GetSuggestion returns slash-command completions for the current input.
func (e *Editor) GetSuggestion() string {
	matches := e.matchingCommands()
	if len(matches) == 0 {
		return ""
	}
	rows := make([]string, 0, len(matches))
	for _, cmd := range matches {
		rows = append(rows, cmd.Name+" "+cmd.Description)
	}
	return strings.Join(rows, "\n")
}

// PreferredHeight returns the number of terminal rows the composer needs.
func (e *Editor) PreferredHeight() int {
	if e.secure {
		return 3
	}
	return e.visibleInputLineCount() + 2 + len(e.matchingCommands())
}

// Draw renders the editor into the given layout rect.
func (e *Editor) Draw(rect layout.Rect) string {
	availableW := rect.W - 2
	if availableW < 10 {
		availableW = rect.W
	}

	if e.secure {
		prompt := e.common.Styles.Editor.Prompt.Render(" secret › ")
		inputW := availableW - 10
		if inputW < 1 {
			inputW = 1
		}
		e.textinput.Width = inputW
		return e.common.Styles.Editor.Panel.
			Width(rect.W).
			Render(lipgloss.JoinHorizontal(lipgloss.Top, prompt, e.textinput.View()))
	}

	inputH := e.visibleInputLineCount()
	prompt := e.common.Styles.Editor.Prompt.Render("› ")
	e.textarea.SetHeight(inputH)
	textW := availableW - 2
	if textW < 1 {
		textW = 1
	}
	e.textarea.SetWidth(textW)
	view := renderEditorValue(e.textarea.Value(), e.textarea.Placeholder, inputH, textW, e.common.Styles.Editor.Cursor, e.cursorVisible, e.textarea.Line(), e.textarea.LineInfo())

	input := e.common.Styles.Editor.Panel.
		Width(rect.W).
		Render(lipgloss.JoinHorizontal(lipgloss.Top, prompt, view))

	suggestions := e.renderSuggestions(rect.W)
	if suggestions == "" {
		return input
	}
	return lipgloss.JoinVertical(lipgloss.Left, input, suggestions)
}

// ─── Helpers ───────────────────────────────────────────────────────────────

func (e *Editor) visibleInputLineCount() int {
	lines := strings.Count(e.textarea.Value(), "\n") + 1
	if lines < 1 {
		return 1
	}
	if lines > maxComposerInputLines {
		return maxComposerInputLines
	}
	return lines
}

func (e *Editor) matchingCommands() []CommandHint {
	val := strings.TrimSpace(e.textarea.Value())
	if val == "" || !strings.HasPrefix(val, "/") || strings.Contains(val, " ") || strings.Contains(val, "\n") {
		return nil
	}

	var matches []CommandHint
	for _, cmd := range e.commands {
		if strings.HasPrefix(cmd.Name, val) {
			matches = append(matches, cmd)
			if len(matches) >= 8 {
				break
			}
		}
	}
	return matches
}

func (e *Editor) renderSuggestions(width int) string {
	matches := e.matchingCommands()
	if len(matches) == 0 {
		return ""
	}

	nameStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("75")).Bold(true).Width(14)
	descStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	innerW := width - 2
	if innerW < 1 {
		innerW = 1
	}
	descW := innerW - 16
	if descW < 12 {
		descW = 12
	}

	rows := make([]string, 0, len(matches))
	for _, cmd := range matches {
		desc := truncateASCII(cmd.Description, descW)
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, nameStyle.Render(cmd.Name), descStyle.Render(desc)))
	}

	return lipgloss.NewStyle().
		Width(innerW).
		PaddingLeft(2).
		Render(strings.Join(rows, "\n"))
}

func renderEditorValue(value, placeholder string, height, width int, cursorStyle lipgloss.Style, cursorVisible bool, cursorLine int, lineInfo textarea.LineInfo) string {
	if height < 1 {
		height = 1
	}
	if width < 1 {
		width = 1
	}
	if value == "" {
		return renderEmptyEditorLine(placeholder, width, cursorStyle, cursorVisible)
	}

	lines := strings.Split(value, "\n")
	start := 0
	if len(lines) > height {
		start = len(lines) - height
		if cursorLine < start {
			start = cursorLine
		}
		if cursorLine >= start+height {
			start = cursorLine - height + 1
		}
		if start < 0 {
			start = 0
		}
	}
	visible := append([]string{}, lines[start:]...)
	for len(visible) < height {
		visible = append(visible, "")
	}

	bg := lipgloss.Color("236")
	lineStyle := lipgloss.NewStyle().Background(bg).Width(width)
	textStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Background(bg)

	for i, line := range visible {
		globalLine := start + i
		isCursorLine := globalLine == cursorLine
		var rendered string
		if isCursorLine {
			rendered = renderEditorCursorLine(line, width, textStyle, cursorStyle, cursorVisible, lineInfo)
		} else {
			rendered = textStyle.Render(truncateDisplayWidth(line, width))
		}
		visible[i] = lineStyle.Render(rendered)
	}
	return strings.Join(visible, "\n")
}

func renderEditorCursorLine(line string, width int, textStyle, cursorStyle lipgloss.Style, cursorVisible bool, lineInfo textarea.LineInfo) string {
	before, cursorText, after := editorCursorParts(line, width, lineInfo)
	if !cursorVisible {
		return textStyle.Render(before + cursorText + after)
	}
	return textStyle.Render(before) + cursorStyle.Render(cursorText) + textStyle.Render(after)
}

func editorCursorParts(line string, width int, lineInfo textarea.LineInfo) (string, string, string) {
	if width < 1 {
		width = 1
	}
	runes := []rune(line)
	start := lineInfo.StartColumn
	if start < 0 {
		start = 0
	}
	if start > len(runes) {
		start = len(runes)
	}
	cursorOffset := lineInfo.ColumnOffset
	if cursorOffset < 0 {
		cursorOffset = 0
	}
	if start+cursorOffset > len(runes) {
		cursorOffset = len(runes) - start
	}
	segment := runes[start:]
	if cursorOffset > len(segment) {
		cursorOffset = len(segment)
	}

	before := string(segment[:cursorOffset])
	beforeWidth := runewidth.StringWidth(before)
	if beforeWidth >= width {
		before = ""
		beforeWidth = 0
	}

	cursorText := " "
	afterStart := cursorOffset
	if cursorOffset < len(segment) {
		cursorText = string(segment[cursorOffset])
		afterStart = cursorOffset + 1
	}
	cursorWidth := runewidth.StringWidth(cursorText)
	if cursorWidth < 1 {
		cursorWidth = 1
	}
	afterWidth := width - beforeWidth - cursorWidth
	if afterWidth < 0 {
		afterWidth = 0
	}
	after := ""
	if afterStart < len(segment) {
		after = truncateDisplayWidth(string(segment[afterStart:]), afterWidth)
	}
	return before, cursorText, after
}

func renderEmptyEditorLine(placeholder string, width int, cursorStyle lipgloss.Style, cursorVisible bool) string {
	if width < 1 {
		width = 1
	}
	bg := lipgloss.Color("236")
	lineStyle := lipgloss.NewStyle().Background(bg).Width(width)
	placeholderStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Background(bg)

	cursorStyleToUse := lipgloss.NewStyle().Background(bg)
	if cursorVisible {
		cursorStyleToUse = cursorStyle
	}
	cursor := cursorStyleToUse.Render(" ")
	available := width - 1
	if available < 0 {
		available = 0
	}
	text := truncateASCII(placeholder, available)
	return lineStyle.Render(cursor + placeholderStyle.Render(text))
}

func truncateDisplayWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	var sb strings.Builder
	used := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if used+rw > width {
			break
		}
		sb.WriteRune(r)
		used += rw
	}
	return sb.String()
}

func truncateASCII(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(s) <= width {
		return s
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}
	return s[:width-3] + "..."
}

// stripTrailingPasteNewlines removes trailing newlines from pasted text,
// but only if there's actual content (not just whitespace/newlines).
func stripTrailingPasteNewlines(text string) string {
	if len(text) == 0 {
		return text
	}
	// Check if there's any non-newline content.
	hasContent := false
	for _, r := range text {
		if r != '\n' && r != '\r' {
			hasContent = true
			break
		}
	}
	if !hasContent {
		return ""
	}
	return strings.TrimRight(text, "\r\n")
}

// edgePreview returns a short preview of text showing head and tail.
// head: first N chars, tail: last M chars. If text fits, returns it unchanged.
func edgePreview(s string, head int, tail int) string {
	if s == "" {
		return ""
	}
	// Collapse whitespace for preview (same as Hermes).
	one := wsRe.ReplaceAllString(s, " ")
	one = strings.TrimSpace(one)
	if len(one) <= head+tail+4 {
		return one
	}
	return strings.TrimRight(one[:head], " \t") + ".. " + strings.TrimLeft(one[len(one)-tail:], " \t")
}

// fmtK formats a number in compact form (e.g. 1234 → "1.2K").
// Uses the same approach as Hermes: Intl.NumberFormat with compact notation.
func fmtK(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1_000_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
}

// pasteTokenLabel generates a user-visible placeholder label matching Hermes exactly:
// "PrivacyDistiller implementation.. [80 lines] .. scrubPII"
// or: "[80 lines]" if no preview available.
func pasteTokenLabel(text string, lineCount int) string {
	preview := edgePreview(text, 16, 28)
	if preview == "" {
		return fmt.Sprintf("[%s lines]", fmtK(lineCount))
	}
	// Split preview on ".. " boundary (from edgePreview).
	if idx := strings.Index(preview, ".. "); idx >= 0 {
		head := strings.TrimRight(preview[:idx], " \t")
		tail := strings.TrimLeft(preview[idx+3:], " \t")
		return fmt.Sprintf("%s.. [%s lines] .. %s", head, fmtK(lineCount), tail)
	}
	return fmt.Sprintf("%s [%s lines]", preview, fmtK(lineCount))
}
