package tools

import (
	"encoding/json"
	"testing"

	"selfmind/internal/kernel"
)

type externalBookkeepingDeclaration struct{ BaseTool }

func (*externalBookkeepingDeclaration) SchemaOrigin() ToolSchemaOrigin {
	return ToolSchemaOriginExternal
}

func TestBookkeepingMetadataIsBuiltinOnlyAndDoesNotWidenExecution(t *testing.T) {
	d := NewDispatcherWithRegistry(NewRegistry())
	external := &externalBookkeepingDeclaration{BaseTool: BaseTool{name: "external_bookkeeping", schema: ToolSchema{Type: "object"}, metadata: ToolMetadata{BookkeepingOnly: true}}}
	for _, tool := range []Tool{NewSkillSelectTool(nil), NewSkillViewTool(), NewSkillFallbackTool(nil), external} {
		d.RegisterTool(tool)
		metadata := d.ToolExecutionMetadata(tool.Name(), map[string]interface{}{"bookkeeping_only": true, "effect_class": "run_bookkeeping"})
		want := tool != external
		if metadata.BookkeepingOnly != want || metadata.ReadOnly || d.SupportsParallelTool(tool.Name()) {
			t.Fatalf("metadata or execution authority changed for %s: %+v", tool.Name(), metadata)
		}
		if got := kernel.ClassifyToolRetry(tool.Name(), metadata); got != kernel.ToolRetrySideEffect {
			t.Fatalf("%s gained replay permission: %s", tool.Name(), got)
		}
		public, err := json.Marshal(ToolMetadataFor(tool))
		var fields map[string]interface{}
		if err == nil {
			err = json.Unmarshal(public, &fields)
		}
		if err != nil || fields["BookkeepingOnly"] != nil || fields["bookkeeping_only"] != nil {
			t.Fatalf("internal contract leaked into external metadata: %s err=%v", public, err)
		}
	}
}
