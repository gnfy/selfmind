package cli

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"selfmind/internal/kernel/llm"
	uitheme "selfmind/internal/ui/theme"
)

func TestAssistantPhaseControlsFinalGutter(t *testing.T) {
	final := strings.TrimSpace(ansi.Strip(renderAssistantMessagePhase("## Result\n\nDone.", 40, llm.AssistantPhaseFinalAnswer)))
	if final != "• Result\n\n  Done." {
		t.Fatalf("final render = %q", final)
	}

	commentary := strings.TrimSpace(ansi.Strip(renderAssistantMessagePhase("I will inspect it.", 40, llm.AssistantPhaseCommentary)))
	if commentary != "› I will inspect it." {
		t.Fatalf("commentary render = %q", commentary)
	}
	if strings.HasPrefix(commentary, "• ") {
		t.Fatalf("commentary unexpectedly used final gutter: %q", commentary)
	}
}

func TestCommentaryNarrationIsReadableAcrossScripts(t *testing.T) {
	tests := []string{
		"I will inspect the trigger mapping.",
		"接下来我检查触发器映射。",
		"Inspect API 配置与 deployment values。",
	}
	for _, content := range tests {
		t.Run(content, func(t *testing.T) {
			rendered := renderAssistantMessagePhase(content, 80, llm.AssistantPhaseCommentary)
			plain := strings.TrimSpace(ansi.Strip(rendered))
			if want := "› " + content; plain != want {
				t.Fatalf("commentary render = %q, want %q", plain, want)
			}
			if strings.Contains(rendered, "\x1b[2m") {
				t.Fatalf("action narration must not use ANSI faint: %q", rendered)
			}
		})
	}
}

func TestReadToolTargetIsVisuallySubordinateToActionNarration(t *testing.T) {
	profile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(profile) })

	narration := renderAssistantMessagePhase("Now verify the committed configuration.", 80, llm.AssistantPhaseCommentary)
	tool := renderToolMessage(ChatMessage{
		Role: "tool", ToolName: "read_file", ToolArgs: `{"path":"config.yml"}`, IsRunning: true,
	}, 80)

	if !strings.Contains(ansi.Strip(tool), "config.yml") {
		t.Fatalf("read tool fixture did not reach the semantic header: %q", ansi.Strip(tool))
	}
	mutedTarget := lipgloss.NewStyle().Foreground(uitheme.Default().Color(uitheme.TextSecondary)).Render("config.yml")
	if !strings.Contains(tool, mutedTarget) {
		t.Fatalf("tool evidence target must use a distinct muted color: %q", tool)
	}
	if strings.Contains(narration, mutedTarget) {
		t.Fatalf("action narration must retain normal body contrast: %q", narration)
	}
	if strings.Contains(mutedTarget, "\x1b[2m") {
		t.Fatalf("tool evidence may use a muted color but not ANSI faint: %q", mutedTarget)
	}
}

func TestFinalizeLiveStreamStoresAssistantPhase(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.commitLiveStream("I will inspect it.")
	if !model.finalizeLiveStream("", llm.AssistantPhaseCommentary) {
		t.Fatal("expected live stream to finalize")
	}
	if len(model.messages) == 0 {
		t.Fatal("expected finalized assistant message")
	}
	got := model.messages[len(model.messages)-1].AssistantPhase
	if got != llm.AssistantPhaseCommentary {
		t.Fatalf("assistant phase = %q, want commentary", got)
	}
}

func TestStreamPhaseChangeFinalizesPendingCommentary(t *testing.T) {
	model := NewController("", "", nil, "").model
	model.updateInner(MsgStream{Content: "I will inspect it.", Phase: llm.AssistantPhaseCommentary})
	if preview := strings.TrimSpace(ansi.Strip(model.renderActiveBlock(80))); preview != "› I will inspect it." {
		t.Fatalf("commentary preview = %q", preview)
	}

	model.updateInner(MsgStream{Content: "Done.", Phase: llm.AssistantPhaseFinalAnswer})
	if len(model.messages) == 0 {
		t.Fatal("expected phase transition to finalize commentary")
	}
	commentary := model.messages[len(model.messages)-1]
	if commentary.AssistantPhase != llm.AssistantPhaseCommentary || commentary.Content != "I will inspect it." {
		t.Fatalf("commentary = %+v", commentary)
	}
	if preview := strings.TrimSpace(ansi.Strip(model.renderActiveBlock(80))); preview != "• Done." {
		t.Fatalf("final preview = %q", preview)
	}
}
