package tools

import (
	"sync"
	"testing"
)

// One tool instance can be registered in every agent worker's dispatcher (the
// shared MCP adapters and Skill tools are), and each worker reads its metadata
// while building a catalogue under only a registry read lock. Reading must
// therefore never write: the defaults belong in the returned copy.
func TestSharedToolMetadataReadsNeverWriteTheTool(t *testing.T) {
	for _, tc := range []struct {
		name         string
		declared     ToolMetadata
		wantExposure ToolExposure
		wantRisk     ToolRiskLevel
	}{
		{"undeclared metadata takes the defaults", ToolMetadata{}, ToolExposureDirect, ToolRiskMedium},
		{"declared metadata is kept", ToolMetadata{Exposure: ToolExposureHidden, RiskLevel: ToolRiskLow, Category: "skill"}, ToolExposureHidden, ToolRiskLow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shared := &BaseTool{
				name: "shared_metadata_probe", description: "metadata probe",
				schema: ToolSchema{Type: "object"}, metadata: tc.declared,
			}
			primary, worker := NewRegistry(), NewRegistry()
			primary.Register(shared)
			worker.Register(shared)

			read := make([]ToolMetadata, 2)
			var wg sync.WaitGroup
			for index, registry := range []*Registry{primary, worker} {
				wg.Add(1)
				go func(index int, registry *Registry) {
					defer wg.Done()
					_ = registry.ToolDefinitions()
					tool, _ := registry.Get(shared.Name())
					read[index] = tool.(MetadataProvider).Metadata()
				}(index, registry)
			}
			wg.Wait()

			for index, metadata := range read {
				if metadata.Exposure != tc.wantExposure || metadata.RiskLevel != tc.wantRisk {
					t.Fatalf("reader %d metadata = %+v, want exposure %q and risk %q", index, metadata, tc.wantExposure, tc.wantRisk)
				}
			}
			if shared.metadata.Exposure != tc.declared.Exposure || shared.metadata.RiskLevel != tc.declared.RiskLevel {
				t.Fatalf("reading metadata rewrote the shared tool: declared %+v, now %+v", tc.declared, shared.metadata)
			}
		})
	}
}
