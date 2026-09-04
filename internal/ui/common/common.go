package common

import (
	"selfmind/internal/ui/theme"

	"github.com/charmbracelet/lipgloss"
)

const (
	PaletteBackground = "#2d001b"
	PaletteSurface    = "#4b2a40"
	PaletteBorder     = "#8a7180"
	PaletteBorderDim  = "#6f5867"
	PaletteText       = "#f4edf2"
	PaletteMuted      = "#a58c9d"
	PaletteSubtle     = "#7e6676"
	PaletteBlue       = "#2f9de8"
	PaletteAmber      = "#b9824f"
	PaletteGreen      = "#7dd36f"
	PaletteRed        = "#ff6b6b"
	PaletteCursor     = "#b8d8bd"
	PaletteEditorBG   = "236"
	PaletteEditorText = "255"
	PaletteEditorHint = "244"
)

type Common struct {
	Width, Height int
	Styles        *Styles
	Theme         theme.Theme
}

type Styles struct {
	Primary, Secondary, Accent, Background, Surface, Border, Error, Warning, Info, FgBase, FgMuted, FgSubtle lipgloss.TerminalColor
	Base, Muted, HalfMuted, Subtle, TagBase, TagError, TagInfo, TagSuccess, TagWarning                       lipgloss.Style
	Header                                                                                                   struct{ Title, Subtitle, Separator lipgloss.Style }
	Sidebar                                                                                                  struct{ Panel, Title, Item, ItemFocus, Muted lipgloss.Style }
	Chat                                                                                                     struct{ UserBubble, UserText, AssistantBubble, AssistantText, ToolBubble, ToolName, ToolResult, Thinking, ThinkingPrefix, ProgressGlyph, ProgressLabel, Timestamp, Separator, Selected lipgloss.Style }
	Editor                                                                                                   struct{ Panel, Prompt, Text, Placeholder, Cursor, LineNumber, CompletionName, CompletionDescription, CompletionSelectedName, CompletionSelectedDescription lipgloss.Style }
	Status                                                                                                   struct{ Panel, Label, Value, Good, Warning, Error lipgloss.Style }
	Welcome                                                                                                  string
	Panel, Main                                                                                              lipgloss.Style
	Scrollbar                                                                                                struct{ Thumb, Track lipgloss.Style }
}

func DefaultStyles() *Styles {
	return StylesFor(theme.Default())
}

// New constructs shared UI primitives for one resolved terminal theme.
func New(t theme.Theme) *Common {
	return &Common{Theme: t, Styles: StylesFor(t)}
}

// StylesFor is the compatibility bridge for components that already consume
// Common styles. New colors originate only from semantic theme roles here.
func StylesFor(t theme.Theme) *Styles {
	bg := t.Color(theme.TextPrimary)
	surface := t.Color(theme.ComposerBackground)
	border := t.Color(theme.BorderMuted)
	borderBright := t.Color(theme.Border)
	fg := t.Color(theme.TextPrimary)
	fgMuted := t.Color(theme.TextSecondary)
	fgSubtle := t.Color(theme.TextDecorative)
	primary := t.Color(theme.Accent)
	secondary := t.Color(theme.Brand)
	accent := t.Color(theme.Error)
	warning := t.Color(theme.Warning)
	info := t.Color(theme.Accent)

	s := &Styles{
		Primary: primary, Secondary: secondary, Accent: accent, Background: bg, Surface: surface,
		Border: border, Error: accent, Warning: warning, Info: info, FgBase: fg, FgMuted: fgMuted, FgSubtle: fgSubtle,
	}

	s.Base = lipgloss.NewStyle().Foreground(fg)
	s.Muted = lipgloss.NewStyle().Foreground(fgMuted)
	s.Subtle = lipgloss.NewStyle().Foreground(fgSubtle)

	s.Header.Title = lipgloss.NewStyle().Foreground(fg).Bold(true).Padding(0, 1)
	s.Header.Subtitle = lipgloss.NewStyle().Foreground(fgMuted).Padding(0, 1)
	s.Header.Separator = lipgloss.NewStyle().Foreground(borderBright)

	s.Chat.UserBubble = lipgloss.NewStyle().Foreground(fgMuted).MarginTop(1)
	s.Chat.UserText = lipgloss.NewStyle().Foreground(fg)
	s.Chat.AssistantBubble = lipgloss.NewStyle().Foreground(fgMuted).MarginTop(1)
	s.Chat.AssistantText = lipgloss.NewStyle().Foreground(fg)
	s.Chat.ToolBubble = lipgloss.NewStyle().Foreground(fgMuted).MarginTop(1)
	s.Chat.ToolName = lipgloss.NewStyle().Foreground(primary).Bold(true)
	s.Chat.ToolResult = lipgloss.NewStyle().Foreground(fgMuted)
	s.Chat.Thinking = lipgloss.NewStyle().Foreground(fgMuted).Italic(true)
	// The live progress row is status, not evidence. It used to share
	// TextSecondary with tool results and plan explanations, so it read as one
	// more piece of evidence instead of "this is happening now". Only the
	// moving glyph takes Accent — the plan's active step already owns bold
	// Accent text, and two accent blocks would compete — while the label uses
	// the mainline foreground.
	s.Chat.ProgressGlyph = lipgloss.NewStyle().Foreground(primary)
	s.Chat.ProgressLabel = lipgloss.NewStyle().Foreground(fg)
	s.Chat.Selected = lipgloss.NewStyle().Foreground(t.Color(theme.SelectionText)).Background(t.Color(theme.SelectionBackground))

	editorText := t.Color(theme.ComposerText)
	// The composer uses open top/bottom boundaries and inherits the terminal
	// background. This keeps long drafts visually distinct without painting a
	// conflicting full-width slab in translucent or custom terminal themes.
	s.Editor.Panel = lipgloss.NewStyle()
	s.Editor.Prompt = lipgloss.NewStyle().Foreground(t.Color(theme.Accent)).Bold(false)
	s.Editor.Text = lipgloss.NewStyle().Foreground(editorText)
	s.Editor.Placeholder = lipgloss.NewStyle().Foreground(t.Color(theme.ComposerPlaceholder))
	s.Editor.Cursor = lipgloss.NewStyle().Background(t.Color(theme.ComposerText)).Foreground(t.Color(theme.SelectionText))
	s.Editor.CompletionName = lipgloss.NewStyle().Foreground(fgMuted).Bold(true).Width(14)
	s.Editor.CompletionDescription = lipgloss.NewStyle().Foreground(fgSubtle)
	s.Editor.CompletionSelectedName = lipgloss.NewStyle().Foreground(primary).Bold(true).Width(14)
	s.Editor.CompletionSelectedDescription = lipgloss.NewStyle().Foreground(primary)

	s.Status.Panel = lipgloss.NewStyle().Foreground(fgMuted).Padding(0, 1)
	s.Status.Label = lipgloss.NewStyle().Foreground(fgMuted)
	s.Status.Value = lipgloss.NewStyle().Foreground(fg)
	s.Status.Good = lipgloss.NewStyle().Foreground(t.Color(theme.Success))
	s.Status.Warning = lipgloss.NewStyle().Foreground(warning)
	s.Status.Error = lipgloss.NewStyle().Foreground(t.Color(theme.Error))

	s.Main = lipgloss.NewStyle().Padding(0, 0)
	s.Welcome = "SelfMind is ready. Type a task to begin."

	return s
}
