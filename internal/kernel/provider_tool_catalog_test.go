package kernel

import (
	"context"
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
