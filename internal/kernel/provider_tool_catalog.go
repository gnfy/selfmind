package kernel

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
)

// probeOutputBudget is the output cap for the health check. It is NOT a spend:
// measured against DeepSeek V4 with the real 27-tool catalogue, a budget of 16
// burned all 16 tokens and produced nothing, while 256 finished naturally after
// TWO — a tight cap costs more and fails, because reasoning tokens are output
// tokens and truncation happens before any content appears.
//
// That is what made this expensive to diagnose: a healthy model, already
// serving traffic, failed its own health check about half the time, and the
// failure rolled back the person's model change and parked their queued work.
// The budget exists to keep one check bounded, not to be the thing it measures.
const probeOutputBudget = 256

type ProviderToolCatalogProbe struct {
	Catalog llm.ToolCatalogPreview
	Model   string
	Latency time.Duration
	Err     error
}

// ProbeProviderToolCatalog sends one bounded, non-executing foreground request
// with the daemon's real effective catalogue. It validates provider acceptance
// only: a tool-call-only response is healthy and is never dispatched.
func (a *Agent) ProbeProviderToolCatalog(ctx context.Context) ProviderToolCatalogProbe {
	started := time.Now()
	probe := ProviderToolCatalogProbe{}
	if a == nil || a.Provider() == nil {
		probe.Err = fmt.Errorf("primary provider is not configured")
		probe.Latency = time.Since(started)
		return probe
	}
	if ctx == nil {
		ctx = context.Background()
	}
	definitions := a.llmToolDefinitions(ctx, DefaultTaskStrategy())
	provider := a.activeLLM()
	probe.Catalog = llm.PreviewProviderToolCatalog(ctx, provider, definitions)
	probe.Model = llm.GetModelName(provider)
	if !probe.Catalog.Valid() {
		probe.Err = llm.ToolCatalogError{Preview: probe.Catalog}
		probe.Latency = time.Since(started)
		return probe
	}
	resp, err := provider.Chat(ctx, llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "system", Content: "This is a provider tool-catalog health check. Do not call tools. Reply exactly OK."},
			{Role: "user", Content: "Reply with OK."},
		},
		Tools:     definitions,
		MaxTokens: probeOutputBudget,
	})
	switch {
	case err != nil:
		probe.Err = err
	case resp == nil:
		probe.Err = fmt.Errorf("provider returned no response")
	case strings.TrimSpace(resp.Content) != "" || len(resp.ToolCalls) > 0:
		// Accepted: a tool-call-only answer is healthy and is never dispatched.
	case strings.EqualFold(strings.TrimSpace(resp.FinishReason), "length"):
		// Say WHY it was empty. The old message named only the symptom, so a
		// truncated reasoning model looked identical to a provider that had
		// rejected the catalogue outright — and the one fact that separates
		// them, the finish reason, was the fact being discarded.
		probe.Err = fmt.Errorf("provider produced no content within the %d-token probe budget (finish_reason=length); a reasoning model may need more", probeOutputBudget)
	default:
		probe.Err = fmt.Errorf("provider returned neither content nor a tool call (finish_reason=%s)", fallbackFinishReason(resp.FinishReason))
	}
	probe.Latency = time.Since(started)
	return probe
}

func fallbackFinishReason(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "unset"
}
