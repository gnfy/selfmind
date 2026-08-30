package theme

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestResolveUsesTerminalDefaultForMainText(t *testing.T) {
	for _, mode := range []Mode{ModeAuto, ModeDark, ModeLight, ModeMono} {
		theme, err := Resolve(Options{Mode: mode, Profile: termenv.TrueColor, DarkBackground: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := theme.Color(TextPrimary).(lipgloss.NoColor); !ok {
			t.Fatalf("mode %q primary color = %T, want terminal default", mode, theme.Color(TextPrimary))
		}
	}
}

func TestResolveHonorsExplicitContrastAndAutoDetection(t *testing.T) {
	tests := []struct {
		mode     Mode
		detected bool
		wantDark bool
	}{
		{ModeAuto, true, true},
		{ModeAuto, false, false},
		{ModeDark, false, true},
		{ModeLight, true, false},
	}
	for _, tt := range tests {
		theme, err := Resolve(Options{Mode: tt.mode, Profile: termenv.TrueColor, DarkBackground: tt.detected})
		if err != nil {
			t.Fatal(err)
		}
		if theme.IsDark() != tt.wantDark {
			t.Fatalf("mode %q detected %v: dark = %v, want %v", tt.mode, tt.detected, theme.IsDark(), tt.wantDark)
		}
	}
}

func TestResolveNoColorIsHardFloor(t *testing.T) {
	for _, mode := range []Mode{ModeAuto, ModeDark, ModeLight, ModeMono} {
		profile := termenv.TrueColor
		if mode != ModeMono {
			profile = termenv.Ascii
		}
		theme, err := Resolve(Options{Mode: mode, Profile: profile, DarkBackground: true})
		if err != nil {
			t.Fatal(err)
		}
		for role := TextPrimary; role <= SelectionText; role++ {
			if _, ok := theme.Color(role).(lipgloss.NoColor); !ok {
				t.Fatalf("mode %q role %d = %T, want no color", mode, role, theme.Color(role))
			}
		}
	}
}

func TestResolveAdaptsSemanticAccentToCapability(t *testing.T) {
	ansi, err := Resolve(Options{Mode: ModeDark, Profile: termenv.ANSI})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ansi.Color(Accent), lipgloss.TerminalColor(lipgloss.Color("6")); got != want {
		t.Fatalf("ANSI accent = %v, want %v", got, want)
	}

	dark, _ := Resolve(Options{Mode: ModeDark, Profile: termenv.TrueColor})
	light, _ := Resolve(Options{Mode: ModeLight, Profile: termenv.TrueColor})
	if dark.Color(Accent) == light.Color(Accent) {
		t.Fatalf("dark and light accents must differ: %v", dark.Color(Accent))
	}
}

func TestParseModeRejectsUnsupportedValue(t *testing.T) {
	if _, err := ParseMode("solarized"); err == nil {
		t.Fatal("ParseMode(solarized) succeeded, want error")
	}
	if got, err := ParseMode(" LIGHT "); err != nil || got != ModeLight {
		t.Fatalf("ParseMode = %q, %v; want light", got, err)
	}
}
