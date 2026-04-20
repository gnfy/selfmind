package components

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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

// Editor wraps textarea + textinput with large-paste detection.
// When a multi-line paste exceeds the configured thresholds, the actual
// content is stored here and a placeholder token is shown in the textarea.
// Call Value() to get the display value (with placeholders).
// Call ExpandValue() to get the real content with placeholders replaced.
type Editor struct {
	common           *common.Common
	textarea         textarea.Model
	textinput        textinput.Model
	secure           bool
	commands         []string
	snippets         []PasteSnippet           // stored snippets for large pastes
	largePasteChars  int                      // threshold in characters (from config, 0=disabled)
	largePasteLines  int                      // threshold in lines (from config, 0=disabled)
}

// NewEditor creates a new Editor component.
// editorCfg controls the large-paste detection thresholds (pass nil for defaults).
func NewEditor(c *common.Common, editorCfg *config.EditorConfig) *Editor {
	t := textarea.New()
	t.SetHeight(1)
	t.ShowLineNumbers = false
	t.Placeholder = "  Type your message or / for commands  |  Shift+Enter for multi-line  |  Ctrl+C to clear"
	t.Prompt = "" // Handled manually in Draw

	// Reset base styles to ensure no background blocks
	t.FocusedStyle.Base = lipgloss.NewStyle()
	t.BlurredStyle.Base = lipgloss.NewStyle()
	t.FocusedStyle.Text = c.Styles.Editor.Text
	t.BlurredStyle.Text = c.Styles.Editor.Text
	t.FocusedStyle.Placeholder = c.Styles.Subtle
	t.BlurredStyle.Placeholder = c.Styles.Subtle
	t.FocusedStyle.Prompt = lipgloss.NewStyle() // Prompt handled manually
	t.BlurredStyle.Prompt = lipgloss.NewStyle()
	t.FocusedStyle.CursorLine = lipgloss.NewStyle() // Remove background on cursor line

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
		commands:        []string{"/help", "/status", "/new", "/clear", "/exit", "/model", "/models", "/config", "/tasks", "/sessions", "/retry", "/undo", "/title", "/stop"},
		largePasteChars: chars,
		largePasteLines: lines,
	}
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
	val := e.textarea.Value()
	if strings.HasPrefix(val, "/") {
		var matches []string
		for _, cmd := range e.commands {
			if strings.HasPrefix(cmd, val) {
				matches = append(matches, cmd)
			}
		}
		if len(matches) > 0 {
			return strings.Join(matches, " | ")
		}
	}
	return ""
}

// Draw renders the editor into the given layout rect.
func (e *Editor) Draw(rect layout.Rect) string {
	availableW := rect.W - 2
	if availableW < 10 {
		availableW = rect.W
	}

	if e.secure {
		prompt := e.common.Styles.Editor.Prompt.Render(" secret > ")
		e.textinput.Width = availableW - 10
		return e.common.Styles.Editor.Panel.
			Width(rect.W).
			Render(lipgloss.JoinHorizontal(lipgloss.Top, prompt, e.textinput.View()))
	}

	prompt := e.common.Styles.Editor.Prompt.Render("> ")
	e.textarea.SetWidth(availableW - 2)
	suggestion := e.GetSuggestion()
	view := e.textarea.View()
	if suggestion != "" {
		view += "\n" + e.common.Styles.Subtle.PaddingLeft(2).Render(suggestion)
	}
	return e.common.Styles.Editor.Panel.
		Width(rect.W).
		Render(lipgloss.JoinHorizontal(lipgloss.Top, prompt, view))
}

// ─── Helpers ───────────────────────────────────────────────────────────────

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
