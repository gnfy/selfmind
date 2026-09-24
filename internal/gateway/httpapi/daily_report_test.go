package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
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

func TestDailyQualityReportMarksUnavailableEvidenceInsteadOfZeros(t *testing.T) {
	dir := t.TempDir()
	store, err := control.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "user-a", "User A")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"approval_requests", "outbound_messages", "approval_triage_events", "maintenance_provider_calls", "maintenance_jobs"} {
		if _, err := db.Exec("DROP TABLE " + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}

	report, err := (&Server{Control: store}).dailyQualityReport(context.Background(), identity, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"triage: unavailable", "Approval backlog now: unavailable", "Delivery: unavailable",
		"Maintenance: unavailable", "Evidence gaps: unavailable:", "Zero values from those sources were not reported as evidence",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

func TestCollectDailyQualityStats(t *testing.T) {
	payload := func(v interface{}) json.RawMessage { b, _ := json.Marshal(v); return b }
	events := []control.Event{
		{RunID: "run_1", Type: "run.finished", Payload: payload(map[string]interface{}{"outcome": map[string]interface{}{"status": "done", "completion_reason": "completed"}})},
		{Type: "provider.call.usage", Payload: payload(map[string]interface{}{"provider": "bailian", "model": "qwen-example", "role": "coding_agent", "input_tokens": 100, "output_tokens": 7, "reasoning_output_tokens": 3, "cache_read_input_tokens": 80, "cache_miss_input_tokens": 20, "duration_ms": 900})},
		{RunID: "run_resume", Type: "run.started", Payload: payload(map[string]interface{}{"origin": "approval", "source_approval_id": "apr_1"})},
		{RunID: "run_resume", Type: "run.resumed", Payload: payload(map[string]interface{}{"resumes_run_id": "run_1"})},
		{RunID: "run_resume", Type: "provider.call.usage", Payload: payload(map[string]interface{}{"input_tokens": 40, "output_tokens": 5, "cache_read_input_tokens": 30, "cache_miss_input_tokens": 10, "duration_ms": 300})},
		{RunID: "run_resume", Type: "run.finished", Payload: payload(map[string]interface{}{"outcome": map[string]interface{}{"status": "done", "completion_reason": "completed"}})},
		{RunID: "run_failed", Type: "tool.started", Payload: payload(map[string]interface{}{"tool": "terminal", "tool_call_id": "call-1"})},
		{RunID: "run_failed", Type: "tool.completed", Payload: payload(map[string]interface{}{"tool": "terminal", "tool_call_id": "call-1", "error": "same attempt", "error_category": "policy_redirect", "error_code": "recovery_attempt_repeated", "failure_phase": "preparation", "effect_state": "not_dispatched"})},
		{RunID: "run_failed", Type: "approval.requested", Payload: payload(map[string]interface{}{"approval_id": "apr_2"})},
		{RunID: "run_parent", Type: "run.recovery_scheduled", Payload: payload(map[string]interface{}{"mode": "verify_only"})},
		{RunID: "run_recovery", Type: "run.started", Payload: payload(map[string]interface{}{"origin": "recovery"})},
		{RunID: "run_recovery", Type: "run.finished", Payload: payload(map[string]interface{}{"outcome": map[string]interface{}{"status": "done", "completion_reason": "completed"}})},
		{RunID: "run_recovery", Type: "external_watch.group_resolved", Payload: payload(map[string]interface{}{"mode": "all", "status": "succeeded"})},
		{RunID: "run_failed", Type: "tool.completed", Payload: payload(map[string]interface{}{"tool": "tool_search", "tool_call_id": "call-2", "error": "exit status 1", "error_category": "command_failed", "failure_phase": "validation", "effect_state": "not_dispatched"})},
		{Type: "tool.completed", Payload: payload(map[string]interface{}{"error": "use watch_external", "error_category": "policy_redirect", "error_code": "active_turn_polling"})},
		{Type: "context.recall", Payload: payload(map[string]interface{}{"candidates": map[string]int{"canonical": 5}, "sources": map[string]int{"canonical": 2}, "slices": 2})},
		{Type: "context.recall", Payload: payload(map[string]interface{}{"skipped": "short_message"})},
		{Type: "context.recall_usage", Payload: payload(map[string]interface{}{"output_overlap": 1, "overlap_sources": map[string]int{"canonical": 1}})},
		{Type: "memory.disposition", Payload: payload(map[string]interface{}{"counts": map[string]int{"add": 2, "duplicate": 1}})},
		{Type: "provider.call.context_breakdown", Payload: payload(map[string]interface{}{"estimated_total": 1000, "tool_schemas": 400, "provider_fingerprint_state": "available", "provider_prefix_hash": "prefix-a"})},
	}
	stats := collectDailyQualityStats(events)
	if stats.RunStatuses["done"] != 3 || stats.LogicalChains != 2 || stats.LogicalChainStatuses["done"] != 2 || stats.ResumeEdges != 1 || stats.ResumeOrigins["approval"] != 1 || stats.ProviderCalls != 2 || stats.CacheReadTokens != 110 || stats.ToolCalls != 3 || stats.ToolDispatched != 1 || stats.ToolRejectedPreDispatch != 1 || stats.ToolFailures != 1 || stats.ToolPolicyRedirects != 2 || stats.ToolFailureClasses["command_failed"] != 1 || stats.ToolFailurePhases["validation"] != 1 || stats.ToolEffectStates["not_dispatched"] != 2 || stats.RecallCandidates != 5 || stats.RecallCandidateSources["canonical"] != 5 || stats.RecallSelected != 2 || stats.RecallSelectedSources["canonical"] != 2 || stats.RecallOverlap != 1 || stats.RecallOverlapSources["canonical"] != 1 || stats.RecallSkipped["short_message"] != 1 || stats.MemoryDisposition["add"] != 2 || stats.MemoryDisposition["duplicate"] != 1 || stats.ContinuationRuns != 1 || stats.ContinuationStatuses["done"] != 1 || stats.ContinuationCalls != 1 || stats.ContinuationInput != 40 || stats.ContinuationOutput != 5 || stats.ContinuationCacheRead != 30 || stats.ContinuationCacheMiss != 10 || stats.ContextSamples != 1 || stats.ContextToolSchemaTokens != 400 || len(stats.ProviderPrefixHashes) != 1 || stats.RecoveryScheduled["verify_only"] != 1 || stats.RecoveryRuns != 1 || stats.RecoveryStatuses["done"] != 1 || stats.RecoveryGuardrails["recovery_attempt_repeated"] != 1 || stats.WaitGroupOutcomes["all:succeeded"] != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	if stats.ToolCallsByName["tool_search"] != 1 {
		t.Fatalf("tool names were not read from durable event payload: %+v", stats.ToolCallsByName)
	}
	if stats.ToolRedirectReasons["active_turn_polling"] != 1 || stats.ToolRedirectReasons["recovery_attempt_repeated"] != 1 || stats.ToolFailureClasses["policy_redirect"] != 0 {
		t.Fatalf("policy redirect was counted as an execution failure: %+v", stats)
	}
	if route := stats.ProviderRoutes["bailian/qwen-example"]; route.Calls != 1 || route.DurationMS != 900 || route.ReasoningTokens != 3 || !strings.Contains(formatDailyRouteUsage(stats.ProviderRoutes), "unattributed 1 calls") {
		t.Fatalf("provider routes lost attribution or legacy unknown state: %+v", stats.ProviderRoutes)
	}
}

func TestApprovalUsageDoesNotDoubleCountMainOrInventHistoricalInput(t *testing.T) {
	events := []control.Event{
		{Type: "provider.call.usage", Payload: []byte(`{"input_tokens":100,"output_tokens":10}`)},
		{Type: "approval.response", Payload: []byte(`{"response":{"version":2,"role":"fast_classifier","usage":{"input_tokens":50,"output_tokens":5,"cache_read_input_tokens":30,"cache_miss_input_tokens":20}}}`)},
		{Type: "approval.response", Payload: []byte(`{"response":{"version":1,"output_tokens":7}}`)},
	}
	s := collectDailyQualityStats(events)
	if s.ProviderCalls != 1 || s.InputTokens != 100 || s.ApprovalModelCalls != 2 || s.ApprovalUsageMissing != 1 || s.ApprovalInputTokens != 50 || s.ApprovalOutputTokens != 5 || s.ApprovalCacheReadTokens != 30 || s.ApprovalCacheMissTokens != 20 {
		t.Fatalf("accounting=%+v", s)
	}
}
