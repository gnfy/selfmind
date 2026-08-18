package httpapi

import (
	"encoding/json"
	"testing"
	"time"

	"selfmind/internal/control"
)

func TestParseDailyReportWindow(t *testing.T) {
	if got, err := parseDailyReportWindow("/report daily --since 48h"); err != nil || got != 48*time.Hour {
		t.Fatalf("got=%s err=%v", got, err)
	}
	if got, err := parseDailyReportWindow("/report daily --since 1000h"); err != nil || got != maxDailyReportWindow {
		t.Fatalf("clamped got=%s err=%v", got, err)
	}
}

func TestCollectDailyQualityStats(t *testing.T) {
	payload := func(v interface{}) json.RawMessage { b, _ := json.Marshal(v); return b }
	events := []control.Event{
		{RunID: "run_1", Type: "run.finished", Payload: payload(map[string]interface{}{"outcome": map[string]interface{}{"status": "done", "completion_reason": "completed"}})},
		{Type: "provider.call.usage", Payload: payload(map[string]interface{}{"input_tokens": 100, "output_tokens": 7, "cache_read_input_tokens": 80, "cache_miss_input_tokens": 20, "duration_ms": 900})},
		{RunID: "run_resume", Type: "run.started", Payload: payload(map[string]interface{}{"origin": "approval", "source_approval_id": "apr_1"})},
		{RunID: "run_resume", Type: "provider.call.usage", Payload: payload(map[string]interface{}{"input_tokens": 40, "output_tokens": 5, "cache_read_input_tokens": 30, "cache_miss_input_tokens": 10, "duration_ms": 300})},
		{RunID: "run_resume", Type: "run.finished", Payload: payload(map[string]interface{}{"outcome": map[string]interface{}{"status": "done", "completion_reason": "completed"}})},
		{Type: "tool.completed", Payload: payload(map[string]interface{}{"error": "exit status 1", "error_category": "command_failed"})},
		{Type: "tool.completed", Payload: payload(map[string]interface{}{"error": "use watch_external", "error_category": "policy_redirect"})},
		{Type: "context.recall", Payload: payload(map[string]interface{}{"candidates": map[string]int{"canonical": 5}, "sources": map[string]int{"canonical": 2}, "slices": 2})},
		{Type: "context.recall", Payload: payload(map[string]interface{}{"skipped": "short_message"})},
		{Type: "context.recall_usage", Payload: payload(map[string]interface{}{"output_overlap": 1, "overlap_sources": map[string]int{"canonical": 1}})},
		{Type: "memory.disposition", Payload: payload(map[string]interface{}{"counts": map[string]int{"add": 2, "duplicate": 1}})},
	}
	stats := collectDailyQualityStats(events)
	if stats.RunStatuses["done"] != 2 || stats.ProviderCalls != 2 || stats.CacheReadTokens != 110 || stats.ToolFailures != 1 || stats.ToolPolicyRedirects != 1 || stats.ToolFailureClasses["command_failed"] != 1 || stats.RecallCandidates != 5 || stats.RecallCandidateSources["canonical"] != 5 || stats.RecallSelected != 2 || stats.RecallSelectedSources["canonical"] != 2 || stats.RecallOverlap != 1 || stats.RecallOverlapSources["canonical"] != 1 || stats.RecallSkipped["short_message"] != 1 || stats.MemoryDisposition["add"] != 2 || stats.MemoryDisposition["duplicate"] != 1 || stats.ContinuationRuns != 1 || stats.ContinuationStatuses["done"] != 1 || stats.ContinuationCalls != 1 || stats.ContinuationInput != 40 || stats.ContinuationOutput != 5 || stats.ContinuationCacheRead != 30 || stats.ContinuationCacheMiss != 10 {
		t.Fatalf("stats=%+v", stats)
	}
}
