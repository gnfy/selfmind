package kernel

import "testing"

func TestFilterToolDefinitionsAlwaysRemovesHiddenTools(t *testing.T) {
	defs := []map[string]interface{}{
		{"type": "function", "function": map[string]interface{}{"name": "visible"}, "selfmind": map[string]interface{}{"exposure": "direct"}},
		{"type": "function", "function": map[string]interface{}{"name": "lifecycle"}, "selfmind": map[string]interface{}{"exposure": "hidden"}},
	}
	got := filterToolDefinitions(defs, TaskStrategy{})
	if len(got) != 1 || toolDefinitionName(got[0]) != "visible" {
		t.Fatalf("hidden tool leaked into model definitions: %+v", got)
	}
}
