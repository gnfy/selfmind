package components

import (
	"strings"
	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"selfmind/internal/ui/common"
	"selfmind/internal/ui/layout"
)

type Editor struct {
	common    *common.Common
	textarea  textarea.Model
	textinput textinput.Model
	secure    bool
	commands  []string
}

func NewEditor(c *common.Common) *Editor {
	t := textarea.New()
	t.SetHeight(1)
	t.ShowLineNumbers = false
	t.Placeholder = "        Type your message or / to see commands, Ctrl+C to cancel"
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
	t.Focus()

	ti := textinput.New()
	ti.Placeholder = "   Enter secret value..."
	ti.Prompt = "" // Handled manually in Draw
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	
	ti.TextStyle = c.Styles.Editor.Text
	ti.PlaceholderStyle = c.Styles.Subtle
	
	ti.Cursor.Style = c.Styles.Editor.Cursor
	ti.Cursor.SetMode(cursor.CursorStatic)
	ti.Cursor.Focus()

	return &Editor{
		common:    c,
		textarea:  t,
		textinput: ti,
		commands:  []string{"/help", "/status", "/new", "/clear", "/exit", "/model", "/models", "/config", "/tasks", "/sessions", "/retry", "/undo", "/title", "/stop"},
	}
}

func (e *Editor) Update(msg tea.Msg) tea.Cmd {
	if e.secure {
		var cmd tea.Cmd
		e.textinput, cmd = e.textinput.Update(msg)
		return cmd
	}
	var cmd tea.Cmd
	e.textarea, cmd = e.textarea.Update(msg)
	return cmd
}

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

func (e *Editor) IsSecure() bool {
	return e.secure
}

func (e *Editor) Value() string {
	if e.secure {
		return e.textinput.Value()
	}
	return e.textarea.Value()
}

func (e *Editor) Reset() {
	e.textarea.Reset()
	e.textinput.Reset()
}

func (e *Editor) SetValue(s string) {
	if e.secure {
		e.textinput.SetValue(s)
	} else {
		e.textarea.SetValue(s)
	}
}

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
