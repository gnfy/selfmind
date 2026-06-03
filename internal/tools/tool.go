package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
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

func (b *BaseTool) Name() string        { return b.name }
func (b *BaseTool) Description() string { return b.description }
func (b *BaseTool) Schema() ToolSchema  { return b.schema }

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

// ValidateArgs checks that all required fields are present and types match.
// Returns an error describing the first validation failure.
func ValidateArgs(schema ToolSchema, args map[string]interface{}) error {
	if schema.Properties == nil {
		return nil
	}

	// Check required fields
	for _, required := range schema.Required {
		if _, ok := args[required]; !ok {
			return fmt.Errorf("missing required parameter: %s", required)
		}
	}

	// Type-check present arguments
	for param, val := range args {
		def, ok := schema.Properties[param]
		if !ok {
			// Unknown parameter — skip, don't error (forward-compat)
			continue
		}
		if err := validateType(param, val, def.Type); err != nil {
			return err
		}
	}

	return nil
}

func validateType(param string, val interface{}, expectedType string) error {
	switch expectedType {
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("parameter %s must be a string, got %T", param, val)
		}
	case "integer":
		switch val.(type) {
		case int, int8, int16, int32, int64, float64:
			// numeric coercion OK
		default:
			return fmt.Errorf("parameter %s must be an integer, got %T", param, val)
		}
	case "number":
		switch val.(type) {
		case int, int8, int16, int32, int64, float64:
			// numeric OK
		default:
			return fmt.Errorf("parameter %s must be a number, got %T", param, val)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("parameter %s must be a boolean, got %T", param, val)
		}
	case "array":
		if _, ok := val.([]interface{}); !ok {
			return fmt.Errorf("parameter %s must be an array, got %T", param, val)
		}
	case "object":
		if _, ok := val.(map[string]interface{}); !ok {
			return fmt.Errorf("parameter %s must be an object, got %T", param, val)
		}
	}
	return nil
}

// CoerceArgs 将 string/bool/int 等动态类型强制转换为 schema 声明的类型
func CoerceArgs(schema ToolSchema, args map[string]interface{}) (map[string]interface{}, error) {
	coerced := make(map[string]interface{})
	for param, val := range args {
		if strings.HasPrefix(param, "_") {
			coerced[param] = val
		}
	}
	for param, def := range schema.Properties {
		val, exists := args[param]
		if !exists {
			continue
		}
		coercedValue, err := coerceValue(param, val, def.Type)
		if err != nil {
			return nil, err
		}
		coerced[param] = coercedValue
	}
	return coerced, nil
}

func coerceValue(param string, val interface{}, targetType string) (interface{}, error) {
	switch targetType {
	case "integer":
		switch v := val.(type) {
		case float64:
			if math.Trunc(v) != v {
				return nil, fmt.Errorf("parameter %s must be an integer, got %v", param, v)
			}
			return int(v), nil
		case string:
			i, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("parameter %s must be an integer: %w", param, err)
			}
			return i, nil
		default:
			return v, nil
		}
	case "number":
		switch v := val.(type) {
		case int:
			return float64(v), nil
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return nil, fmt.Errorf("parameter %s must be a number: %w", param, err)
			}
			return f, nil
		default:
			return v, nil
		}
	case "boolean":
		switch v := val.(type) {
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("parameter %s must be a boolean: %w", param, err)
			}
			return b, nil
		default:
			return v, nil
		}
	case "string":
		return fmt.Sprintf("%v", val), nil
	default:
		return val, nil
	}
}

// MarshalArgs 将 args 序列化为 JSON 字符串（用于日志/调试）
func MarshalArgs(args map[string]interface{}) string {
	b, _ := json.Marshal(args)
	return string(b)
}
