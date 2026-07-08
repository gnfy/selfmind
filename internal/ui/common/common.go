package common

import (
	"image/color"

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
}

type Styles struct {
	Primary, Secondary, Accent, Background, Surface, Border, Error, Warning, Info, FgBase, FgMuted, FgSubtle color.Color
	Base, Muted, HalfMuted, Subtle, TagBase, TagError, TagInfo, TagSuccess, TagWarning                       lipgloss.Style
	Header                                                                                                   struct{ Title, Subtitle, Separator lipgloss.Style }
	Sidebar                                                                                                  struct{ Panel, Title, Item, ItemFocus, Muted lipgloss.Style }
	Chat                                                                                                     struct{ UserBubble, UserText, AssistantBubble, AssistantText, ToolBubble, ToolName, ToolResult, Thinking, ThinkingPrefix, Timestamp, Separator, Selected lipgloss.Style }
	Editor                                                                                                   struct{ Panel, Prompt, Text, Cursor, LineNumber lipgloss.Style }
	Status                                                                                                   struct{ Panel, Label, Value, Good, Warning, Error lipgloss.Style }
	Welcome                                                                                                  string
	Panel, Main                                                                                              lipgloss.Style
	Scrollbar                                                                                                struct{ Thumb, Track lipgloss.Style }
}

func DefaultStyles() *Styles {
	bg := lipgloss.Color(PaletteBackground)
	surface := lipgloss.Color(PaletteSurface)
	border := lipgloss.Color(PaletteBorderDim)
	borderBright := lipgloss.Color(PaletteBorder)
	fg := lipgloss.Color(PaletteText)
	fgMuted := lipgloss.Color(PaletteMuted)
	fgSubtle := lipgloss.Color(PaletteSubtle)
	primary := lipgloss.Color(PaletteBlue)
	secondary := lipgloss.Color("#c7a8d8")
	accent := lipgloss.Color("#d778a5")
	warning := lipgloss.Color(PaletteAmber)
	info := lipgloss.Color(PaletteBlue)

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
	s.Chat.ToolBubble = lipgloss.NewStyle().Foreground(fgMuted).Background(bg).MarginTop(1)
	s.Chat.ToolName = lipgloss.NewStyle().Foreground(primary).Bold(true)
	s.Chat.ToolResult = lipgloss.NewStyle().Foreground(fgMuted).Background(bg)
	s.Chat.Thinking = lipgloss.NewStyle().Foreground(fgMuted).Italic(true)
	s.Chat.Selected = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("15"))

	editorBG := lipgloss.Color(PaletteEditorBG)
	editorText := lipgloss.Color(PaletteEditorText)
	// Keep the composer on the original neutral gray; the Codex-like palette is
	// reserved for chrome and hints so the input remains familiar and legible.
	s.Editor.Panel = lipgloss.NewStyle().Background(editorBG).Padding(1, 1)
	s.Editor.Prompt = lipgloss.NewStyle().Foreground(editorText).Background(editorBG).Bold(false)
	s.Editor.Text = lipgloss.NewStyle().Foreground(editorText).Background(editorBG)
	s.Editor.Cursor = lipgloss.NewStyle().Background(lipgloss.Color(PaletteEditorText)).Foreground(lipgloss.Color("0"))

	s.Status.Panel = lipgloss.NewStyle().Foreground(fgMuted).Padding(0, 1)
	s.Status.Label = lipgloss.NewStyle().Foreground(fgMuted)
	s.Status.Value = lipgloss.NewStyle().Foreground(fg)
	s.Status.Good = lipgloss.NewStyle().Foreground(lipgloss.Color(PaletteGreen))
	s.Status.Warning = lipgloss.NewStyle().Foreground(warning)
	s.Status.Error = lipgloss.NewStyle().Foreground(lipgloss.Color(PaletteRed))

	s.Main = lipgloss.NewStyle().Padding(0, 0)
	s.Welcome = "SelfMind is ready. Type a task to begin."

	return s
}
