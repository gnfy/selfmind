package router

import (
	"context"
	"errors"
	"testing"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/runpool"
)

func TestWatchdogErrorPreservesStalledCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(runpool.ErrStalled)

	err := watchdogError(ctx)
	if !errors.Is(err, runpool.ErrStalled) {
		t.Fatalf("watchdog error %v does not wrap ErrStalled", err)
	}
}

func TestAgentEventToStreamPreservesAssistantPhase(t *testing.T) {
	raw := kernel.EncodeAgentEvent(kernel.AgentEvent{
		Type:    "stream",
		Content: "Working",
		Phase:   llm.AssistantPhaseCommentary,
	})
	event := agentEventToStream(raw)
	if event.Content != "Working" || event.Phase != llm.AssistantPhaseCommentary {
		t.Fatalf("stream event = %+v", event)
	}
}
