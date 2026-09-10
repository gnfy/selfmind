package kernel

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

func artifactBackedResult(id string, size int) llm.Message {
	note := fmt.Sprintf("\n\n... [%s%s] ...\n\n", toolArtifactNoteToken, id)
	body := strings.Repeat("x", size)
	return llm.Message{Role: "tool", Content: body + note}
}

// TestToolResultTurnBudgetAgesOldestFirst pins the cumulative cap. The
// per-result cap and the age rule are both per-result, so a window of several
// large results can dominate a request with none of them individually old
// enough to shrink.
func TestToolResultTurnBudgetAgesOldestFirst(t *testing.T) {
	messages := []llm.Message{
		{Role: "user", Content: "do the work"},
		artifactBackedResult("art_first", 20000),
		artifactBackedResult("art_second", 20000),
		artifactBackedResult("art_third", 20000),
	}
	before := liveToolResultBytes(messages)
	if before <= toolResultTurnBudgetBytes {
		t.Fatalf("fixture must exceed the budget: %d", before)
	}

	shrunk := enforceToolResultTurnBudget(context.Background(), messages)
	if shrunk == 0 {
		t.Fatal("nothing was aged")
	}
	if after := liveToolResultBytes(messages); after > toolResultTurnBudgetBytes {
		t.Fatalf("still over budget: %d > %d", after, toolResultTurnBudgetBytes)
	}
	// Oldest first: the newest result keeps its body while an older one gave
	// way, so the model loses proximity to old evidence, not to fresh evidence.
	if len(messages[3].Content) <= toolResultAgedBytes {
		t.Fatalf("the newest result was aged before older ones: %d bytes", len(messages[3].Content))
	}
	if len(messages[1].Content) > toolResultAgedBytes {
		t.Fatalf("the oldest result was not aged: %d bytes", len(messages[1].Content))
	}
	// Every aged result still names its artifact, so the full output stays
	// addressable. Shrinking would be lossy otherwise.
	if !strings.Contains(messages[1].Content, "art_first") {
		t.Fatalf("aged result lost its artifact reference: %q", messages[1].Content)
	}
	if messages[0].Role != "user" || messages[0].Content != "do the work" {
		t.Fatal("a non-tool message was modified")
	}
}

// TestToolResultTurnBudgetNeverDropsUnspooledBytes: a result with no artifact
// reference exists nowhere else, so it is never shrunk even when that leaves
// the turn over budget. Losing evidence to save context is the wrong trade.
func TestToolResultTurnBudgetNeverDropsUnspooledBytes(t *testing.T) {
	messages := []llm.Message{
		{Role: "tool", Content: strings.Repeat("y", 40000)},
	}
	if shrunk := enforceToolResultTurnBudget(context.Background(), messages); shrunk != 0 {
		t.Fatalf("an unspooled result was shrunk: %d", shrunk)
	}
	if len(messages[0].Content) != 40000 {
		t.Fatalf("unspooled content changed: %d bytes", len(messages[0].Content))
	}
}

// TestToolResultTurnBudgetLeavesUnderBudgetTurnsAlone keeps the hot path free:
// an ordinary turn must not pay for a pass that has nothing to do.
func TestToolResultTurnBudgetLeavesUnderBudgetTurnsAlone(t *testing.T) {
	messages := []llm.Message{artifactBackedResult("art_small", 8000)}
	original := messages[0].Content
	if shrunk := enforceToolResultTurnBudget(context.Background(), messages); shrunk != 0 {
		t.Fatalf("an under-budget turn was aged: %d", shrunk)
	}
	if messages[0].Content != original {
		t.Fatal("an under-budget result was modified")
	}
}

type capturedResultSink struct{ contents map[string]string }

func (s *capturedResultSink) SaveToolOutput(_ context.Context, _ string, content string) (ToolArtifactRef, error) {
	id := fmt.Sprintf("art_capture%d", len(s.contents))
	s.contents[id] = content
	return ToolArtifactRef{ID: id, Bytes: len(content)}, nil
}

func TestCumulativeSpoolingKeepsReferencesAcrossFurtherShrinking(t *testing.T) {
	sink := &capturedResultSink{contents: map[string]string{}}
	ctx := WithToolArtifactSink(context.Background(), sink)
	var messages []llm.Message
	for i := 0; i < 40; i++ {
		messages = append(messages, llm.Message{Role: "tool", Name: "read_file", ToolCallID: fmt.Sprint(i), Content: fmt.Sprint(i) + strings.Repeat("x", 6000)})
		enforceToolResultTurnBudget(ctx, messages)
		if got := liveToolResultBytes(messages); got > toolResultTurnBudgetBytes {
			t.Fatalf("after %d results: %d bytes", i+1, got)
		}
	}
	if len(sink.contents) > len(messages) {
		t.Fatalf("already saved output was saved again: %d", len(sink.contents))
	}
	for i, msg := range messages {
		original := fmt.Sprint(i) + strings.Repeat("x", 6000)
		if msg.Content == original {
			continue
		}
		match := toolArtifactIDPattern.FindStringSubmatch(msg.Content)
		if len(match) != 2 || !strings.Contains(msg.Content, "tool_output_view") || sink.contents[match[1]] != original {
			t.Fatalf("lost exact read-back for output %d", i)
		}
	}
}
