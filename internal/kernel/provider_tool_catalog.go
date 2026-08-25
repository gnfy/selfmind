package kernel

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
)

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
		MaxTokens: 16,
	})
	if err != nil {
		probe.Err = err
	} else if resp == nil || (strings.TrimSpace(resp.Content) == "" && len(resp.ToolCalls) == 0) {
		probe.Err = fmt.Errorf("provider returned neither content nor a tool call")
	}
	probe.Latency = time.Since(started)
	return probe
}
