package tools

import "testing"

func TestOptionalNullIsOmittedWithoutWeakeningArgumentContract(t *testing.T) {
	schema := ToolSchema{Type: "object", AdditionalProperties: rejectAdditionalProperties(), Properties: map[string]PropertyDef{"entries": {Type: "array", Items: &PropertyDef{Type: "object", AdditionalProperties: rejectAdditionalProperties(), Properties: map[string]PropertyDef{"status": {Type: "string", Enum: []string{"pending", "completed"}}, "disposition": {Type: "string", Enum: []string{"unfinished"}}}, Required: []string{"status"}}}}, Required: []string{"entries"}}
	args := map[string]interface{}{"entries": []interface{}{map[string]interface{}{"status": "pending", "disposition": nil}}}
	coerced, err := CoerceArgs(schema, args)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateArgs(schema, coerced); err != nil {
		t.Fatalf("optional null was not normalized: %v", err)
	}
	entry := coerced["entries"].([]interface{})[0].(map[string]interface{})
	if _, ok := entry["disposition"]; ok {
		t.Fatal("optional null acquired a value")
	}
	if _, ok := args["entries"].([]interface{})[0].(map[string]interface{})["disposition"]; !ok {
		t.Fatal("normalization mutated input")
	}
	for _, bad := range []map[string]interface{}{
		{"entries": nil},
		{"entries": []interface{}{map[string]interface{}{"status": nil}}},
		{"entries": []interface{}{map[string]interface{}{"status": "pending", "typo": nil}}},
		{"entries": []interface{}{nil}},
	} {
		if c, err := CoerceArgs(schema, bad); err == nil && ValidateArgs(schema, c) == nil {
			t.Fatalf("invalid call accepted: %+v", bad)
		}
	}
	open := ToolSchema{Type: "object", Properties: map[string]PropertyDef{"metadata": {Type: "object"}}}
	c, err := CoerceArgs(open, map[string]interface{}{"extension": nil, "metadata": map[string]interface{}{"raw": nil}})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := c["extension"]; !ok || v != nil {
		t.Fatal("open extension null was lost")
	}
	if v, ok := c["metadata"].(map[string]interface{})["raw"]; !ok || v != nil {
		t.Fatal("raw nested metadata changed")
	}
}
