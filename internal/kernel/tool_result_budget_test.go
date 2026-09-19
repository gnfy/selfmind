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
		artifactBackedResult("art_first", 160000),
		artifactBackedResult("art_second", 160000),
		artifactBackedResult("art_third", 160000),
	}
	before := liveToolResultBytes(messages)
	if before <= toolResultReclaimCeilingBytes {
		t.Fatalf("fixture must exceed the budget: %d", before)
	}

	shrunk := reclaimToolResultBytes(context.Background(), messages)
	if shrunk == 0 {
		t.Fatal("nothing was aged")
	}
	if after := liveToolResultBytes(messages); after > toolResultReclaimCeilingBytes {
		t.Fatalf("still over budget: %d > %d", after, toolResultReclaimCeilingBytes)
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
		{Role: "tool", Content: strings.Repeat("y", 400000)},
	}
	if shrunk := reclaimToolResultBytes(context.Background(), messages); shrunk != 0 {
		t.Fatalf("an unspooled result was shrunk: %d", shrunk)
	}
	if len(messages[0].Content) != 400000 {
		t.Fatalf("unspooled content changed: %d bytes", len(messages[0].Content))
	}
}

// TestToolResultReclaimLeavesOrdinaryTurnsAlone keeps the hot path free and
// the request prefix stable: an ordinary turn must not be rewritten at all.
func TestToolResultReclaimLeavesOrdinaryTurnsAlone(t *testing.T) {
	messages := []llm.Message{artifactBackedResult("art_small", 80000)}
	original := messages[0].Content
	if shrunk := reclaimToolResultBytes(context.Background(), messages); shrunk != 0 {
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
		messages = append(messages, llm.Message{Role: "tool", Name: "read_file", ToolCallID: fmt.Sprint(i), Content: fmt.Sprint(i) + strings.Repeat("x", 24000)})
		reclaimToolResultBytes(ctx, messages)
		if got := liveToolResultBytes(messages); got > toolResultReclaimCeilingBytes {
			t.Fatalf("after %d results: %d bytes", i+1, got)
		}
	}
	if len(sink.contents) > len(messages) {
		t.Fatalf("already saved output was saved again: %d", len(sink.contents))
	}
	for i, msg := range messages {
		original := fmt.Sprint(i) + strings.Repeat("x", 24000)
		if msg.Content == original {
			continue
		}
		match := toolArtifactIDPattern.FindStringSubmatch(msg.Content)
		if len(match) != 2 || !strings.Contains(msg.Content, "tool_output_view") || sink.contents[match[1]] != original {
			t.Fatalf("lost exact read-back for output %d", i)
		}
	}
}

// TestToolResultTurnBudgetLeavesHeadroomForTheNextTurns is the cache contract.
// Aging rewrites an earlier message, so the provider's prefix cache breaks from
// that message on. Trimming to exactly the budget made the next result cross it
// again and pay that cost again, turn after turn. One pass must trim past the
// budget so the turns that follow add results without aging at all.
func TestToolResultTurnBudgetLeavesHeadroomForTheNextTurns(t *testing.T) {
	messages := []llm.Message{
		{Role: "user", Content: "do the work"},
		artifactBackedResult("art_first", 160000),
		artifactBackedResult("art_second", 160000),
	}
	if shrunk := reclaimToolResultBytes(context.Background(), messages); shrunk == 0 {
		t.Fatal("nothing was aged")
	}
	after := liveToolResultBytes(messages)
	if after > toolResultReclaimTargetBytes {
		t.Fatalf("one pass must trim to the target, not merely under the budget: %d > %d", after, toolResultReclaimTargetBytes)
	}

	// A result that fits in the headroom must not start another pass: no message
	// may be rewritten, so the cached prefix survives.
	headroom := toolResultReclaimCeilingBytes - after
	if headroom < 2048 {
		t.Fatalf("a pass left only %d bytes of headroom", headroom)
	}
	messages = append(messages, artifactBackedResult("art_third", headroom-1024))
	before := append([]string(nil), contentsOf(messages)...)
	if shrunk := reclaimToolResultBytes(context.Background(), messages); shrunk != 0 {
		t.Fatalf("a result inside the headroom aged the window again: %d", shrunk)
	}
	for i, content := range contentsOf(messages) {
		if content != before[i] {
			t.Fatalf("message %d was rewritten without exceeding the budget", i)
		}
	}
}

// TestToolResultTurnBudgetStopsAtTheBudgetWhenTheTargetIsUnreachable: a window
// whose results are already at their floor cannot reach the target. It must
// still behave as before — never worse, and never rewriting what it cannot
// usefully shrink.
func TestToolResultTurnBudgetStopsAtTheBudgetWhenTheTargetIsUnreachable(t *testing.T) {
	var messages []llm.Message
	for i := 0; i < 12; i++ {
		messages = append(messages, artifactBackedResult(fmt.Sprintf("art_%d", i), 24000))
	}
	if liveToolResultBytes(messages) <= toolResultReclaimCeilingBytes {
		t.Fatal("fixture must exceed the budget")
	}
	reclaimToolResultBytes(context.Background(), messages)
	if after := liveToolResultBytes(messages); after > toolResultReclaimCeilingBytes {
		t.Fatalf("an unreachable target must not weaken the budget: %d > %d", after, toolResultReclaimCeilingBytes)
	}
	// A second pass with nothing left to shrink must rewrite nothing at all.
	before := append([]string(nil), contentsOf(messages)...)
	reclaimToolResultBytes(context.Background(), messages)
	for i, content := range contentsOf(messages) {
		if content != before[i] {
			t.Fatalf("message %d was rewritten by a pass that could not shrink it", i)
		}
	}
}

func contentsOf(messages []llm.Message) []string {
	out := make([]string, len(messages))
	for i, msg := range messages {
		out[i] = msg.Content
	}
	return out
}

// TestToolResultTurnBudgetAgesTheNewestOnlyAsALastResort: reaching for headroom
// must not consume the result the model is about to reason over. The newest
// result gives way only when the window exceeds the budget without it, which is
// the cumulative cap the target may not weaken.
func TestToolResultTurnBudgetAgesTheNewestOnlyAsALastResort(t *testing.T) {
	// Older results alone can carry the window under the budget: the newest
	// keeps its body even though the target is not reached.
	spared := []llm.Message{
		artifactBackedResult("art_old_a", 160000),
		artifactBackedResult("art_old_b", 160000),
		artifactBackedResult("art_newest", 160000),
	}
	reclaimToolResultBytes(context.Background(), spared)
	if len(spared[2].Content) <= toolResultAgedBytes {
		t.Fatalf("the newest result was aged while older ones could still pay: %d bytes", len(spared[2].Content))
	}
	if after := liveToolResultBytes(spared); after > toolResultReclaimCeilingBytes {
		t.Fatalf("sparing the newest must not break the budget: %d", after)
	}

	// Older results cannot: every one of them is already at its floor, so the
	// newest has to give way rather than let the window exceed the budget.
	// Enough older results that even at their 4 KiB floor they cannot carry the
	// window under the ceiling on their own.
	forced := make([]llm.Message, 0, 31)
	for i := 0; i < 30; i++ {
		forced = append(forced, artifactBackedResult(fmt.Sprintf("art_floor_%d", i), 21000))
	}
	forced = append(forced, artifactBackedResult("art_newest", 160000))
	reclaimToolResultBytes(context.Background(), forced)
	if after := liveToolResultBytes(forced); after > toolResultReclaimCeilingBytes {
		t.Fatalf("the budget was not enforced when only the newest could pay: %d", after)
	}
	if len(forced[30].Content) > toolResultAgedBytes {
		t.Fatalf("the newest result was spared past the budget: %d bytes", len(forced[11].Content))
	}
}

// TestToolResultReclaimIgnoresAnOrdinaryWorkingWindow is the regression guard
// for the change that removed the rolling window. A run's ordinary tool output
// measured 71 KiB on average and peaked at 143 KiB; none of that may be
// rewritten, because every rewrite moves the request prefix and the provider
// re-sends everything after it uncached.
func TestToolResultReclaimIgnoresAnOrdinaryWorkingWindow(t *testing.T) {
	var messages []llm.Message
	for i := 0; i < 8; i++ {
		messages = append(messages, artifactBackedResult(fmt.Sprintf("art_ordinary_%d", i), 18000))
	}
	live := liveToolResultBytes(messages)
	if live < 140*1024 {
		t.Fatalf("fixture must cover the observed peak working size: %d", live)
	}
	before := append([]string(nil), contentsOf(messages)...)
	if shrunk := reclaimToolResultBytes(context.Background(), messages); shrunk != 0 {
		t.Fatalf("an ordinary working window was rewritten: %d messages", shrunk)
	}
	for i, content := range contentsOf(messages) {
		if content != before[i] {
			t.Fatalf("message %d was rewritten below the reclaim ceiling", i)
		}
	}
}
