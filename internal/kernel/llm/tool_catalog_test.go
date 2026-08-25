package llm

import (
	"context"
	"strings"
	"testing"
)

func TestOpenAIProviderToolCatalogRejectsInvalidNameBeforeNetwork(t *testing.T) {
	adapter := &OpenAIAdapter{BaseURL: "http://127.0.0.1:1", Quirks: ProviderQuirks{SupportsTools: true}}
	tools := []ToolDefinition{
		{Name: "read_file", Parameters: map[string]interface{}{"type": "object"}},
		{Name: "skill:proto-contract", Parameters: map[string]interface{}{"type": "object"}},
	}
	preview := PreviewProviderToolCatalog(context.Background(), adapter, tools)
	if preview.Valid() || len(preview.Issues) != 1 || preview.Issues[0].Index != 1 || preview.Issues[0].Code != "invalid_name" {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	if _, err := adapter.Chat(context.Background(), ChatRequest{Tools: tools}); err == nil || !strings.Contains(err.Error(), `tools[1] name "skill:proto-contract"`) {
		t.Fatalf("invalid catalog reached transport or returned a weak error: %v", err)
	}
}

func TestResponsesProviderCatalogReportsAliasedWireNames(t *testing.T) {
	adapter := &ResponsesAdapter{}
	preview := PreviewProviderToolCatalog(context.Background(), adapter, []ToolDefinition{{
		Name: "skill:proto-contract", Parameters: map[string]interface{}{"type": "object"},
	}})
	if !preview.Valid() || len(preview.Names) != 1 || strings.Contains(preview.Names[0], ":") {
		t.Fatalf("Responses preview did not reflect its actual aliasing: %+v", preview)
	}
	if len(preview.Entries) != 1 || preview.Entries[0].SourceName != "skill:proto-contract" || preview.Entries[0].WireName != preview.Names[0] {
		t.Fatalf("Responses preview lost source-to-wire provenance: %+v", preview)
	}
}

func TestProviderToolCatalogDetectsPostWireDuplicates(t *testing.T) {
	adapter := &OpenAIAdapter{}
	preview := PreviewProviderToolCatalog(context.Background(), adapter, []ToolDefinition{{Name: "same"}, {Name: "same"}})
	if preview.Valid() || preview.Issues[0].Code != "duplicate_name" {
		t.Fatalf("duplicate wire names were not rejected: %+v", preview)
	}
}
