package tools

import (
	"encoding/json"
	"testing"
)

func TestToolSearchExactIdentifierWinsDespiteUnmatchedExtraTerms(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&BaseTool{
		name: "skill_view", description: "Read one skill resource page.",
		metadata: ToolMetadata{Exposure: ToolExposureDeferred, Category: "skill", RiskLevel: ToolRiskLow, ReadOnly: true},
	})
	registry.Register(&BaseTool{
		name: "generic_reader", description: "Load paged skill instructions and inspect details.",
		metadata: ToolMetadata{Exposure: ToolExposureDeferred, Category: "skill", RiskLevel: ToolRiskLow, ReadOnly: true},
	})

	raw, err := NewToolSearchTool().Execute(map[string]interface{}{
		"_registry": registry,
		"query":     "skill_view load paged skill instructions",
	})
	if err != nil {
		t.Fatal(err)
	}
	var results []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].Name != "skill_view" {
		t.Fatalf("results = %s", raw)
	}
}
