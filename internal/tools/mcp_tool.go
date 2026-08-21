package tools

import "fmt"

// MCPTool adapts one remote MCP tool to SelfMind's normal Dispatcher contract.
type MCPTool struct {
	BaseTool
	toolName    string
	inputSchema map[string]interface{}
	client      *MCPClient
}

// RawToolSchema preserves the MCP server's complete inputSchema for the
// provider-neutral compiler. The compiler clones it before repair.
func (t *MCPTool) RawToolSchema() map[string]interface{} {
	return t.inputSchema
}

func (t *MCPTool) SchemaOrigin() ToolSchemaOrigin {
	return ToolSchemaOriginExternal
}

func (c *MCPClient) WrapTool(def MCPToolDef) *MCPTool {
	return c.WrapToolNamed(def, def.Name)
}

func (c *MCPClient) WrapToolNamed(def MCPToolDef, localName string) *MCPTool {
	return &MCPTool{
		BaseTool: BaseTool{
			name:        localName,
			description: def.Description,
			schema:      convertJSONSchema(def.InputSchema),
			metadata: ToolMetadata{
				Category:  "mcp",
				RiskLevel: ToolRiskMedium,
				SearchText: fmt.Sprintf(
					"mcp server %s tool %s %s",
					c.config.Name,
					def.Name,
					def.Description,
				),
			},
		},
		toolName:    def.Name,
		inputSchema: def.InputSchema,
		client:      c,
	}
}

func (t *MCPTool) Execute(args map[string]interface{}) (string, error) {
	return t.client.CallTool(t.toolName, args)
}

func convertJSONSchema(input map[string]interface{}) ToolSchema {
	props := make(map[string]PropertyDef)
	var required []string

	if propsRaw, ok := input["properties"].(map[string]interface{}); ok {
		for name, propRaw := range propsRaw {
			if prop, ok := propRaw.(map[string]interface{}); ok {
				props[name] = convertJSONSchemaProperty(prop)
			}
		}
	}

	if req, ok := input["required"].([]interface{}); ok {
		for _, raw := range req {
			if name, ok := raw.(string); ok {
				required = append(required, name)
			}
		}
	}

	return ToolSchema{Type: "object", Properties: props, Required: required}
}

func convertJSONSchemaProperty(prop map[string]interface{}) PropertyDef {
	p := PropertyDef{}
	if value, ok := prop["type"].(string); ok {
		p.Type = value
	}
	if value, ok := prop["description"].(string); ok {
		p.Description = value
	}
	if value, exists := prop["default"]; exists {
		p.Default = value
	}
	if values, ok := prop["enum"].([]interface{}); ok {
		for _, value := range values {
			p.Enum = append(p.Enum, fmt.Sprintf("%v", value))
		}
	}
	if items, ok := prop["items"].(map[string]interface{}); ok {
		converted := convertJSONSchemaProperty(items)
		p.Items = &converted
	}
	if nested, ok := prop["properties"].(map[string]interface{}); ok {
		p.Properties = make(map[string]PropertyDef, len(nested))
		for name, raw := range nested {
			if child, ok := raw.(map[string]interface{}); ok {
				p.Properties[name] = convertJSONSchemaProperty(child)
			}
		}
	}
	if values, ok := prop["required"].([]interface{}); ok {
		for _, raw := range values {
			if name, ok := raw.(string); ok {
				p.Required = append(p.Required, name)
			}
		}
	}
	return p
}
