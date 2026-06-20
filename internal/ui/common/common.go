package common

import (
	"image/color"

	"github.com/charmbracelet/lipgloss"
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
	bg := lipgloss.Color("235")
	surface := lipgloss.Color("236")
	border := lipgloss.Color("238")
	borderBright := lipgloss.Color("240")
	fg := lipgloss.Color("255")
	fgMuted := lipgloss.Color("245")
	fgSubtle := lipgloss.Color("238")
	primary := lipgloss.Color("86")
	secondary := lipgloss.Color("147")
	accent := lipgloss.Color("212")
	warning := lipgloss.Color("214")
	info := lipgloss.Color("75")

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

	// Codex-cli style: transcript stays plain, composer is a relaxed filled band.
	s.Editor.Panel = lipgloss.NewStyle().Background(surface).Padding(1, 1)
	s.Editor.Prompt = lipgloss.NewStyle().Foreground(fg).Background(surface).Bold(false)
	s.Editor.Text = lipgloss.NewStyle().Foreground(fg).Background(surface)
	s.Editor.Cursor = lipgloss.NewStyle().Background(lipgloss.Color("255")).Foreground(lipgloss.Color("0"))

	s.Status.Panel = lipgloss.NewStyle().Foreground(fgMuted).Padding(0, 1)
	s.Status.Label = lipgloss.NewStyle().Foreground(fgMuted)
	s.Status.Value = lipgloss.NewStyle().Foreground(fg)
	s.Status.Good = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	s.Status.Warning = lipgloss.NewStyle().Foreground(warning)
	s.Status.Error = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))

	s.Main = lipgloss.NewStyle().Padding(0, 0)
	s.Welcome = "SelfMind is ready. Type a task to begin."

	return s
}
