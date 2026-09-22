package tools

import (
	"strings"
	"testing"
)

func TestConvertMCPJSONSchemaPreservesClosedObjectsRecursively(t *testing.T) {
	schema := convertJSONSchema(map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"request": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"name": map[string]interface{}{"type": "string"},
				},
			},
		},
	})
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatalf("root closure was lost: %+v", schema)
	}
	request := schema.Properties["request"]
	if request.AdditionalProperties == nil || *request.AdditionalProperties {
		t.Fatalf("nested closure was lost: %+v", request)
	}
	if err := ValidateArgs(schema, map[string]interface{}{
		"request": map[string]interface{}{"name": "ok", "unexpected": true},
	}); err == nil || !strings.Contains(err.Error(), "request.unexpected") {
		t.Fatalf("nested external argument did not fail closed: %v", err)
	}
}
