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
		"memory": 200, "runtime": 1200, "history": 2200, "total": 20000,
	})}
	out := contextBreakdownDetail(events)
	for _, want := range []string{"~20000 tok", "project context (AGENTS.md)", "75%", "history"} {
		if !strings.Contains(out, want) {
			t.Fatalf("breakdown detail missing %q:\n%s", want, out)
		}
	}
	if contextBreakdownDetail(nil) != "" {
		t.Fatal("no events must render empty")
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

func TestLatestRecallLine(t *testing.T) {
	hit := []control.Event{eventWith("context.recall", map[string]interface{}{
		"sources": map[string]int{"taskcard": 1, "session": 2}, "slices": 3, "expanded": true, "elapsed_ms": 45,
	})}
	out := latestRecallLine(hit)
	for _, want := range []string{"3 slice(s)", "session=2", "taskcard=1", "query expanded", "45ms"} {
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
