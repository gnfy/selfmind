package kernel

import (
	"context"
	"testing"
)

func TestFilterToolDefinitionsAlwaysRemovesHiddenTools(t *testing.T) {
	defs := []map[string]interface{}{
		{"type": "function", "function": map[string]interface{}{"name": "visible"}, "selfmind": map[string]interface{}{"exposure": "direct"}},
		{"type": "function", "function": map[string]interface{}{"name": "lifecycle"}, "selfmind": map[string]interface{}{"exposure": "hidden"}},
	}
	got := filterToolDefinitions(context.Background(), defs, TaskStrategy{})
	if len(got) != 1 || toolDefinitionName(got[0]) != "visible" {
		t.Fatalf("hidden tool leaked into model definitions: %+v", got)
	}
}

func TestDeferredToolsActivateMonotonicallyAfterSearch(t *testing.T) {
	defs := []map[string]interface{}{
		{"type": "function", "function": map[string]interface{}{"name": "direct"}, "selfmind": map[string]interface{}{"exposure": "direct"}},
		{"type": "function", "function": map[string]interface{}{"name": "cold"}, "selfmind": map[string]interface{}{"exposure": "deferred"}},
		{"type": "function", "function": map[string]interface{}{"name": "hidden"}, "selfmind": map[string]interface{}{"exposure": "hidden"}},
	}
	ctx := withToolActivationState(context.Background())
	if got := filterToolDefinitions(ctx, defs, TaskStrategy{}); len(got) != 1 || toolDefinitionName(got[0]) != "direct" {
		t.Fatalf("initial definitions=%+v", got)
	}
	activated := activateToolsFromSearchResult(ctx, "tool_search", `[{"name":"cold","activated":true},{"name":"direct","activated":false}]`)
	if len(activated) != 1 || activated[0] != "cold" {
		t.Fatalf("activated=%v", activated)
	}
	got := filterToolDefinitions(ctx, defs, TaskStrategy{})
	if len(got) != 2 || toolDefinitionName(got[0]) != "direct" || toolDefinitionName(got[1]) != "cold" {
		t.Fatalf("activated definitions=%+v", got)
	}
	if added := activateToolsFromSearchResult(ctx, "tool_search", `[{"name":"cold","activated":true}]`); len(added) != 0 {
		t.Fatalf("repeat activation changed set: %v", added)
	}
}
