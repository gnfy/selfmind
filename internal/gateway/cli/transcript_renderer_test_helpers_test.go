package cli

import (
	"selfmind/internal/kernel/llm"
	uitheme "selfmind/internal/ui/theme"

	"github.com/charmbracelet/lipgloss"
)

// These helpers keep renderer unit tests concise while production uses only
// the explicitly themed entry points checked by the reachability test.
func testTranscriptStyles() transcriptStyles { return newTranscriptStyles(uitheme.Default()) }

func renderAssistantMessage(content string, width int) string {
	return renderAssistantMessagePhase(content, width, llm.AssistantPhaseFinalAnswer)
}

func renderAssistantMessagePhase(content string, width int, phase llm.AssistantPhase) string {
	return renderAssistantMessagePhaseWithStyles(content, width, phase, testTranscriptStyles())
}

func renderToolMessage(msg ChatMessage, width int) string {
	return renderToolMessageWithStyles(msg, width, testTranscriptStyles())
}

func renderPatchCell(patch string, duration float64, width, maxLines int) string {
	return renderPatchCellWithStyles(patch, duration, width, maxLines, testTranscriptStyles())
}

func renderPlanCell(content string, duration float64, width int) string {
	return renderPlanCellWithStyles(content, duration, width, testTranscriptStyles())
}

func renderNoticeMessage(content string, kind noticeKind, width int) string {
	return renderNoticeMessageWithTheme(content, kind, width, uitheme.Default())
}

func toolSemanticActionStyle(label string) lipgloss.Style {
	return toolSemanticActionStyleWithStyles(label, testTranscriptStyles())
}
