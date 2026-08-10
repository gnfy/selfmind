package llm

import "strings"

// normalizeToolDefinitions returns a detached, JSON-Schema-safe copy of tool
// definitions before protocol-specific compatibility rules are applied.
func normalizeToolDefinitions(tools []ToolDefinition) []ToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		tool.Parameters = normalizeToolParameters(tool.Parameters)
		out = append(out, tool)
	}
	return out
}

func normalizeToolParameters(parameters map[string]interface{}) map[string]interface{} {
	if len(parameters) == 0 {
		return map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		}
	}
	out, ok := normalizeToolSchemaValue(parameters).(map[string]interface{})
	if !ok {
		out = map[string]interface{}{}
	}
	if strings.TrimSpace(stringValue(out["type"])) == "" {
		out["type"] = "object"
	}
	if stringValue(out["type"]) == "object" {
		if _, ok := out["properties"].(map[string]interface{}); !ok {
			out["properties"] = map[string]interface{}{}
		}
	}
	return out
}

func normalizeToolSchemaValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			if key == "required" {
				if required := normalizedRequired(child); len(required) > 0 {
					out[key] = required
				}
				continue
			}
			if key == "properties" && child == nil {
				out[key] = map[string]interface{}{}
				continue
			}
			out[key] = normalizeToolSchemaValue(child)
		}
		if stringValue(out["type"]) == "object" {
			if _, ok := out["properties"].(map[string]interface{}); !ok {
				out["properties"] = map[string]interface{}{}
			}
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(typed))
		for i, child := range typed {
			out[i] = normalizeToolSchemaValue(child)
		}
		return out
	default:
		return value
	}
}

func normalizedRequired(value interface{}) []interface{} {
	var raw []interface{}
	switch typed := value.(type) {
	case []interface{}:
		raw = typed
	case []string:
		raw = make([]interface{}, 0, len(typed))
		for _, item := range typed {
			raw = append(raw, item)
		}
	default:
		return nil
	}
	out := make([]interface{}, 0, len(raw))
	for _, item := range raw {
		name, ok := item.(string)
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}
