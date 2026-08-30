package cli

import (
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	uitheme "selfmind/internal/ui/theme"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestCommentaryThemeKeepsMultilingualBodyAtTerminalDefault(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })

	resolved, err := uitheme.Resolve(uitheme.Options{Mode: uitheme.ModeLight, Profile: termenv.TrueColor})
	if err != nil {
		t.Fatal(err)
	}
	styles := newTranscriptStyles(resolved)
	body := "接下来检查 trigger mapping and deployment values。"
	rendered := renderAssistantMessagePhaseWithStyles(body, 100, llm.AssistantPhaseCommentary, styles)
	marker := lipgloss.NewStyle().Foreground(resolved.Color(uitheme.Accent)).Render(glyphChevron + " ")
	if !strings.Contains(rendered, marker+body) {
		t.Fatalf("commentary did not keep the body at terminal default contrast: %q", rendered)
	}
	if strings.Contains(rendered, "\x1b[2m") {
		t.Fatalf("commentary must not depend on ANSI faint: %q", rendered)
	}
}

func TestMonoTranscriptContainsNoForegroundOrBackgroundColor(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })

	resolved, err := uitheme.Resolve(uitheme.Options{Mode: uitheme.ModeMono, Profile: termenv.TrueColor})
	if err != nil {
		t.Fatal(err)
	}
	rendered := renderCellWithTheme(ChatMessage{
		Role: "assistant", Content: "**Result:** done", AssistantPhase: llm.AssistantPhaseFinalAnswer,
	}, 80, resolved)
	if strings.Contains(rendered, "\x1b[38;") || strings.Contains(rendered, "\x1b[48;") {
		t.Fatalf("mono transcript emitted a color escape: %q", rendered)
	}
}

func TestUserRequestUsesOpenBoundariesWithoutBackground(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })

	rendered := renderCellWithTheme(ChatMessage{Role: "user", Content: "检查中文 main path"}, 32, uitheme.Default())
	if strings.Contains(rendered, "\x1b[48;") {
		t.Fatalf("user request painted a background: %q", rendered)
	}
	plain := strings.TrimPrefix(stripANSI(rendered), "\n")
	lines := strings.Split(plain, "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "YOUR REQUEST") {
		t.Fatalf("user request layout = %q", plain)
	}
	if lines[2] != strings.Repeat("─", 32) {
		t.Fatalf("bottom boundary width = %q", lines[2])
	}
	if strings.HasPrefix(lines[1], "│") || strings.HasSuffix(lines[1], "│") {
		t.Fatalf("user request body has side rails: %q", lines[1])
	}
}
