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
	dailyReportEventPageSize = 5000
	dailyReportEventLimit    = 100000
)

type dailyQualityStats struct {
	RunStatuses             map[string]int
	CompletionReasons       map[string]int
	ExternalStatuses        map[string]int
	RecoveryScheduled       map[string]int
	RecoveryRuns            int
	RecoveryStatuses        map[string]int
	RecoveryGuardrails      map[string]int
	PostFailureApprovals    int
	WaitGroupOutcomes       map[string]int
	ProviderCalls           int
	InputTokens             int64
	OutputTokens            int64
	CacheReadTokens         int64
	CacheMissTokens         int64
	ProviderLatencyMS       int64
	ToolCalls               int
	ToolFailures            int
	ToolPolicyRedirects     int
	ToolFailureClasses      map[string]int
	ToolCallsByName         map[string]int
	ApprovalCounts          map[string]int
	RecallCandidates        int
	RecallSelected          int
	RecallOverlap           int
	RecallCandidateSources  map[string]int
	RecallSelectedSources   map[string]int
	RecallOverlapSources    map[string]int
	RecallSkipped           map[string]int
	MemoryDisposition       map[string]int
	ContinuationRuns        int
	ContinuationStatuses    map[string]int
	ContinuationCalls       int
	ContinuationInput       int64
	ContinuationOutput      int64
	ContinuationCacheRead   int64
	ContinuationCacheMiss   int64
	ContextSamples          int
	ContextEstimatedTokens  int64
	ContextToolSchemaTokens int64
	FingerprintStates       map[string]int
	ProviderPrefixHashes    map[string]bool
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
		RecoveryScheduled:      make(map[string]int),
		RecoveryStatuses:       make(map[string]int),
		RecoveryGuardrails:     make(map[string]int),
		WaitGroupOutcomes:      make(map[string]int),
		ApprovalCounts:         make(map[string]int),
		MemoryDisposition:      make(map[string]int),
		ToolFailureClasses:     make(map[string]int),
		ToolCallsByName:        make(map[string]int),
		RecallCandidateSources: make(map[string]int),
		RecallSelectedSources:  make(map[string]int),
		RecallOverlapSources:   make(map[string]int),
		RecallSkipped:          make(map[string]int),
		ContinuationStatuses:   make(map[string]int),
		FingerprintStates:      make(map[string]int),
		ProviderPrefixHashes:   make(map[string]bool),
	}
	runOrigins := make(map[string]string)
	for _, event := range events {
		if event.Type != "run.started" || strings.TrimSpace(event.RunID) == "" {
			continue
		}
		var payload struct {
			Origin string `json:"origin"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil {
			runOrigins[event.RunID] = strings.TrimSpace(payload.Origin)
			if payload.Origin == runOriginApproval {
				stats.ContinuationRuns++
			} else if payload.Origin == runOriginRecovery {
				stats.RecoveryRuns++
			}
		}
	}
	terminalRuns := make(map[string]bool)
	failedToolRuns := make(map[string]bool)
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
			if runOrigins[event.RunID] == runOriginApproval {
				stats.ContinuationStatuses[status]++
			} else if runOrigins[event.RunID] == runOriginRecovery {
				stats.RecoveryStatuses[status]++
			}
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
				if runOrigins[event.RunID] == runOriginApproval {
					stats.ContinuationCalls++
					stats.ContinuationInput += p.InputTokens
					stats.ContinuationOutput += p.OutputTokens
					stats.ContinuationCacheRead += p.CacheReadTokens
					if p.CacheMissTokens > 0 {
						stats.ContinuationCacheMiss += p.CacheMissTokens
					} else {
						stats.ContinuationCacheMiss += max(p.InputTokens-p.CacheReadTokens, 0)
					}
				}
			}
		case "tool.completed":
			stats.ToolCalls++
			var p struct {
				ToolName      string `json:"tool_name"`
				Tool          string `json:"tool"`
				Error         string `json:"error"`
				ErrorCategory string `json:"error_category"`
				ErrorCode     string `json:"error_code"`
			}
			if json.Unmarshal(event.Payload, &p) == nil && strings.TrimSpace(p.Error) != "" {
				if strings.TrimSpace(event.RunID) != "" {
					failedToolRuns[event.RunID] = true
				}
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
			if code := strings.TrimSpace(p.ErrorCode); code != "" && isRecoveryGuardrailCode(code) {
				stats.RecoveryGuardrails[code]++
			}
			name := strings.TrimSpace(p.ToolName)
			if name == "" {
				name = strings.TrimSpace(p.Tool)
			}
			if name != "" {
				stats.ToolCallsByName[name]++
			}
		case "approval.requested", "approval.approved", "approval.rejected", "approval.parked", "approval.expired", "approval.archived":
			stats.ApprovalCounts[strings.TrimPrefix(event.Type, "approval.")]++
			if event.Type == "approval.requested" && failedToolRuns[event.RunID] {
				stats.PostFailureApprovals++
			}
		case "run.recovery_scheduled":
			var p struct {
				Mode string `json:"mode"`
			}
			if json.Unmarshal(event.Payload, &p) == nil {
				mode := strings.TrimSpace(p.Mode)
				if mode == "" {
					mode = "unknown"
				}
				stats.RecoveryScheduled[mode]++
			}
		case "external_watch.group_resolved":
			var p struct {
				Mode   string `json:"mode"`
				Status string `json:"status"`
			}
			if json.Unmarshal(event.Payload, &p) == nil {
				key := strings.TrimSpace(p.Mode) + ":" + strings.TrimSpace(p.Status)
				if key != ":" {
					stats.WaitGroupOutcomes[strings.Trim(key, ":")]++
				}
			}
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
		case "provider.call.context_breakdown":
			var p struct {
				EstimatedTotal           int64  `json:"estimated_total"`
				ToolSchemas              int64  `json:"tool_schemas"`
				ProviderFingerprintState string `json:"provider_fingerprint_state"`
				ProviderPrefixHash       string `json:"provider_prefix_hash"`
			}
			if json.Unmarshal(event.Payload, &p) == nil {
				stats.ContextSamples++
				stats.ContextEstimatedTokens += p.EstimatedTotal
				stats.ContextToolSchemaTokens += p.ToolSchemas
				state := strings.TrimSpace(p.ProviderFingerprintState)
				if state == "" {
					state = "missing"
				}
				stats.FingerprintStates[state]++
				if hash := strings.TrimSpace(p.ProviderPrefixHash); hash != "" {
					stats.ProviderPrefixHashes[hash] = true
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
	generatedAt := time.Now()
	since := generatedAt.Add(-window)
	events, truncated, err := d.listDailyReportEvents(ctx, identity, since)
	if err != nil {
		return "", err
	}
	stats := collectDailyQualityStats(events)
	var evidenceGaps []string
	backlog, backlogErr := d.Control.ApprovalBacklog(ctx, identity.TenantID, identity.PersonID)
	if backlogErr != nil {
		evidenceGaps = append(evidenceGaps, "approval backlog")
	}
	deliveryCounts, deliveryErr := d.Control.CountOutboundByStatusSince(ctx, identity.TenantID, identity.PersonID, since)
	if deliveryErr != nil {
		evidenceGaps = append(evidenceGaps, "delivery status")
	}
	triage, triageErr := d.Control.ApprovalTriageStatsSince(ctx, identity.TenantID, identity.PersonID, since)
	if triageErr != nil {
		evidenceGaps = append(evidenceGaps, "approval triage")
	}
	maintenance, maintenanceErr := d.Control.MaintenanceProviderUsageForPersonSince(ctx, identity.TenantID, identity.PersonID, since)
	if maintenanceErr != nil {
		evidenceGaps = append(evidenceGaps, "maintenance provider usage")
	}
	health, healthErr := d.Control.MaintenanceHealthForPerson(ctx, identity.TenantID, identity.PersonID)
	if healthErr != nil {
		evidenceGaps = append(evidenceGaps, "maintenance health")
	}

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
	coverage := "complete within the local event store"
	if truncated {
		coverage = "lower bound at diagnostic safety cap"
	}
	fmt.Fprintf(&sb, "Evidence window: %s to %s; generated %s; scanned %d event(s); coverage %s\n",
		since.Local().Format(time.RFC3339), generatedAt.Local().Format(time.RFC3339),
		generatedAt.Local().Format(time.RFC3339), len(events), coverage)
	fmt.Fprintf(&sb, "Runs: %s\n", formatCountMap(stats.RunStatuses))
	fmt.Fprintf(&sb, "Completion reasons: %s\n", formatCountMap(stats.CompletionReasons))
	fmt.Fprintf(&sb, "External outcomes: %s\n", formatCountMap(stats.ExternalStatuses))
	fmt.Fprintf(&sb, "Automatic recovery: scheduled %s; %d child run(s), outcomes %s; guardrails %s\n",
		formatCountMap(stats.RecoveryScheduled), stats.RecoveryRuns,
		formatCountMap(stats.RecoveryStatuses), formatCountMap(stats.RecoveryGuardrails))
	fmt.Fprintf(&sb, "Durable waits: groups %s; post-failure approvals %d\n",
		formatCountMap(stats.WaitGroupOutcomes), stats.PostFailureApprovals)
	fmt.Fprintf(&sb, "Model: %d calls, input %d, cache read %d (%d%%), uncached %d, output %d, avg latency %dms\n",
		stats.ProviderCalls, stats.InputTokens, stats.CacheReadTokens, cacheRate, stats.CacheMissTokens, stats.OutputTokens, avgLatency)
	if stats.ContextSamples > 0 {
		avgRequest := stats.ContextEstimatedTokens / int64(stats.ContextSamples)
		avgSchemas := stats.ContextToolSchemaTokens / int64(stats.ContextSamples)
		schemaShare := int64(0)
		if stats.ContextEstimatedTokens > 0 {
			schemaShare = stats.ContextToolSchemaTokens * 100 / stats.ContextEstimatedTokens
		}
		fmt.Fprintf(&sb, "Request context: %d samples, avg total %d tok, avg tool schemas %d tok (%d%%); fingerprints %s, unique prefixes %d\n",
			stats.ContextSamples, avgRequest, avgSchemas, schemaShare,
			formatCountMap(stats.FingerprintStates), len(stats.ProviderPrefixHashes))
	}
	fmt.Fprintf(&sb, "Tools: %d calls, %d failed (%d%%), %d policy redirects; failures by class: %s\n",
		stats.ToolCalls, stats.ToolFailures, toolFailureRate, stats.ToolPolicyRedirects, formatCountMap(stats.ToolFailureClasses))
	if searches := stats.ToolCallsByName["tool_search"]; searches > 0 {
		fmt.Fprintf(&sb, "Deferred tool discovery: %d tool_search call(s), %d%% of tool calls\n", searches, searches*100/max(stats.ToolCalls, 1))
	}
	if triageErr != nil {
		fmt.Fprintf(&sb, "Approvals: %s; triage: unavailable\n", formatCountMap(stats.ApprovalCounts))
	} else {
		fmt.Fprintf(&sb, "Approvals: %s; triage: %s\n", formatCountMap(stats.ApprovalCounts), formatCountMap(triage.Counts))
	}
	if backlogErr != nil {
		sb.WriteString("Approval backlog now: unavailable\n")
	} else {
		oldestParked := "none"
		if backlog.OldestParkedAt != nil {
			oldestParked = formatReportAge(time.Since(*backlog.OldestParkedAt))
		}
		fmt.Fprintf(&sb, "Approval backlog now: live %d, parked %d, oldest parked %s\n", backlog.Live, backlog.Parked, oldestParked)
	}
	continuationShare := int64(0)
	if stats.InputTokens+stats.OutputTokens > 0 {
		continuationShare = (stats.ContinuationInput + stats.ContinuationOutput) * 100 / (stats.InputTokens + stats.OutputTokens)
	}
	fmt.Fprintf(&sb, "Approval continuations: %d runs (%s), %d model calls, input %d, cache read %d, uncached %d, output %d, %d%% of reported tokens\n",
		stats.ContinuationRuns, formatCountMap(stats.ContinuationStatuses), stats.ContinuationCalls,
		stats.ContinuationInput, stats.ContinuationCacheRead, stats.ContinuationCacheMiss, stats.ContinuationOutput, continuationShare)
	if deliveryErr != nil {
		sb.WriteString("Delivery: unavailable\n")
	} else {
		fmt.Fprintf(&sb, "Delivery: %s\n", formatCountMap(deliveryCounts))
	}
	fmt.Fprintf(&sb, "Recall: %d candidates (%s), %d selected (%s), %d output-overlap signals (%s; not causal proof); skipped: %s\n",
		stats.RecallCandidates, formatCountMap(stats.RecallCandidateSources),
		stats.RecallSelected, formatCountMap(stats.RecallSelectedSources),
		stats.RecallOverlap, formatCountMap(stats.RecallOverlapSources), formatCountMap(stats.RecallSkipped))
	fmt.Fprintf(&sb, "Memory disposition: %s\n", formatCountMap(stats.MemoryDisposition))
	if maintenanceErr != nil || healthErr != nil {
		usage := "unavailable"
		if maintenanceErr == nil {
			usage = fmt.Sprintf("%d calls, %d failed, %d circuit-open, input %d, cache read %d, uncached %d, output %d, reasoning %d",
				maintenanceCalls, maintenanceFailed, maintenanceOpen, maintenanceInput, maintenanceCacheRead, maintenanceCacheMiss, maintenanceOutput, maintenanceReasoning)
		}
		healthText := "unavailable"
		if healthErr == nil {
			healthText = fmt.Sprintf("queued %d, retrying %d, running %d, blocked %d; historical terminal: succeeded %d, skipped %d",
				health.Pending, health.Failed, health.Running, health.Blocked, health.Succeeded, health.Skipped)
		}
		fmt.Fprintf(&sb, "Maintenance: %s; current health: %s\n", usage, healthText)
	} else {
		fmt.Fprintf(&sb, "Maintenance: %d calls, %d failed, %d circuit-open, input %d, cache read %d, uncached %d, output %d, reasoning %d; current actionable: queued %d, retrying %d, running %d, blocked %d; historical terminal: succeeded %d, skipped %d\n",
			maintenanceCalls, maintenanceFailed, maintenanceOpen, maintenanceInput, maintenanceCacheRead, maintenanceCacheMiss, maintenanceOutput, maintenanceReasoning,
			health.Pending, health.Failed, health.Running, health.Blocked, health.Succeeded, health.Skipped)
	}
	if healthErr == nil {
		if !health.OldestPendingAt.IsZero() {
			fmt.Fprintf(&sb, "Maintenance oldest actionable: %s\n", formatReportAge(time.Since(health.OldestPendingAt)))
		}
		if !health.LastSuccessAt.IsZero() {
			fmt.Fprintf(&sb, "Maintenance last success: %s\n", health.LastSuccessAt.Local().Format(time.RFC3339))
		}
		if reason := strings.TrimSpace(health.LastError); reason != "" {
			fmt.Fprintf(&sb, "Maintenance last error: %s\n", truncate(toOneLine(reason), 180))
		}
		if health.Blocked > 0 || health.Failed > 0 {
			sb.WriteString("Memory baseline: degraded; write/disposition metrics are incomplete until maintenance recovers.\n")
		}
	}
	if len(evidenceGaps) > 0 {
		fmt.Fprintf(&sb, "Evidence gaps: unavailable: %s. Zero values from those sources were not reported as evidence.\n", strings.Join(evidenceGaps, ", "))
	}
	if truncated {
		fmt.Fprintf(&sb, "Evidence: LOWER BOUND — the window exceeded the %d-event safety cap after cursor pagination.\n", dailyReportEventLimit)
	}
	return strings.TrimSpace(sb.String()), nil
}

func isRecoveryGuardrailCode(code string) bool {
	switch strings.TrimSpace(code) {
	case "recovery_attempt_repeated", "recovery_strategy_exhausted",
		"unknown_effect_requires_observation", "verification_only_mutation_refused":
		return true
	default:
		return false
	}
}

func (d *Server) listDailyReportEvents(ctx context.Context, identity *control.IdentityContext, since time.Time) ([]control.Event, bool, error) {
	var newestFirst []control.Event
	beforeCursor := int64(0)
	truncated := false
	for len(newestFirst) < dailyReportEventLimit {
		pageLimit := min(dailyReportEventPageSize, dailyReportEventLimit-len(newestFirst))
		page, err := d.Control.ListPersonEventsSincePage(ctx, identity.TenantID, identity.PersonID, since, beforeCursor, pageLimit)
		if err != nil {
			return nil, false, err
		}
		if len(page) == 0 {
			break
		}
		newestFirst = append(newestFirst, page...)
		beforeCursor = page[len(page)-1].Cursor
		if len(page) < pageLimit {
			break
		}
	}
	if len(newestFirst) == dailyReportEventLimit {
		more, err := d.Control.ListPersonEventsSincePage(ctx, identity.TenantID, identity.PersonID, since, beforeCursor, 1)
		if err != nil {
			return nil, false, err
		}
		truncated = len(more) > 0
	}
	for left, right := 0, len(newestFirst)-1; left < right; left, right = left+1, right-1 {
		newestFirst[left], newestFirst[right] = newestFirst[right], newestFirst[left]
	}
	return newestFirst, truncated, nil
}

func formatReportAge(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	if age < time.Minute {
		return "<1m"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm", int(age/time.Minute))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
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
