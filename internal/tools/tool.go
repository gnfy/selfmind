package tools

import (
	"encoding/json"
	"fmt"
)

// Tool 是所有工具的统一接口
type Tool interface {
	Name() string
	Description() string
	Execute(args map[string]interface{}) (string, error)
	Schema() ToolSchema
}

// ToolSchema 定义工具的参数 schema（兼容 OpenAI tool schema）
type ToolSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]PropertyDef `json:"properties,omitempty"`
	Required   []string               `json:"required,omitempty"`
}

type PropertyDef struct {
	Type        string      `json:"type"`
	Description string      `json:"description,omitempty"`
	Default     interface{} `json:"default,omitempty"`
	Enum        []string    `json:"enum,omitempty"`
}

// BaseTool 提供工具的默认实现基类
type BaseTool struct {
	name        string
	description string
	schema      ToolSchema
	handler     func(args map[string]interface{}) (string, error)
}

func (b *BaseTool) Name() string         { return b.name }
func (b *BaseTool) Description() string  { return b.description }
func (b *BaseTool) Schema() ToolSchema   { return b.schema }

func (b *BaseTool) Execute(args map[string]interface{}) (string, error) {
	if b.handler == nil {
		return "", fmt.Errorf("no handler registered for tool %s", b.name)
	}
	return b.handler(args)
}

// toToolDefinition converts a Tool to LLM tool definition format
func ToToolDefinition(t Tool) map[string]interface{} {
	props := make(map[string]interface{})
	for k, v := range t.Schema().Properties {
		props[k] = map[string]interface{}{
			"type":        v.Type,
			"description": v.Description,
		}
	}
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        t.Name(),
			"description": t.Description(),
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": props,
				"required":   t.Schema().Required,
			},
		},
	}
}

// CoerceArgs 将 string/bool/int 等动态类型强制转换为 schema 声明的类型
func CoerceArgs(schema ToolSchema, args map[string]interface{}) (map[string]interface{}, error) {
	coerced := make(map[string]interface{})
	for param, def := range schema.Properties {
		val, exists := args[param]
		if !exists {
			continue
		}
		coerced[param] = coerceValue(val, def.Type)
	}
	return coerced, nil
}

func coerceValue(val interface{}, targetType string) interface{} {
	switch targetType {
	case "integer":
		switch v := val.(type) {
		case float64:
			return int(v)
		case string:
			var i int
			fmt.Sscanf(v, "%d", &i)
			return i
		default:
			return v
		}
	case "number":
		switch v := val.(type) {
		case int:
			return float64(v)
		case string:
			var f float64
			fmt.Sscanf(v, "%f", &f)
			return f
		default:
			return v
		}
	case "boolean":
		switch v := val.(type) {
		case string:
			return v == "true" || v == "1"
		default:
			return v
		}
	case "string":
		return fmt.Sprintf("%v", val)
	default:
		return val
	}
}

// MarshalArgs 将 args 序列化为 JSON 字符串（用于日志/调试）
func MarshalArgs(args map[string]interface{}) string {
	b, _ := json.Marshal(args)
	return string(b)
}
