package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

type providerCatalogProbeBackend struct {
	dispatches int
}

func (b *providerCatalogProbeBackend) Dispatch(string, map[string]interface{}) (string, error) {
	b.dispatches++
	return "unexpected", nil
}

func (*providerCatalogProbeBackend) GetToolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name":       "read_file",
			"parameters": map[string]interface{}{"type": "object"},
			"selfmind":   map[string]interface{}{"exposure": "direct"},
		},
		{
			"name":       "skill:proto-contract-drift-audit",
			"parameters": map[string]interface{}{"type": "object"},
			"selfmind":   map[string]interface{}{"exposure": "hidden"},
		},
	}
}

type providerCatalogProbeLLM struct {
	request llm.ChatRequest
}

func (*providerCatalogProbeLLM) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "OK", nil
}

func (p *providerCatalogProbeLLM) Chat(_ context.Context, request llm.ChatRequest) (*llm.ChatResponse, error) {
	p.request = request
	return &llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "probe-call", Function: "read_file", Args: `{}`}}}, nil
}

func (*providerCatalogProbeLLM) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	events := make(chan llm.StreamEvent)
	close(events)
	return events, nil
}

func (*providerCatalogProbeLLM) GetModel() string { return "probe-model" }

func TestProviderToolCatalogProbeUsesFinalVisibleCatalogWithoutDispatch(t *testing.T) {
	backend := &providerCatalogProbeBackend{}
	provider := &providerCatalogProbeLLM{}
	agent := NewAgent(nil, backend, provider, "test", 1, 1, nil)

	preview := agent.ProviderToolCatalogPreview(context.Background())
	if !preview.Valid() || preview.Count != 1 || len(preview.Names) != 1 || preview.Names[0] != "read_file" {
		t.Fatalf("final provider catalogue = %+v", preview)
	}

	probe := agent.ProbeProviderToolCatalog(context.Background())
	if probe.Err != nil {
		t.Fatalf("probe error = %v", probe.Err)
	}
	if probe.Model != "probe-model" || len(provider.request.Tools) != 1 || provider.request.Tools[0].Name != "read_file" {
		t.Fatalf("probe model=%q tools=%+v", probe.Model, provider.request.Tools)
	}
	if backend.dispatches != 0 {
		t.Fatalf("probe dispatched %d model-returned tool calls", backend.dispatches)
	}
}

// scriptedProbeLLM answers the health check with whatever the test wants,
// including the shape a reasoning model produces when the output cap cuts it
// off before it has said anything.
type scriptedProbeLLM struct {
	response llm.ChatResponse
	request  llm.ChatRequest
}

func (*scriptedProbeLLM) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "OK", nil
}

func (p *scriptedProbeLLM) Chat(_ context.Context, request llm.ChatRequest) (*llm.ChatResponse, error) {
	p.request = request
	reply := p.response
	return &reply, nil
}

func (*scriptedProbeLLM) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	events := make(chan llm.StreamEvent)
	close(events)
	return events, nil
}

func (*scriptedProbeLLM) GetModel() string { return "probe-model" }

// TestProviderToolCatalogProbeBudgetFitsAReasoningModel pins the output cap.
// Reasoning tokens ARE output tokens, so a tight cap truncates the answer before
// any content exists. Measured against DeepSeek V4 with the real catalogue: a
// budget of 16 burned all 16 tokens and returned nothing on every attempt, while
// 256 finished naturally after two — the tight cap both failed AND cost more.
// The failure rolled back the person's model change and parked their queued
// work, so a healthy model kept losing its own health check.
func TestProviderToolCatalogProbeBudgetFitsAReasoningModel(t *testing.T) {
	provider := &scriptedProbeLLM{response: llm.ChatResponse{Content: "OK", FinishReason: "stop"}}
	agent := NewAgent(nil, &providerCatalogProbeBackend{}, provider, "test", 1, 1, nil)

	if probe := agent.ProbeProviderToolCatalog(context.Background()); probe.Err != nil {
		t.Fatalf("probe error = %v", probe.Err)
	}
	if provider.request.MaxTokens < 64 {
		t.Fatalf("probe output budget = %d; too tight for a reasoning model to reach any content",
			provider.request.MaxTokens)
	}
}

// TestProviderToolCatalogProbeNamesTruncation: an empty answer that was CUT OFF
// is a different diagnosis from a provider that rejected the catalogue, and the
// finish reason is the only thing separating them. The old message reported the
// symptom and discarded that fact, which is what made this hard to trace from a
// log alone.
func TestProviderToolCatalogProbeNamesTruncation(t *testing.T) {
	provider := &scriptedProbeLLM{response: llm.ChatResponse{FinishReason: "length"}}
	agent := NewAgent(nil, &providerCatalogProbeBackend{}, provider, "test", 1, 1, nil)

	probe := agent.ProbeProviderToolCatalog(context.Background())
	if probe.Err == nil {
		t.Fatal("a truncated empty answer must still fail the probe")
	}
	message := probe.Err.Error()
	for _, want := range []string{"finish_reason=length", "budget"} {
		if !strings.Contains(message, want) {
			t.Fatalf("probe error must name %q so the cause is visible in a log: %q", want, message)
		}
	}

	// An empty answer for any OTHER reason keeps the original diagnosis, and
	// still says which reason, rather than leaving the reader to guess.
	provider.response = llm.ChatResponse{FinishReason: "content_filter"}
	probe = agent.ProbeProviderToolCatalog(context.Background())
	if probe.Err == nil || !strings.Contains(probe.Err.Error(), "content_filter") {
		t.Fatalf("non-truncation failure should name its finish reason: %v", probe.Err)
	}

	// A missing finish reason must not render as an empty gap.
	provider.response = llm.ChatResponse{}
	probe = agent.ProbeProviderToolCatalog(context.Background())
	if probe.Err == nil || !strings.Contains(probe.Err.Error(), "unset") {
		t.Fatalf("absent finish reason should read as unset: %v", probe.Err)
	}
}

// A tool-call-only answer stays healthy: that was already true and must not
// change while fixing the empty-answer path.
func TestProviderToolCatalogProbeAcceptsToolCallOnlyAnswer(t *testing.T) {
	provider := &scriptedProbeLLM{response: llm.ChatResponse{
		ToolCalls:    []llm.ToolCall{{ID: "c1", Function: "read_file", Args: `{}`}},
		FinishReason: "tool_calls",
	}}
	agent := NewAgent(nil, &providerCatalogProbeBackend{}, provider, "test", 1, 1, nil)
	if probe := agent.ProbeProviderToolCatalog(context.Background()); probe.Err != nil {
		t.Fatalf("a tool-call-only answer is healthy: %v", probe.Err)
	}
}

// TestProbeIsSafeBesideARunningTurn pins the concurrency boundary between
// diagnostics and real work. The probe reads the per-run provider; a turn
// assigns and clears it. That field was read with no lock at all, so running
// `selfmind doctor` while a turn was in flight was a genuine data race — the
// race detector reports it, and the two accesses are on different goroutines by
// design, not by accident.
func TestProbeIsSafeBesideARunningTurn(t *testing.T) {
	provider := &scriptedProbeLLM{response: llm.ChatResponse{Content: "OK", FinishReason: "stop"}}
	agent := NewAgent(nil, &providerCatalogProbeBackend{}, provider, "test", 1, 1, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			agent.setRunLLM(provider)
			agent.setRunLLM(nil)
		}
	}()
	for i := 0; i < 200; i++ {
		if probe := agent.ProbeProviderToolCatalog(context.Background()); probe.Err != nil {
			t.Fatalf("probe failed beside a running turn: %v", probe.Err)
		}
	}
	<-done
}
