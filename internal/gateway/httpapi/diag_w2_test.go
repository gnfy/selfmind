package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
)

func eventWith(eventType string, payload map[string]interface{}) control.Event {
	raw, _ := json.Marshal(payload)
	return control.Event{Type: eventType, Payload: raw}
}

func TestContextBreakdownDetailRendersSections(t *testing.T) {
	events := []control.Event{eventWith("context.breakdown", map[string]interface{}{
		"identity": 400, "tools": 1000, "project_context": 15000,
		"memory": 200, "runtime": 600, "recall": 200, "artifacts": 200,
		"history": 2200, "tool_results": 200, "total": 20000,
		"stable": 1400, "volatile": 15000, "stable_prefix_hash": "abcd1234",
	})}
	out := contextBreakdownDetail(events)
	for _, want := range []string{"~20000 tok", "project context (AGENTS.md)", "75%", "semantic recall", "artifact references", "tool results", "history", "prefix abcd1234"} {
		if !strings.Contains(out, want) {
			t.Fatalf("breakdown detail missing %q:\n%s", want, out)
		}
	}
	if contextBreakdownDetail(nil) != "" {
		t.Fatal("no events must render empty")
	}
}

func TestLatestProviderContextBreakdownLine(t *testing.T) {
	out := latestProviderContextBreakdownLine([]control.Event{eventWith("provider.call.context_breakdown", map[string]interface{}{
		"iteration": 2, "transport": "stream", "estimated_total": 1234,
		"stable_system": 300, "tool_schemas": 400, "history": 200,
		"current_tool_results": 100, "workspace": 80, "task_runtime": 60,
		"skill": 50, "recall": 40, "memory": 30, "artifacts": 24,
	})})
	for _, want := range []string{"#2 stream", "~1234 tok", "tool schemas 400", "tool results 100", "skill 50", "recall 40", "artifacts 24"} {
		if !strings.Contains(out, want) {
			t.Fatalf("provider breakdown missing %q: %s", want, out)
		}
	}
}

func TestLatestCompactionLine(t *testing.T) {
	events := []control.Event{eventWith("context.compacted", map[string]interface{}{
		"before_tokens": 45000, "after_tokens": 18000, "messages_replaced": 32, "duration_ms": 1200,
	})}
	out := latestCompactionLine(events)
	for _, want := range []string{"45000", "18000", "32 messages", "1200ms"} {
		if !strings.Contains(out, want) {
			t.Fatalf("compaction line missing %q: %s", want, out)
		}
	}
	if !strings.Contains(latestCompactionLine(nil), "not triggered") {
		t.Fatal("absence must be explicit")
	}
}

func TestLatestPromptCacheLine(t *testing.T) {
	events := []control.Event{
		eventWith("provider.call.usage", map[string]interface{}{
			"iteration": 3, "transport": "stream", "status": "succeeded", "duration_ms": 240,
			"input_tokens": 250, "output_tokens": 40, "cache_read_input_tokens": 200,
			"cache_miss_input_tokens": 50, "cache_usage_reported": true,
			"cache_creation_input_tokens": 20, "cache_creation_reported": true, "uncached_input_tokens": 50,
		}),
		eventWith("token.updated", map[string]interface{}{
			"input_tokens": 1000, "output_tokens": 80, "cache_read_input_tokens": 800,
			"cache_miss_input_tokens": 200, "cache_usage_reported": true,
			"cache_creation_input_tokens": 120, "cache_creation_reported": true, "uncached_input_tokens": 200,
		}),
	}
	out := latestPromptCacheLine(events)
	for _, want := range []string{
		"Provider call (#3 stream, succeeded, 240ms)", "input 250 tok", "cache hit 200 tok (80%)",
		"cache miss 50 tok", "Prompt cache (run snapshot): hit 800 tok (80%)", "created 120 tok", "uncached 200 tok",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt cache line missing %q: %s", want, out)
		}
	}
	if !strings.Contains(latestPromptCacheLine(nil), "no provider usage data") {
		t.Fatal("absence must be explicit")
	}
}

func TestLatestPromptCacheLineReportsUnavailableCreationCounter(t *testing.T) {
	out := latestPromptCacheLine([]control.Event{eventWith("provider.call.usage", map[string]interface{}{
		"iteration": 1, "transport": "responses", "status": "succeeded",
		"input_tokens": 100, "cache_read_input_tokens": 80, "uncached_input_tokens": 20,
	})})
	if !strings.Contains(out, "created n/a") {
		t.Fatalf("missing creation counter must not look like a real zero: %s", out)
	}
}

func TestPromptCacheAggregateLine(t *testing.T) {
	events := []control.Event{
		eventWith("provider.call.usage", map[string]interface{}{
			"input_tokens": 100, "cache_read_input_tokens": 80, "cache_miss_input_tokens": 20,
			"cache_usage_reported": true, "cache_creation_input_tokens": 10, "cache_creation_reported": true,
		}),
		eventWith("provider.call.usage", map[string]interface{}{
			"input_tokens": 200, "cache_read_input_tokens": 0, "cache_miss_input_tokens": 200,
			"cache_usage_reported": true, "cache_creation_reported": false,
		}),
		eventWith("token.updated", map[string]interface{}{"input_tokens": 999}),
	}
	out := promptCacheAggregateLine(events)
	for _, want := range []string{
		"visible 2 calls", "hit 80/300 tok (26%)", "cache miss 220 tok", "hits 1/2", "created 10 tok",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("aggregate cache line missing %q: %s", want, out)
		}
	}
	if got := promptCacheAggregateLine(nil); got != "" {
		t.Fatalf("empty events must not add a duplicate absence line: %q", got)
	}
}

func TestPromptCacheAggregateLineReportsUnavailableCreationCounter(t *testing.T) {
	out := promptCacheAggregateLine([]control.Event{eventWith("provider.call.usage", map[string]interface{}{
		"input_tokens": 100, "cache_read_input_tokens": 80, "uncached_input_tokens": 20,
	})})
	if !strings.Contains(out, "creation not reported by this transport") {
		t.Fatalf("missing creation counter must be explicit: %s", out)
	}
}

func TestPromptPrefixStabilityLine(t *testing.T) {
	stable := []control.Event{
		eventWith("context.breakdown", map[string]interface{}{"stable_prefix_hash": "same"}),
		eventWith("context.breakdown", map[string]interface{}{"stable_prefix_hash": "same"}),
	}
	if got := promptPrefixStabilityLine(stable); !strings.Contains(got, "stable across") {
		t.Fatalf("stable prefix not reported: %s", got)
	}
	changed := []control.Event{
		eventWith("context.breakdown", map[string]interface{}{"stable_prefix_hash": "new"}),
		eventWith("context.breakdown", map[string]interface{}{"stable_prefix_hash": "old"}),
	}
	if got := promptPrefixStabilityLine(changed); !strings.Contains(got, "changed") {
		t.Fatalf("changed prefix not reported: %s", got)
	}
}

func TestLatestRecallLine(t *testing.T) {
	hit := []control.Event{eventWith("context.recall", map[string]interface{}{
		"candidates": map[string]int{"canonical": 4, "taskcard": 2, "session": 5},
		"sources":    map[string]int{"taskcard": 1, "session": 2}, "slices": 3, "expanded": true, "elapsed_ms": 45,
	})}
	out := latestRecallLine(hit)
	for _, want := range []string{"candidates [canonical=4", "selected 3 slice(s)", "session=2", "taskcard=1", "query expanded", "45ms"} {
		if !strings.Contains(out, want) {
			t.Fatalf("recall line missing %q: %s", want, out)
		}
	}
	skipped := []control.Event{eventWith("context.recall", map[string]interface{}{"skipped": "short_message"})}
	if !strings.Contains(latestRecallLine(skipped), "skipped (short_message)") {
		t.Fatalf("skip reason must render: %s", latestRecallLine(skipped))
	}
	if !strings.Contains(latestRecallLine(nil), "no recall event") {
		t.Fatal("absence must be explicit")
	}
}

// TestStuckTaskLines: only old interrupted/blocked (>48h) and old in_progress
// (>7d) work is flagged; fresh work and terminal statuses never are.
func TestStuckTaskLines(t *testing.T) {
	now := time.Now()
	card := func(title, status string, age time.Duration) control.TaskCard {
		return control.TaskCard{TaskID: "task_" + title, Title: title, Status: status, UpdatedAt: now.Add(-age)}
	}
	lines := stuckTaskLines([]control.TaskCard{
		card("stale-interrupted", "interrupted", 72*time.Hour),
		card("stale-blocked", "blocked", 49*time.Hour),
		card("dormant-progress", "in_progress", 8*24*time.Hour),
		card("fresh-interrupted", "interrupted", time.Hour),
		card("fresh-progress", "in_progress", 24*time.Hour),
		card("old-done", "done", 30*24*time.Hour),
	}, now)
	joined := strings.Join(lines, "")
	for _, want := range []string{"stale-interrupted", "stale-blocked", "dormant-progress", "idle 8d"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q:\n%s", want, joined)
		}
	}
	for _, forbidden := range []string{"fresh-interrupted", "fresh-progress", "old-done"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("%q must not be flagged:\n%s", forbidden, joined)
		}
	}
	// Oldest first.
	if len(lines) != 3 || !strings.Contains(lines[0], "dormant-progress") {
		t.Fatalf("expected 3 lines oldest-first: %v", lines)
	}
}

// TestTasksDiagReplySmoke: end-to-end render over a real (fresh) store — no
// stuck work, zero waiting counters.
func TestTasksDiagReplySmoke(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	identity, err := store.ResolveOrCreateAccount(ctx, "tenant", "cli", "local", "Tester")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "fresh work",
	}); err != nil {
		t.Fatal(err)
	}
	d := &Server{Control: store, DefaultTenantID: identity.TenantID}
	out, err := d.tasksDiagReply(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Task diagnostics", "Labels: open 1", "Waiting: queued 0, pending approvals 0, pending questions 0", "Possibly stuck: none"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}
