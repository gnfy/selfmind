package tools

import (
	"strings"
	"testing"
)

type rawSchemaTestTool struct {
	BaseTool
	raw    map[string]interface{}
	origin ToolSchemaOrigin
}

func (t *rawSchemaTestTool) RawToolSchema() map[string]interface{} { return t.raw }
func (t *rawSchemaTestTool) SchemaOrigin() ToolSchemaOrigin        { return t.origin }

func newRawSchemaTestTool(name string, raw map[string]interface{}, origin ToolSchemaOrigin) *rawSchemaTestTool {
	return &rawSchemaTestTool{
		BaseTool: BaseTool{
			name: name, description: "schema test tool", schema: ToolSchema{Type: "object"},
			handler: func(map[string]interface{}) (string, error) { return "ok", nil },
		},
		raw: raw, origin: origin,
	}
}

func TestCompileToolSchemaRepairsOnlyDeterministicShapeIssues(t *testing.T) {
	tool := newRawSchemaTestTool("repairable", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{"type": "string", "default": 3},
			"tags":  map[string]interface{}{"type": "array"},
		},
		"required": nil,
	}, ToolSchemaOriginExternal)

	compiled := compileToolSchema(tool)
	if compiled.Report.Status != ToolSchemaRepaired {
		t.Fatalf("status=%s issues=%+v", compiled.Report.Status, compiled.Report.Issues)
	}
	if _, exists := compiled.Parameters["required"]; exists {
		t.Fatal("null required should be omitted")
	}
	query := compiled.Parameters["properties"].(map[string]interface{})["query"].(map[string]interface{})
	if _, exists := query["default"]; exists {
		t.Fatal("type-mismatched default should be removed")
	}
	tags := compiled.Parameters["properties"].(map[string]interface{})["tags"].(map[string]interface{})
	if _, ok := tags["items"].(map[string]interface{}); !ok {
		t.Fatalf("array items not repaired: %#v", tags["items"])
	}
}

func TestCompileToolSchemaQuarantinesAmbiguousRequiredReference(t *testing.T) {
	tool := newRawSchemaTestTool("broken", map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"known": map[string]interface{}{"type": "string"}},
		"required":   []interface{}{"missing"},
	}, ToolSchemaOriginExternal)

	compiled := compileToolSchema(tool)
	if compiled.Report.Status != ToolSchemaQuarantined {
		t.Fatalf("status=%s issues=%+v", compiled.Report.Status, compiled.Report.Issues)
	}
	if len(compiled.Report.Issues) == 0 || compiled.Report.Issues[0].Code != "unknown_required_property" {
		t.Fatalf("issues=%+v", compiled.Report.Issues)
	}
}

func TestCompileExternalToolSchemaRejectsReservedRuntimeParameter(t *testing.T) {
	tool := newRawSchemaTestTool("reserved", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"_tenant_id": map[string]interface{}{"type": "string"},
		},
	}, ToolSchemaOriginExternal)

	compiled := compileToolSchema(tool)
	if compiled.Report.Status != ToolSchemaQuarantined {
		t.Fatalf("status=%s issues=%+v", compiled.Report.Status, compiled.Report.Issues)
	}
	if len(compiled.Report.Issues) == 0 || compiled.Report.Issues[len(compiled.Report.Issues)-1].Code != "reserved_parameter_name" {
		t.Fatalf("issues=%+v", compiled.Report.Issues)
	}
}

func TestCompileToolSchemaRejectsKeywordsThatContradictType(t *testing.T) {
	tool := newRawSchemaTestTool("contradictory", map[string]interface{}{
		"type":       "string",
		"properties": map[string]interface{}{},
	}, ToolSchemaOriginExternal)
	compiled := compileToolSchema(tool)
	if compiled.Report.Status != ToolSchemaQuarantined {
		t.Fatalf("status=%s issues=%+v", compiled.Report.Status, compiled.Report.Issues)
	}
	found := false
	for _, issue := range compiled.Report.Issues {
		if issue.Code == "object_keyword_type_mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("issues=%+v", compiled.Report.Issues)
	}
}

func TestRegistryQuarantinesOneExternalToolWithoutBreakingCatalog(t *testing.T) {
	registry := NewRegistry()
	registry.Register(newRawSchemaTestTool("healthy", map[string]interface{}{
		"type": "object", "properties": map[string]interface{}{},
	}, ToolSchemaOriginExternal))
	registry.Register(newRawSchemaTestTool("broken", map[string]interface{}{
		"type": "object", "properties": "not-an-object",
	}, ToolSchemaOriginExternal))

	defs := registry.ToolDefinitions()
	if len(defs) != 1 || defs[0]["function"].(map[string]interface{})["name"] != "healthy" {
		t.Fatalf("definitions=%#v", defs)
	}
	if _, err := registry.Dispatch("broken", nil); err == nil || !strings.Contains(err.Error(), "schema quarantined") {
		t.Fatalf("dispatch error=%v", err)
	}
	if err := registry.ValidateInternalToolSchemas(); err != nil {
		t.Fatalf("external quarantine must not fail startup: %v", err)
	}
}

func TestMCPToolPreservesRawNestedSchemaForProviders(t *testing.T) {
	client := &MCPClient{config: MCPServerConfig{Name: "nested"}}
	rawSchema := map[string]interface{}{
		"type":     "object",
		"required": nil,
		"properties": map[string]interface{}{
			"filters": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"value": map[string]interface{}{
							"oneOf": []interface{}{
								map[string]interface{}{"type": "string"},
								map[string]interface{}{"type": "number"},
							},
						},
					},
					"required": []interface{}{"value"},
				},
			},
		},
	}
	tool := client.WrapToolNamed(MCPToolDef{
		Name:        "search",
		InputSchema: rawSchema,
	}, "mcp_nested_search")

	registry := NewRegistry()
	registry.Register(tool)
	defs := registry.ToolDefinitions()
	if len(defs) != 1 {
		t.Fatalf("definitions=%#v reports=%+v", defs, registry.ToolSchemaReport())
	}
	parameters := defs[0]["function"].(map[string]interface{})["parameters"].(map[string]interface{})
	filters := parameters["properties"].(map[string]interface{})["filters"].(map[string]interface{})
	items := filters["items"].(map[string]interface{})
	value := items["properties"].(map[string]interface{})["value"].(map[string]interface{})
	if branches, ok := value["oneOf"].([]interface{}); !ok || len(branches) != 2 {
		t.Fatalf("nested oneOf was lost: %#v", value)
	}
	if _, exists := rawSchema["required"]; !exists {
		t.Fatalf("compiler mutated raw MCP schema: %#v", rawSchema)
	}
}

func TestBuiltinCatalogCompilesWithoutRepairOrQuarantine(t *testing.T) {
	dispatcher := NewDispatcher()
	RegisterBuiltins(dispatcher)
	RegisterExtendedTools(dispatcher, WebSearchOptions{})
	if err := dispatcher.ValidateInternalToolSchemas(); err != nil {
		t.Fatal(err)
	}
	for _, report := range dispatcher.ToolSchemaReport() {
		if report.Status != ToolSchemaActive {
			t.Errorf("built-in %s status=%s issues=%+v", report.Name, report.Status, report.Issues)
		}
	}
}

func TestMCPToolLocalNameIsProviderSafeAndCollisionResistant(t *testing.T) {
	a := MCPToolLocalName(strings.Repeat("server", 20), strings.Repeat("tool", 30)+"a")
	b := MCPToolLocalName(strings.Repeat("server", 20), strings.Repeat("tool", 30)+"b")
	c := MCPToolLocalName("foo-bar", "search")
	d := MCPToolLocalName("foo_bar", "search")
	if len(a) > 64 || len(b) > 64 {
		t.Fatalf("names exceed provider limit: %d %d", len(a), len(b))
	}
	if a == b {
		t.Fatalf("truncated names collided: %q", a)
	}
	if c == d {
		t.Fatalf("sanitized names collided: %q", c)
	}
	for _, name := range []string{a, b, c, d} {
		for _, r := range name {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
				t.Fatalf("unsafe provider tool name %q", name)
			}
		}
	}
}
