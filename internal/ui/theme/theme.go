// Package theme resolves terminal capabilities and a small user preference
// into semantic colors. Renderers consume roles rather than owning RGB or ANSI
// values, so contrast and no-color behavior stay consistent across the TUI.
package theme

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Mode is the only user-facing theme choice. Dark and light select a contrast
// palette; neither mode paints the terminal background.
type Mode string

const (
	ModeAuto  Mode = "auto"
	ModeDark  Mode = "dark"
	ModeLight Mode = "light"
	ModeMono  Mode = "mono"
)

// Role names meaning, not a particular color. Keep this vocabulary small: a
// new renderer should normally reuse one of these roles instead of introducing
// a component-specific color.
type Role uint8

const (
	TextPrimary Role = iota
	TextSecondary
	TextDecorative
	Accent
	Brand
	Success
	Warning
	Error
	Border
	BorderMuted
	ComposerBackground
	ComposerText
	ComposerPlaceholder
	SelectionBackground
	SelectionText
)

// Options are resolved once by the TUI composition root. DarkBackground is
// consulted only for auto mode; explicit dark/light choices never query the
// terminal background.
type Options struct {
	Mode           Mode
	Profile        termenv.Profile
	DarkBackground bool
}

// Theme is an immutable resolved semantic palette.
type Theme struct {
	mode   Mode
	dark   bool
	colors [SelectionText + 1]lipgloss.TerminalColor
}

// ParseMode validates a configured mode and normalizes whitespace/case.
func ParseMode(value string) (Mode, error) {
	mode := Mode(strings.ToLower(strings.TrimSpace(value)))
	if mode == "" {
		mode = ModeAuto
	}
	switch mode {
	case ModeAuto, ModeDark, ModeLight, ModeMono:
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported TUI theme %q (want auto, dark, light, or mono)", value)
	}
}

// Resolve builds the semantic palette for one terminal session.
func Resolve(opts Options) (Theme, error) {
	mode, err := ParseMode(string(opts.Mode))
	if err != nil {
		return Theme{}, err
	}
	dark := opts.DarkBackground
	if mode == ModeDark {
		dark = true
	} else if mode == ModeLight {
		dark = false
	}

	t := Theme{mode: mode, dark: dark}
	if mode == ModeMono || opts.Profile == termenv.Ascii {
		for i := range t.colors {
			t.colors[i] = lipgloss.NoColor{}
		}
		return t, nil
	}

	if opts.Profile == termenv.ANSI {
		t.colors = ansiPalette(dark)
		return t, nil
	}
	if dark {
		t.colors = richDarkPalette()
	} else {
		t.colors = richLightPalette()
	}
	return t, nil
}

// Default returns a deterministic dark rich-color theme for compatibility
// constructors and tests that do not run through the TUI composition root.
func Default() Theme {
	t, _ := Resolve(Options{Mode: ModeDark, Profile: termenv.TrueColor, DarkBackground: true})
	return t
}

func (t Theme) Mode() Mode   { return t.mode }
func (t Theme) IsDark() bool { return t.dark }

// Color returns the terminal color for a semantic role. Unknown roles safely
// fall back to the terminal's default foreground.
func (t Theme) Color(role Role) lipgloss.TerminalColor {
	if role > SelectionText || t.colors[role] == nil {
		return lipgloss.NoColor{}
	}
	return t.colors[role]
}

func richDarkPalette() [SelectionText + 1]lipgloss.TerminalColor {
	return [SelectionText + 1]lipgloss.TerminalColor{
		TextPrimary:         lipgloss.NoColor{},
		TextSecondary:       lipgloss.Color("#b6aeb4"),
		TextDecorative:      lipgloss.Color("#a0979e"),
		Accent:              lipgloss.Color("#56d4dd"),
		Brand:               lipgloss.Color("#d2a8ff"),
		Success:             lipgloss.Color("#7ee787"),
		Warning:             lipgloss.Color("#e3b341"),
		Error:               lipgloss.Color("#ff7b72"),
		Border:              lipgloss.Color("#8b949e"),
		BorderMuted:         lipgloss.Color("#6e7681"),
		ComposerBackground:  lipgloss.Color("236"),
		ComposerText:        lipgloss.Color("255"),
		ComposerPlaceholder: lipgloss.Color("250"),
		SelectionBackground: lipgloss.Color("#56d4dd"),
		SelectionText:       lipgloss.Color("0"),
	}
}

func richLightPalette() [SelectionText + 1]lipgloss.TerminalColor {
	return [SelectionText + 1]lipgloss.TerminalColor{
		TextPrimary:         lipgloss.NoColor{},
		TextSecondary:       lipgloss.Color("#59636e"),
		TextDecorative:      lipgloss.Color("#636d77"),
		Accent:              lipgloss.Color("#006f78"),
		Brand:               lipgloss.Color("#8250df"),
		Success:             lipgloss.Color("#1a7f37"),
		Warning:             lipgloss.Color("#8a5d00"),
		Error:               lipgloss.Color("#cf222e"),
		Border:              lipgloss.Color("#59636e"),
		BorderMuted:         lipgloss.Color("#8c959f"),
		ComposerBackground:  lipgloss.Color("254"),
		ComposerText:        lipgloss.Color("0"),
		ComposerPlaceholder: lipgloss.Color("242"),
		SelectionBackground: lipgloss.Color("#006f78"),
		SelectionText:       lipgloss.Color("15"),
	}
}

func ansiPalette(_ bool) [SelectionText + 1]lipgloss.TerminalColor {
	p := [SelectionText + 1]lipgloss.TerminalColor{
		TextPrimary:         lipgloss.NoColor{},
		TextSecondary:       lipgloss.NoColor{},
		TextDecorative:      lipgloss.NoColor{},
		Accent:              lipgloss.Color("6"),
		Brand:               lipgloss.Color("5"),
		Success:             lipgloss.Color("2"),
		Warning:             lipgloss.Color("3"),
		Error:               lipgloss.Color("1"),
		Border:              lipgloss.NoColor{},
		BorderMuted:         lipgloss.NoColor{},
		ComposerBackground:  lipgloss.NoColor{},
		ComposerText:        lipgloss.NoColor{},
		ComposerPlaceholder: lipgloss.NoColor{},
		SelectionBackground: lipgloss.Color("6"),
		SelectionText:       lipgloss.Color("0"),
	}
	return p
}
