package llm

import (
	"reflect"
	"testing"
)

func TestNormalizeToolParametersRemovesInvalidRequiredRecursively(t *testing.T) {
	var required []string
	original := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"options": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string"},
				},
				"required": required,
			},
		},
		"required": required,
	}

	normalized := normalizeToolParameters(original)
	if _, exists := normalized["required"]; exists {
		t.Fatalf("root required was not removed: %#v", normalized)
	}
	options := normalized["properties"].(map[string]interface{})["options"].(map[string]interface{})
	if _, exists := options["required"]; exists {
		t.Fatalf("nested required was not removed: %#v", options)
	}
	if _, exists := original["required"]; !exists {
		t.Fatal("normalization mutated the original schema")
	}
}

func TestNormalizeToolParametersPreservesValidRequired(t *testing.T) {
	original := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"goal": map[string]interface{}{"type": "string"},
		},
		"required": []string{"goal"},
	}
	normalized := normalizeToolParameters(original)
	if got := normalized["required"]; !reflect.DeepEqual(got, []interface{}{"goal"}) {
		t.Fatalf("required = %#v", got)
	}
}
