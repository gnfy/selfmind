package cli

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"selfmind/internal/kernel/llm"
)

func TestAssistantPhaseControlsFinalGutter(t *testing.T) {
	final := strings.TrimSpace(ansi.Strip(renderAssistantMessagePhase("## Result\n\nDone.", 40, llm.AssistantPhaseFinalAnswer)))
	if final != "• Result\n\n  Done." {
		t.Fatalf("final render = %q", final)
	}

	commentary := strings.TrimSpace(ansi.Strip(renderAssistantMessagePhase("I will inspect it.", 40, llm.AssistantPhaseCommentary)))
	if commentary != "I will inspect it." {
		t.Fatalf("commentary render = %q", commentary)
	}
	if strings.HasPrefix(commentary, "• ") {
		t.Fatalf("commentary unexpectedly used final gutter: %q", commentary)
	}
}

func TestFinalizeLiveStreamStoresAssistantPhase(t *testing.T) {
	model := NewController(nil, nil, nil, "").model
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
	model := NewController(nil, nil, nil, "").model
	model.updateInner(MsgStream{Content: "I will inspect it.", Phase: llm.AssistantPhaseCommentary})
	if !model.streamController.Pending() {
		t.Fatal("expected unterminated commentary to remain pending")
	}

	model.updateInner(MsgStream{Content: "Done.", Phase: llm.AssistantPhaseFinalAnswer})
	if len(model.messages) == 0 {
		t.Fatal("expected phase transition to finalize commentary")
	}
	commentary := model.messages[len(model.messages)-1]
	if commentary.AssistantPhase != llm.AssistantPhaseCommentary || commentary.Content != "I will inspect it." {
		t.Fatalf("commentary = %+v", commentary)
	}
	if model.liveStreamPhase != llm.AssistantPhaseFinalAnswer {
		t.Fatalf("live phase = %q, want final", model.liveStreamPhase)
	}
}
