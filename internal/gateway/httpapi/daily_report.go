package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

const (
	defaultDailyReportWindow = 24 * time.Hour
	maxDailyReportWindow     = 30 * 24 * time.Hour
)

type dailyQualityStats struct {
	RunStatuses            map[string]int
	CompletionReasons      map[string]int
	ExternalStatuses       map[string]int
	ProviderCalls          int
	InputTokens            int64
	OutputTokens           int64
	CacheReadTokens        int64
	CacheMissTokens        int64
	ProviderLatencyMS      int64
	ToolCalls              int
	ToolFailures           int
	ToolPolicyRedirects    int
	ToolFailureClasses     map[string]int
	ApprovalCounts         map[string]int
	RecallCandidates       int
	RecallSelected         int
	RecallOverlap          int
	RecallCandidateSources map[string]int
	RecallSelectedSources  map[string]int
	RecallOverlapSources   map[string]int
	RecallSkipped          map[string]int
	MemoryDisposition      map[string]int
}

func parseDailyReportWindow(input string) (time.Duration, error) {
	window := defaultDailyReportWindow
	fields := strings.Fields(input)
	for i := 0; i < len(fields); i++ {
		if fields[i] != "--since" {
			continue
		}
		if i+1 >= len(fields) {
			return 0, fmt.Errorf("--since requires a duration such as 24h")
		}
		parsed, err := time.ParseDuration(fields[i+1])
		if err != nil || parsed <= 0 {
			return 0, fmt.Errorf("invalid --since duration %q", fields[i+1])
		}
		window = parsed
		i++
	}
	if window > maxDailyReportWindow {
		window = maxDailyReportWindow
	}
	return window, nil
}

func collectDailyQualityStats(events []control.Event) dailyQualityStats {
	stats := dailyQualityStats{
		RunStatuses:            make(map[string]int),
		CompletionReasons:      make(map[string]int),
		ExternalStatuses:       make(map[string]int),
		ApprovalCounts:         make(map[string]int),
		MemoryDisposition:      make(map[string]int),
		ToolFailureClasses:     make(map[string]int),
		RecallCandidateSources: make(map[string]int),
		RecallSelectedSources:  make(map[string]int),
		RecallOverlapSources:   make(map[string]int),
		RecallSkipped:          make(map[string]int),
	}
	terminalRuns := make(map[string]bool)
	for _, event := range events {
		switch event.Type {
		case "run.finished", "run.interrupted", "run.failed", "run.cancelled":
			if event.RunID != "" && terminalRuns[event.RunID] {
				continue
			}
			var payload struct {
				Outcome api.RunOutcome `json:"outcome"`
			}
			_ = json.Unmarshal(event.Payload, &payload)
			status := strings.TrimSpace(payload.Outcome.Status)
			if status == "" {
				status = strings.TrimPrefix(event.Type, "run.")
			}
			stats.RunStatuses[status]++
			if reason := strings.TrimSpace(payload.Outcome.CompletionReason); reason != "" {
				stats.CompletionReasons[reason]++
			}
			if payload.Outcome.External != nil {
				if status := strings.TrimSpace(payload.Outcome.External.Status); status != "" {
					stats.ExternalStatuses[status]++
				}
			}
			if event.RunID != "" {
				terminalRuns[event.RunID] = true
			}
		case "provider.call.usage":
			var p struct {
				InputTokens     int64 `json:"input_tokens"`
				OutputTokens    int64 `json:"output_tokens"`
				CacheReadTokens int64 `json:"cache_read_input_tokens"`
				CacheMissTokens int64 `json:"cache_miss_input_tokens"`
				DurationMS      int64 `json:"duration_ms"`
			}
			if json.Unmarshal(event.Payload, &p) == nil {
				stats.ProviderCalls++
				stats.InputTokens += p.InputTokens
				stats.OutputTokens += p.OutputTokens
				stats.CacheReadTokens += p.CacheReadTokens
				if p.CacheMissTokens > 0 {
					stats.CacheMissTokens += p.CacheMissTokens
				} else {
					stats.CacheMissTokens += max(p.InputTokens-p.CacheReadTokens, 0)
				}
				stats.ProviderLatencyMS += p.DurationMS
			}
		case "tool.completed":
			stats.ToolCalls++
			var p struct {
				Error         string `json:"error"`
				ErrorCategory string `json:"error_category"`
			}
			if json.Unmarshal(event.Payload, &p) == nil && strings.TrimSpace(p.Error) != "" {
				category := strings.TrimSpace(p.ErrorCategory)
				if category == "" {
					category = "unknown"
				}
				if category == "policy_redirect" {
					stats.ToolPolicyRedirects++
				} else {
					stats.ToolFailures++
					stats.ToolFailureClasses[category]++
				}
			}
		case "approval.requested", "approval.approved", "approval.rejected", "approval.expired":
			stats.ApprovalCounts[strings.TrimPrefix(event.Type, "approval.")]++
		case "context.recall":
			var p struct {
				Candidates map[string]int `json:"candidates"`
				Sources    map[string]int `json:"sources"`
				Slices     int            `json:"slices"`
				Skipped    string         `json:"skipped"`
			}
			if json.Unmarshal(event.Payload, &p) == nil {
				for source, count := range p.Candidates {
					stats.RecallCandidates += count
					stats.RecallCandidateSources[source] += count
				}
				stats.RecallSelected += p.Slices
				for source, count := range p.Sources {
					stats.RecallSelectedSources[source] += count
				}
				if reason := strings.TrimSpace(p.Skipped); reason != "" {
					stats.RecallSkipped[reason]++
				}
			}
		case "context.recall_usage":
			var p struct {
				OutputOverlap  int            `json:"output_overlap"`
				OverlapSources map[string]int `json:"overlap_sources"`
			}
			if json.Unmarshal(event.Payload, &p) == nil {
				stats.RecallOverlap += p.OutputOverlap
				for source, count := range p.OverlapSources {
					stats.RecallOverlapSources[source] += count
				}
			}
		case "memory.disposition":
			var p struct {
				Counts map[string]int `json:"counts"`
			}
			if json.Unmarshal(event.Payload, &p) == nil {
				for reason, count := range p.Counts {
					stats.MemoryDisposition[reason] += count
				}
			}
		}
	}
	return stats
}

func (d *Server) dailyQualityReport(ctx context.Context, identity *control.IdentityContext, window time.Duration) (string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return "Daily report unavailable.", nil
	}
	since := time.Now().Add(-window)
	events, err := d.Control.ListPersonEventsSince(ctx, identity.TenantID, identity.PersonID, since, 10000)
	if err != nil {
		return "", err
	}
	stats := collectDailyQualityStats(events)
	deliveryCounts, _ := d.Control.CountOutboundByStatusSince(ctx, identity.TenantID, identity.PersonID, since)
	triage, _ := d.Control.ApprovalTriageStatsSince(ctx, identity.TenantID, identity.PersonID, since)
	maintenance, _ := d.Control.MaintenanceProviderUsageForPersonSince(ctx, identity.TenantID, identity.PersonID, since)
	health, _ := d.Control.MaintenanceHealthForPerson(ctx, identity.TenantID, identity.PersonID)

	var maintenanceCalls, maintenanceFailed, maintenanceOpen int
	var maintenanceInput, maintenanceOutput, maintenanceCacheRead, maintenanceCacheMiss, maintenanceReasoning int64
	for _, item := range maintenance {
		maintenanceCalls += item.Calls
		maintenanceFailed += item.Failed
		maintenanceOpen += item.CircuitOpen
		maintenanceInput += item.InputTokens
		maintenanceOutput += item.OutputTokens
		maintenanceCacheRead += item.CacheReadInputTokens
		maintenanceCacheMiss += item.CacheMissInputTokens
		maintenanceReasoning += item.ReasoningOutputTokens
	}

	cacheRate := 0
	if stats.InputTokens > 0 {
		cacheRate = int(stats.CacheReadTokens * 100 / stats.InputTokens)
	}
	toolFailureRate := 0
	if stats.ToolCalls > 0 {
		toolFailureRate = stats.ToolFailures * 100 / stats.ToolCalls
	}
	avgLatency := int64(0)
	if stats.ProviderCalls > 0 {
		avgLatency = stats.ProviderLatencyMS / int64(stats.ProviderCalls)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Daily quality report (last %s)\n", window.Round(time.Minute))
	fmt.Fprintf(&sb, "Runs: %s\n", formatCountMap(stats.RunStatuses))
	fmt.Fprintf(&sb, "Completion reasons: %s\n", formatCountMap(stats.CompletionReasons))
	fmt.Fprintf(&sb, "External outcomes: %s\n", formatCountMap(stats.ExternalStatuses))
	fmt.Fprintf(&sb, "Model: %d calls, input %d, cache read %d (%d%%), uncached %d, output %d, avg latency %dms\n",
		stats.ProviderCalls, stats.InputTokens, stats.CacheReadTokens, cacheRate, stats.CacheMissTokens, stats.OutputTokens, avgLatency)
	fmt.Fprintf(&sb, "Tools: %d calls, %d failed (%d%%), %d policy redirects; failures by class: %s\n",
		stats.ToolCalls, stats.ToolFailures, toolFailureRate, stats.ToolPolicyRedirects, formatCountMap(stats.ToolFailureClasses))
	fmt.Fprintf(&sb, "Approvals: %s; triage: %s\n", formatCountMap(stats.ApprovalCounts), formatCountMap(triage.Counts))
	fmt.Fprintf(&sb, "Delivery: %s\n", formatCountMap(deliveryCounts))
	fmt.Fprintf(&sb, "Recall: %d candidates (%s), %d selected (%s), %d output-overlap signals (%s; not causal proof); skipped: %s\n",
		stats.RecallCandidates, formatCountMap(stats.RecallCandidateSources),
		stats.RecallSelected, formatCountMap(stats.RecallSelectedSources),
		stats.RecallOverlap, formatCountMap(stats.RecallOverlapSources), formatCountMap(stats.RecallSkipped))
	fmt.Fprintf(&sb, "Memory disposition: %s\n", formatCountMap(stats.MemoryDisposition))
	fmt.Fprintf(&sb, "Maintenance: %d calls, %d failed, %d circuit-open, input %d, cache read %d, uncached %d, output %d, reasoning %d; jobs pending %d, failed %d, blocked %d\n",
		maintenanceCalls, maintenanceFailed, maintenanceOpen, maintenanceInput, maintenanceCacheRead, maintenanceCacheMiss, maintenanceOutput, maintenanceReasoning,
		health.Pending, health.Failed, health.Blocked)
	if !health.LastSuccessAt.IsZero() {
		fmt.Fprintf(&sb, "Maintenance last success: %s\n", health.LastSuccessAt.Local().Format(time.RFC3339))
	}
	if reason := strings.TrimSpace(health.LastError); reason != "" {
		fmt.Fprintf(&sb, "Maintenance last error: %s\n", truncate(toOneLine(reason), 180))
	}
	if health.Blocked > 0 || health.Failed > 0 {
		sb.WriteString("Memory baseline: degraded; write/disposition metrics are incomplete until maintenance recovers.\n")
	}
	if len(events) == 10000 {
		sb.WriteString("Note: event window reached the 10,000-row diagnostic cap.\n")
	}
	return strings.TrimSpace(sb.String()), nil
}

func formatCountMap(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+" "+strconv.Itoa(counts[key]))
	}
	return strings.Join(parts, ", ")
}
