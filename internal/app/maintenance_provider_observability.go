package app

import (
	"context"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/log"
)

func maintenanceBatchSize(req llm.ChatRequest) int {
	if req.Options == nil {
		return 1
	}
	value, ok := req.Options["maintenance_batch_size"]
	if !ok {
		return 1
	}
	var size int
	switch typed := value.(type) {
	case int:
		size = typed
	case int64:
		size = int(typed)
	case float64:
		size = int(typed)
	}
	if size < 1 {
		return 1
	}
	return size
}

func providerErrorClass(err error) string {
	if info, ok := llm.ProviderErrorInfo(err); ok {
		return string(info.Class)
	}
	if err != nil {
		return string(llm.ProviderErrorUnknown)
	}
	return ""
}

func providerErrorUsage(err error) llm.UsageStats {
	if info, ok := llm.ProviderErrorInfo(err); ok {
		return info.Usage
	}
	return llm.UsageStats{}
}

func providerErrorUsageOr(err error, fallback llm.UsageStats) llm.UsageStats {
	usage := providerErrorUsage(err)
	if usage.InputTokens == 0 && usage.OutputTokens == 0 &&
		usage.CacheReadInputTokens == 0 && usage.CacheMissInputTokens == 0 &&
		usage.CacheCreationInputTokens == 0 && usage.ReasoningOutputTokens == 0 {
		return fallback
	}
	return usage
}

func providerErrorFinishReason(err error) string {
	if info, ok := llm.ProviderErrorInfo(err); ok {
		return strings.TrimSpace(info.StopReason)
	}
	return ""
}

func providerErrorFinishReasonOr(err error, fallback string) string {
	if reason := providerErrorFinishReason(err); reason != "" {
		return reason
	}
	return strings.TrimSpace(fallback)
}

func isMaintenanceOutputExhausted(err error) bool {
	info, ok := llm.ProviderErrorInfo(err)
	if !ok || info.Class != llm.ProviderErrorEmptyResponse {
		return false
	}
	reason := strings.ToLower(strings.TrimSpace(info.StopReason))
	switch reason {
	case "max_tokens", "max_output_tokens", "length", "token_limit":
		return true
	default:
		return strings.Contains(reason, "max_token") || strings.Contains(reason, "length")
	}
}

func (c *maintenanceProviderChain) recordProviderCall(ctx context.Context, candidate namedMaintenanceProvider,
	index int, status, triggerClass string, callErr error, usage llm.UsageStats,
	finishReason string, batchSize int, started time.Time) {
	if c == nil || c.control == nil {
		return
	}
	latency := int64(0)
	if !started.IsZero() {
		latency = time.Since(started).Milliseconds()
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	err := c.control.RecordMaintenanceProviderCall(writeCtx, control.MaintenanceProviderCall{
		TenantID:                 c.routeTenant(ctx),
		Role:                     string(candidate.role),
		Provider:                 candidate.route.Provider,
		Model:                    candidate.route.Model,
		RouteID:                  candidate.route.ID,
		CandidateIndex:           index,
		Status:                   status,
		TriggerClass:             triggerClass,
		FinishReason:             strings.TrimSpace(finishReason),
		ErrorClass:               providerErrorClass(callErr),
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
		CacheMissInputTokens:     usage.CacheMissInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		ReasoningOutputTokens:    usage.ReasoningOutputTokens,
		CacheUsageReported:       usage.CacheUsageReported,
		BatchSize:                batchSize,
		LatencyMS:                latency,
		CreatedAt:                time.Now(),
	})
	if err != nil {
		log.Warn("maintenance provider usage record failed", "provider", candidate.route.Provider, "error", err)
	}
}
