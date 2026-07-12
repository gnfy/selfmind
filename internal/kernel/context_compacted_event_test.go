package kernel

import (
	"context"
	"testing"
)

// TestCompactionEmitsContextCompactedEvent: when compaction actually fires
// with an event channel installed, exactly one context.compacted event
// reports what it bought (W2 observability; the compaction result itself is
// covered by the sibling compaction tests).
func TestCompactionEmitsContextCompactedEvent(t *testing.T) {
	provider := &fakeSummarizer{reply: "## Active Task\nbuild the app"}
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(provider)

	ch := make(chan string, 16)
	ctx := WithEventChannel(context.Background(), ch)

	got := engine.TruncateMessagesCtx(ctx, compactionFixture())
	if provider.calls.Load() != 1 {
		t.Fatalf("expected one compaction call, got %d", provider.calls.Load())
	}
	close(ch)

	var payload map[string]interface{}
	events := 0
	for raw := range ch {
		event, ok := DecodeAgentEvent(raw)
		if !ok || event.Type != "context.compacted" {
			continue
		}
		events++
		payload = event.Payload
	}
	if events != 1 {
		t.Fatalf("expected exactly one context.compacted event, got %d", events)
	}
	before, _ := payload["before_tokens"].(float64)
	after, _ := payload["after_tokens"].(float64)
	if before <= 0 || after <= 0 || after >= before {
		t.Fatalf("compaction payload must show a real reduction: before=%v after=%v", before, after)
	}
	if replaced, _ := payload["messages_replaced"].(float64); replaced <= 0 {
		t.Fatalf("messages_replaced must be positive: %v", payload["messages_replaced"])
	}
	_ = got

	// No channel installed → same compaction, no event machinery involved.
	provider2 := &fakeSummarizer{reply: "## Active Task\nbuild the app"}
	engine2 := NewContextEngine(200, 10)
	engine2.SetSummaryProvider(provider2)
	engine2.TruncateMessages(compactionFixture())
	if provider2.calls.Load() != 1 {
		t.Fatalf("channel-less compaction must be unaffected: %d calls", provider2.calls.Load())
	}
}
