package components

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/pastetoken"
	"selfmind/internal/ui/common"
	"selfmind/internal/ui/layout"
)

// PasteSnippet stores the real content for a placeholder token.
type PasteSnippet struct {
	Token string // the placeholder, e.g. "[Paste #1 · 80 lines]"
	Text  string // actual pasted content
}

// ImageAttachment stores a composer-attached local image behind a display
// token, mirroring PasteSnippet: the raw absolute path never appears in the
// input line (a leading "/mnt/…" both reads badly and used to be mistaken for
// a slash command); ExpandValue substitutes the path back at submit time so
// the existing path→attachment pipeline picks it up unchanged.
type ImageAttachment struct {
	Token string // the placeholder, e.g. "[Image #1 · screenshot.png]"
	Path  string // absolute local file path
	Name  string // display base name
}

// pasteLineBreakRe matches every line separator a terminal can deliver. A
// bracketed paste arrives with bare CR in Windows Terminal/WSL, so counting
// only "\n" reported every multi-line document as "1 lines" and left the
// configured line threshold permanently unreachable.
var pasteLineBreakRe = regexp.MustCompile(`\r\n|\r|\n`)

const maxComposerInputLines = 6

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
	skillFilter     func(query string) []CommandHint
	snippets        []PasteSnippet    // stored snippets for large pastes
	images          []ImageAttachment // stored attachments for pasted/attached images
	largePasteChars int               // threshold in characters (from config, 0=disabled)
	largePasteLines int               // threshold in lines (from config, 0=disabled)
	cursorVisible   bool
	hintIndex       int // selected row in the slash-command suggestion popup
	layoutWidth     int // last known total composer width, for wrap-aware height
	layoutHeight    int // last known terminal height, for proportional height cap
	history         []composerDraft
	historyIndex    int
	historyBytes    int64
	historyMaxBytes int64
	dismissedToken  string
}

type CommandHint struct {
	Name        string
	Description string
	// Insert is what completion writes when this hint is chosen. It defaults to
	// Name; a Skill hint uses it to write the reference that resolves to exactly
	// one package while the row still shows a readable name.
	Insert string
}

func (h CommandHint) insertText() string {
	if strings.TrimSpace(h.Insert) != "" {
		return h.Insert
	}
	return h.Name
}

// NewEditor creates a new Editor component.
// editorCfg controls the large-paste detection thresholds (pass nil for defaults).
func NewEditor(c *common.Common, editorCfg *config.EditorConfig) *Editor {
	t := textarea.New()
	t.SetHeight(1)
	t.ShowLineNumbers = false
	t.Placeholder = "Ask SelfMind to inspect, change, test, or remember"
	t.Prompt = "" // Handled manually in Draw

	baseStyle := c.Styles.Editor.Text.Copy().UnsetForeground()
	placeholderStyle := c.Styles.Editor.Placeholder

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

	// Override Enter so it does NOT insert a newline — the controller uses it
	// for submit. Ctrl+J is the portable multiline key: legacy terminal input
	// cannot distinguish Shift+Enter from plain Enter.
	t.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("ctrl+j"),
		key.WithHelp("ctrl+j", "new line"),
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
		historyIndex:    -1,
	}
}

// SetCommandHints sets slash-command suggestions supplied by the application.
func (e *Editor) SetCommandHints(commands []CommandHint) {
	e.commands = append([]CommandHint(nil), commands...)
}

// SetSkillFilter installs the application's matcher for `$` completion. The
// application owns matching so completion can reuse the same metadata ranker the
// Skill catalog uses instead of a second, weaker prefix test, and so the
// inventory can be fetched where usage recency lives. Until one is installed the
// `$` popup stays closed rather than offering a weaker match.
func (e *Editor) SetSkillFilter(fn func(query string) []CommandHint) {
	e.skillFilter = fn
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

	before := e.textarea.Value()
	var cmd tea.Cmd
	e.textarea, cmd = e.textarea.Update(msg)
	// Any actual edit resets the popup selection to the top (navigation keys are
	// intercepted by the controller and never reach here, so this only fires on
	// real text changes).
	if e.textarea.Value() != before {
		e.hintIndex = 0
		e.dismissedToken = ""
	}
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

	// Normalize line endings before anything measures or stores the payload: a
	// terminal paste may separate lines with bare CR, which both hid the real
	// line count and would have reached the model as one long line.
	cleaned := stripTrailingPasteNewlines(normalizePasteNewlines(pasted))

	// Count lines across every separator form.
	lineCount := len(pasteLineBreakRe.FindAllString(cleaned, -1)) + 1
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
		// Large paste: store content, show placeholder token. The token text is
		// the registry key ExpandValue substitutes back, so it must come from
		// pastetoken.Format — the only builder that guarantees a bracket-free,
		// single-line token.
		idx := len(e.snippets)
		label := pasteTokenLabel(cleaned, lineCount)
		display = pastetoken.Format(pastetoken.KindPaste, idx, label)
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
	e.hintIndex = 0
	e.dismissedToken = ""

	return nil
}

// Value returns the current textarea/textinput value (may contain placeholders).
func (e *Editor) Value() string {
	if e.secure {
		return e.textinput.Value()
	}
	return e.textarea.Value()
}

// AttachImage registers a local image file and appends its compact display
// token to the composer. A token the user deletes before submitting simply
// never expands — deleting the token IS detaching the image. Returns the token.
func (e *Editor) AttachImage(path string) string {
	idx := len(e.images)
	name := filepath.Base(path)
	// A file name may legitimately contain brackets ("Screenshot [1].png"); the
	// sanitized label keeps the token well-formed while the untouched Path stays
	// the value ExpandValue substitutes back.
	token := pastetoken.Format(pastetoken.KindImage, idx, name)
	e.images = append(e.images, ImageAttachment{Token: token, Path: path, Name: name})

	current := e.textarea.Value()
	lead := ""
	if len(current) > 0 {
		last := current[len(current)-1]
		if last != ' ' && last != '\t' && last != '\n' {
			lead = " "
		}
	}
	e.textarea.SetValue(current + lead + token + " ")
	e.hintIndex = 0
	e.dismissedToken = ""
	return token
}

// ActiveImageCount reports attachments whose exact token is still present in
// the editable draft. The registry retains payload ownership until submit or
// reset, but deleting a token immediately detaches it from the visible and
// outgoing message state.
func (e *Editor) ActiveImageCount() int {
	if e == nil || e.secure {
		return 0
	}
	value := e.textarea.Value()
	count := 0
	for _, image := range e.images {
		if strings.Contains(value, image.Token) {
			count++
		}
	}
	return count
}

// ExpandValue replaces paste placeholders with the actual clipboard content
// and image placeholders with their local file paths. Call this before
// submitting.
//
// Substitution is EXACT: every registered token is replaced by its payload
// through a plain string replacer. Matching by pattern instead made expansion
// depend on the label's shape, and one bracket in the label ("[80 lines]") was
// enough to strand every large paste as literal placeholder text.
func (e *Editor) ExpandValue() string {
	val := e.Value()
	if len(e.snippets) == 0 && len(e.images) == 0 {
		return val
	}
	pairs := make([]string, 0, 2*(len(e.snippets)+len(e.images)))
	for _, s := range e.snippets {
		pairs = append(pairs, s.Token, s.Text)
	}
	for _, img := range e.images {
		pairs = append(pairs, img.Token, img.Path)
	}
	return strings.NewReplacer(pairs...).Replace(val)
}

// UnresolvedToken returns a current-format placeholder that survived
// ExpandValue, or "" when the value is safe to submit. It fires when a token
// was edited or pasted without a matching client-owned payload. Former `[[...]]`
// placeholders are ordinary text and deliberately have no compatibility path.
func (e *Editor) UnresolvedToken() string {
	return pastetoken.FindUnresolved(e.ExpandValue())
}

// Reset clears the editor, all stored paste snippets, and image attachments.
func (e *Editor) Reset() {
	e.textarea.Reset()
	e.textinput.Reset()
	e.snippets = nil
	e.images = nil
	e.historyIndex = -1
	e.hintIndex = 0
	e.dismissedToken = ""
}

// SetValue replaces the externally supplied draft and exits history browsing.
// Internal history recall uses applyHistoryDraft so it can retain navigation
// ownership and the recalled draft's paste/image payloads.
func (e *Editor) SetValue(s string) {
	e.historyIndex = -1
	e.hintIndex = 0
	e.dismissedToken = ""
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
	e.historyIndex = -1
	e.hintIndex = 0
	e.dismissedToken = ""
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
	return e.visibleInputLineCount() + 2 + e.suggestionHeight()
}

// SetLayout records terminal dimensions so PreferredHeight can account for
// soft wrapping and cap long drafts to one third of the visible terminal.
func (e *Editor) SetLayout(width, height int) {
	e.SetLayoutWidth(width)
	if height > 0 {
		e.layoutHeight = height
	}
}

// SetLayoutWidth remains available to narrow component callers and tests.
func (e *Editor) SetLayoutWidth(w int) {
	if w > 0 {
		e.layoutWidth = w
	}
}

// CursorAtTextBoundary reports whether the cursor sits at the very start or
// very end of the whole input. History navigation (codex-style) only replaces
// the text from a boundary; interior positions keep Up/Down as cursor moves.
func (e *Editor) CursorAtTextBoundary() bool {
	if e.secure {
		return true
	}
	value := e.textarea.Value()
	if value == "" {
		return true
	}
	row := e.textarea.Line()
	li := e.textarea.LineInfo()
	col := li.StartColumn + li.ColumnOffset
	if row == 0 && col == 0 {
		return true
	}
	lines := strings.Split(value, "\n")
	last := len(lines) - 1
	return row == last && col >= len([]rune(lines[last]))
}

// Draw renders the editor into the given layout rect.
func (e *Editor) Draw(rect layout.Rect) string {
	e.SetLayoutWidth(rect.W)
	width := max(1, rect.W)
	borderStyle := lipgloss.NewStyle().Foreground(e.common.Styles.Border)
	labelStyle := lipgloss.NewStyle().Foreground(e.common.Styles.Primary).Bold(true)
	hintStyle := e.common.Styles.Subtle

	if e.secure {
		prompt := e.common.Styles.Editor.Prompt.Render("› ")
		inputW := width - 2
		if inputW < 1 {
			inputW = 1
		}
		e.textinput.Width = inputW
		body := lipgloss.JoinHorizontal(lipgloss.Top, prompt, e.textinput.View())
		return lipgloss.JoinVertical(lipgloss.Left,
			renderOpenBoundary("Secret", nil, width, borderStyle, labelStyle, hintStyle),
			body,
			borderStyle.Render(strings.Repeat("─", width)),
		)
	}

	inputH := e.visibleInputLineCount()
	prompt := e.common.Styles.Editor.Prompt.Render("› ")
	e.textarea.SetHeight(inputH)
	textW := editorTextWidth(width)
	e.textarea.SetWidth(textW)
	view, viewport := renderEditorValue(e.textarea.Value(), e.textarea.Placeholder, inputH, textW,
		e.common.Styles.Editor.Text, e.common.Styles.Editor.Placeholder, e.common.Styles.Editor.Cursor,
		e.cursorVisible, e.textarea.Line(), e.textarea.LineInfo())

	label := e.composerLabel(viewport)
	input := lipgloss.JoinVertical(lipgloss.Left,
		renderOpenBoundary(label, e.composerBoundaryHints(), width, borderStyle, labelStyle, hintStyle),
		lipgloss.JoinHorizontal(lipgloss.Top, prompt, view),
		borderStyle.Render(strings.Repeat("─", width)),
	)

	suggestions := e.renderSuggestions(width)
	if suggestions == "" {
		return input
	}
	return lipgloss.JoinVertical(lipgloss.Left, input, suggestions)
}

// ─── Helpers ───────────────────────────────────────────────────────────────

// visibleInputLineCount counts display rows including soft-wrapped long lines,
// not just hard newlines, so a wrapped paragraph is fully visible while typing.
func (e *Editor) visibleInputLineCount() int {
	value := e.textarea.Value()
	lines := strings.Count(value, "\n") + 1
	if e.layoutWidth > 0 && value != "" {
		lines = editorWrappedRowCount(value, editorTextWidth(e.layoutWidth))
	}
	if lines < 1 {
		return 1
	}
	if maxRows := e.maxInputRows(); lines > maxRows {
		return maxRows
	}
	return lines
}

func (e *Editor) maxInputRows() int {
	if e.layoutHeight <= 0 {
		return maxComposerInputLines
	}
	rows := e.layoutHeight / 3
	if rows < 2 {
		rows = 2
	}
	if rows > maxComposerInputLines {
		rows = maxComposerInputLines
	}
	return rows
}

// editorTextWidth maps the composer's total width to the text-column width,
// mirroring the prompt and padding math in Draw.
func editorTextWidth(rectW int) int {
	textW := rectW - 2
	if textW < 1 {
		textW = 1
	}
	return textW
}

// wrapEditorLine soft-wraps one logical line exactly like the embedded bubbles
// textarea (word wrap with a trailing cursor-cell space), so rendered rows stay
// aligned with textarea.LineInfo RowOffset/ColumnOffset.
func wrapEditorLine(runes []rune, width int) [][]rune {
	if width < 1 {
		width = 1
	}
	var (
		lines  = [][]rune{{}}
		word   = []rune{}
		row    int
		spaces int
	)

	for _, r := range runes {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}

		if spaces > 0 {
			if uniseg.StringWidth(string(lines[row]))+uniseg.StringWidth(string(word))+spaces > width {
				row++
				lines = append(lines, []rune{})
				lines[row] = append(lines[row], word...)
				lines[row] = append(lines[row], editorRepeatSpaces(spaces)...)
			} else {
				lines[row] = append(lines[row], word...)
				lines[row] = append(lines[row], editorRepeatSpaces(spaces)...)
			}
			spaces = 0
			word = nil
		} else {
			lastCharLen := runewidth.RuneWidth(word[len(word)-1])
			if uniseg.StringWidth(string(word))+lastCharLen > width {
				if len(lines[row]) > 0 {
					row++
					lines = append(lines, []rune{})
				}
				lines[row] = append(lines[row], word...)
				word = nil
			}
		}
	}

	if uniseg.StringWidth(string(lines[row]))+uniseg.StringWidth(string(word))+spaces >= width {
		lines = append(lines, []rune{})
		lines[row+1] = append(lines[row+1], word...)
		spaces++
		lines[row+1] = append(lines[row+1], editorRepeatSpaces(spaces)...)
	} else {
		lines[row] = append(lines[row], word...)
		spaces++
		lines[row] = append(lines[row], editorRepeatSpaces(spaces)...)
	}

	return lines
}

func editorRepeatSpaces(n int) []rune {
	return []rune(strings.Repeat(" ", n))
}

// editorWrappedRowCount returns the number of soft-wrapped display rows the
// value occupies at the given text width.
func editorWrappedRowCount(value string, width int) int {
	rows := 0
	for _, line := range strings.Split(value, "\n") {
		rows += len(wrapEditorLine([]rune(line), width))
	}
	if rows < 1 {
		rows = 1
	}
	return rows
}

// suggestionRows is the number of popup rows shown at once. It windows the
// rendering only; every match stays selectable.
const suggestionRows = 8

func (e *Editor) suggestionHeight() int {
	n := len(e.matchingCommands())
	if n == 0 {
		return 0
	}
	height := min(n, suggestionRows)
	if n > suggestionRows {
		height++ // position footer rendered below the window
	}
	return height
}

func (e *Editor) matchingCommands() []CommandHint {
	if e.browsingHistory() {
		return nil
	}
	raw := e.textarea.Value()
	val := strings.TrimSpace(raw)
	// Check the raw value for spaces/newlines (not the trimmed one) so a trailing
	// space — e.g. just after Tab-completing a command — closes the popup.
	if val == "" || strings.Contains(raw, " ") || strings.Contains(raw, "\n") {
		return nil
	}
	if raw == e.dismissedToken {
		return nil
	}
	var pool []CommandHint
	switch {
	case strings.HasPrefix(val, "/"):
		pool = e.commands
	case strings.HasPrefix(val, "$"):
		if e.skillFilter == nil {
			return nil
		}
		return e.skillFilter(strings.TrimPrefix(val, "$"))
	default:
		return nil
	}

	var matches []CommandHint
	for _, cmd := range pool {
		if strings.HasPrefix(cmd.Name, val) {
			matches = append(matches, cmd)
		}
	}
	return matches
}

// SuggestionsVisible reports whether the slash-command popup is currently shown.
func (e *Editor) SuggestionsVisible() bool {
	return len(e.matchingCommands()) > 0
}

// MoveSuggestion moves the popup selection by delta (wrapping), so the controller
// can map Up/Down to it instead of input history while the popup is open.
func (e *Editor) MoveSuggestion(delta int) {
	n := len(e.matchingCommands())
	if n == 0 {
		return
	}
	e.hintIndex = ((e.hintIndex+delta)%n + n) % n
}

// AcceptSuggestion completes the input to the currently selected command and
// returns true when a suggestion was applied (Tab behaviour).
func (e *Editor) AcceptSuggestion() bool {
	matches := e.matchingCommands()
	if len(matches) == 0 {
		return false
	}
	idx := e.clampedHintIndex(len(matches))
	e.textarea.SetValue(matches[idx].insertText() + " ")
	e.textarea.CursorEnd()
	e.hintIndex = 0
	e.dismissedToken = ""
	return true
}

func (e *Editor) clampedHintIndex(n int) int {
	if e.hintIndex >= n {
		return n - 1
	}
	if e.hintIndex < 0 {
		return 0
	}
	return e.hintIndex
}

func (e *Editor) renderSuggestions(width int) string {
	matches := e.matchingCommands()
	if len(matches) == 0 {
		return ""
	}
	selected := e.clampedHintIndex(len(matches))
	start := 0
	if selected >= suggestionRows {
		start = selected - suggestionRows + 1
	}
	end := start + suggestionRows
	if end > len(matches) {
		end = len(matches)
	}
	window := matches[start:end]

	// codex-style: highlight the selected row by foreground color only (bright
	// cyan for the whole row), dim the rest. No background blocks — those read as
	// mismatched bars over a themed/transparent terminal background.
	nameStyle := e.common.Styles.Editor.CompletionName
	descStyle := e.common.Styles.Editor.CompletionDescription
	selNameStyle := e.common.Styles.Editor.CompletionSelectedName
	selDescStyle := e.common.Styles.Editor.CompletionSelectedDescription
	innerW := width - 2
	if innerW < 1 {
		innerW = 1
	}
	descW := innerW - 16
	if descW < 12 {
		descW = 12
	}

	rows := make([]string, 0, len(window))
	for i, cmd := range window {
		desc := truncateASCII(cmd.Description, descW)
		ns, ds := nameStyle, descStyle
		if start+i == selected {
			ns, ds = selNameStyle, selDescStyle
		}
		rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, ns.Render(cmd.Name), ds.Render(desc)))
	}
	if len(matches) > len(window) {
		rows = append(rows, descStyle.Render(fmt.Sprintf("  %d of %d", selected+1, len(matches))))
	}

	return lipgloss.NewStyle().
		Width(innerW).
		PaddingLeft(2).
		Render(strings.Join(rows, "\n"))
}

type editorViewport struct {
	start int // one-based visible row
	end   int // one-based visible row
	total int
}

func (v editorViewport) overflowed() bool {
	return v.total > 0 && v.total > v.end-v.start+1
}

func (e *Editor) composerLabel(viewport editorViewport) string {
	label := "Message"
	if position, total, ok := e.HistoryPosition(); ok {
		label = fmt.Sprintf("History %d/%d", position, total)
	}
	if viewport.overflowed() {
		lines := fmt.Sprintf("Lines %d–%d/%d", viewport.start, viewport.end, viewport.total)
		if label == "Message" {
			return lines
		}
		return label + " · " + lines
	}
	return label
}

func (e *Editor) composerBoundaryHints() []string {
	count := e.ActiveImageCount()
	if count == 0 {
		return []string{"Ctrl+J newline · Ctrl+V image", "Ctrl+V image"}
	}
	imageLabel := fmt.Sprintf("%d images", count)
	if count == 1 {
		imageLabel = "1 image"
	}
	return []string{
		imageLabel + " · Ctrl+J newline · Ctrl+V more",
		imageLabel + " · Ctrl+V more",
		imageLabel,
	}
}

func renderOpenBoundary(label string, hints []string, width int, borderStyle, labelStyle, hintStyle lipgloss.Style) string {
	if width < 1 {
		return ""
	}
	const prefix = "── "
	available := width - runewidth.StringWidth(prefix) - 1
	if available < 1 {
		return borderStyle.Render(strings.Repeat("─", width))
	}
	label = truncateDisplayWidth(label, available)
	used := runewidth.StringWidth(prefix) + runewidth.StringWidth(label) + 1
	for _, hint := range hints {
		const minimumRuleWidth = 3
		hintWidth := runewidth.StringWidth(hint)
		if used+minimumRuleWidth+1+hintWidth > width {
			continue
		}
		tail := width - used - 1 - hintWidth
		return borderStyle.Render(prefix) + labelStyle.Render(label) +
			borderStyle.Render(" "+strings.Repeat("─", tail)) + " " + hintStyle.Render(hint)
	}

	tail := width - used
	if tail < 0 {
		tail = 0
	}
	return borderStyle.Render(prefix) + labelStyle.Render(label) + borderStyle.Render(" "+strings.Repeat("─", tail))
}

func renderEditorValue(value, placeholder string, height, width int, textStyle, placeholderStyle, cursorStyle lipgloss.Style, cursorVisible bool, cursorLine int, lineInfo textarea.LineInfo) (string, editorViewport) {
	if height < 1 {
		height = 1
	}
	if width < 1 {
		width = 1
	}
	if value == "" {
		return renderEmptyEditorLine(placeholder, width, placeholderStyle, cursorStyle, cursorVisible), editorViewport{start: 1, end: 1, total: 1}
	}

	// Soft-wrap every logical line the same way the underlying textarea does,
	// so the cursor's RowOffset/ColumnOffset map directly onto display rows.
	logical := strings.Split(value, "\n")
	rows := make([]string, 0, len(logical))
	cursorRow := -1
	cursorOffset := 0
	for li, line := range logical {
		wrapped := wrapEditorLine([]rune(line), width)
		if li == cursorLine {
			ro := lineInfo.RowOffset
			if ro < 0 {
				ro = 0
			}
			if ro >= len(wrapped) {
				ro = len(wrapped) - 1
			}
			cursorRow = len(rows) + ro
			cursorOffset = lineInfo.ColumnOffset
		}
		for _, r := range wrapped {
			rows = append(rows, string(r))
		}
	}
	if cursorRow < 0 || cursorRow >= len(rows) {
		cursorRow = len(rows) - 1
		cursorOffset = len([]rune(rows[cursorRow]))
	}

	// Scroll the visible window so the cursor row is always on screen.
	start := 0
	if len(rows) > height {
		start = len(rows) - height
		if cursorRow < start {
			start = cursorRow
		}
		if cursorRow >= start+height {
			start = cursorRow - height + 1
		}
		if start < 0 {
			start = 0
		}
	}
	end := start + height
	if end > len(rows) {
		end = len(rows)
	}
	visible := append([]string{}, rows[start:end]...)
	for len(visible) < height {
		visible = append(visible, "")
	}

	lineStyle := textStyle.Copy().UnsetForeground().Width(width)

	for i, row := range visible {
		globalRow := start + i
		var rendered string
		if globalRow == cursorRow {
			rendered = renderEditorCursorLine(row, width, textStyle, cursorStyle, cursorVisible, cursorOffset)
		} else {
			rendered = textStyle.Render(truncateDisplayWidth(row, width))
		}
		visible[i] = lineStyle.Render(rendered)
	}
	return strings.Join(visible, "\n"), editorViewport{start: start + 1, end: end, total: len(rows)}
}

func renderEditorCursorLine(row string, width int, textStyle, cursorStyle lipgloss.Style, cursorVisible bool, cursorOffset int) string {
	before, cursorText, after := editorCursorParts(row, width, cursorOffset)
	if !cursorVisible {
		return textStyle.Render(before + cursorText + after)
	}
	return textStyle.Render(before) + cursorStyle.Render(cursorText) + textStyle.Render(after)
}

// editorCursorParts splits one display row around the cursor. cursorOffset is
// the rune offset of the cursor within the row (LineInfo.ColumnOffset).
func editorCursorParts(row string, width int, cursorOffset int) (string, string, string) {
	if width < 1 {
		width = 1
	}
	runes := []rune(row)
	if cursorOffset < 0 {
		cursorOffset = 0
	}
	if cursorOffset > len(runes) {
		cursorOffset = len(runes)
	}

	before := string(runes[:cursorOffset])
	beforeWidth := runewidth.StringWidth(before)
	if beforeWidth >= width {
		before = ""
		beforeWidth = 0
	}

	cursorText := " "
	afterStart := cursorOffset
	if cursorOffset < len(runes) {
		cursorText = string(runes[cursorOffset])
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
	if afterStart < len(runes) {
		after = truncateDisplayWidth(string(runes[afterStart:]), afterWidth)
	}
	return before, cursorText, after
}

func renderEmptyEditorLine(placeholder string, width int, placeholderStyle, cursorStyle lipgloss.Style, cursorVisible bool) string {
	if width < 1 {
		width = 1
	}
	lineStyle := placeholderStyle.Copy().UnsetForeground().Width(width)

	cursorStyleToUse := placeholderStyle.Copy().UnsetForeground()
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

// pasteTokenLabel keeps placeholders compact and payload-free. Multi-line
// content reports lines; a single long line reports Unicode characters.
func pasteTokenLabel(text string, lineCount int) string {
	if lineCount > 1 {
		return fmt.Sprintf("%s lines", fmtK(lineCount))
	}
	return fmt.Sprintf("%s chars", fmtK(utf8.RuneCountInString(text)))
}

// normalizePasteNewlines turns CRLF and bare CR into LF so a pasted document
// keeps its line structure end to end (count, label, stored payload, prompt).
func normalizePasteNewlines(text string) string {
	if !strings.ContainsRune(text, '\r') {
		return text
	}
	return pasteLineBreakRe.ReplaceAllString(text, "\n")
}
