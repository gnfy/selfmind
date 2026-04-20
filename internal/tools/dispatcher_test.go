package tools

import (
	"encoding/json"
	"fmt"
	"testing"
)

// MockTool 模拟一个真实工具
type MockTool struct {
	BaseTool
}

func NewMockTool() *MockTool {
	return &MockTool{
		BaseTool: BaseTool{
			name:        "test_tool",
			description: "Mock tool for testing",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"age":  {Type: "integer", Description: "Age in years"},
					"rate": {Type: "number", Description: "Rate per unit"},
					"on":   {Type: "boolean", Description: "Toggle on/off"},
				},
				Required: []string{"age"},
			},
			handler: func(args map[string]interface{}) (string, error) {
				return fmt.Sprintf("executed with %v", args), nil
			},
		},
	}
}

func TestCoerceArgs(t *testing.T) {
	tool := NewMockTool()
	Register(&tool.BaseTool) // register to global for this test

	args := map[string]interface{}{
		"age":  "25",
		"rate": "10.5",
		"on":   "true",
	}

	coerced, err := CoerceArgs(tool.Schema(), args)
	if err != nil {
		t.Fatalf("CoerceArgs failed: %v", err)
	}

	if _, ok := coerced["age"].(int); !ok {
		t.Errorf("age should be int, got %T", coerced["age"])
	}
	if _, ok := coerced["rate"].(float64); !ok {
		t.Errorf("rate should be float64, got %T", coerced["rate"])
	}
	if _, ok := coerced["on"].(bool); !ok {
		t.Errorf("on should be bool, got %T", coerced["on"])
	}
}

func TestDispatcherRegister(t *testing.T) {
	// 每个测试用独立的 registry 避免污染
	reg := NewRegistry()
	disp := &Dispatcher{registry: reg}

	tool := NewMockTool()
	reg.Register(tool)

	if !disp.ToolExists("test_tool") {
		t.Error("test_tool should be registered")
	}

	result, err := disp.Dispatch("test_tool", map[string]interface{}{"age": 30})
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestDispatcherDispatchRaw(t *testing.T) {
	reg := NewRegistry()
	disp := &Dispatcher{registry: reg}

	tool := NewMockTool()
	reg.Register(tool)

	result, err := disp.DispatchRaw("test_tool", `{"age":"25","rate":"10.5","on":"true"}`)
	if err != nil {
		t.Fatalf("DispatchRaw failed: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestToolDefinitions(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewListFilesTool())

	defs := reg.ToolDefinitions()
	if len(defs) == 0 {
		t.Fatal("expected at least one tool definition")
	}
	if defs[0]["type"] != "function" {
		t.Errorf("expected type=function, got %v", defs[0]["type"])
	}
}

func TestMiddlewareChain(t *testing.T) {
	reg := NewRegistry()
	var logLines []string

	reg.UseMiddleware(func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			logLines = append(logLines, "middleware1:before")
			result, err := next(args)
			logLines = append(logLines, "middleware1:after")
			return result, err
		}
	})
	reg.UseMiddleware(func(next ToolExecutor) ToolExecutor {
		return func(args map[string]interface{}) (string, error) {
			logLines = append(logLines, "middleware2:before")
			result, err := next(args)
			logLines = append(logLines, "middleware2:after")
			return result, err
		}
	})

	tool := &BaseTool{
		name:        "mw_test",
		description: "test",
		schema:      ToolSchema{Type: "object"},
		handler: func(args map[string]interface{}) (string, error) {
			logLines = append(logLines, "handler")
			return "ok", nil
		},
	}
	reg.Register(tool)

	exec := reg.Wrap(tool, reg.middleware)
	_, err := exec(map[string]interface{}{})
	if err != nil {
		t.Fatalf("exec failed: %v", err)
	}

	// reg.Wrap 逆序应用 middleware，所以 mw1 最外层（后注册的先应用）
	// 执行顺序：mw1:before → mw2:before → handler → mw2:after → mw1:after
	expected := []string{"middleware1:before", "middleware2:before", "handler", "middleware2:after", "middleware1:after"}
	for i, e := range expected {
		if logLines[i] != e {
			t.Errorf("logLines[%d] = %q, want %q", i, logLines[i], e)
		}
	}
}

func TestExtractToolCalls(t *testing.T) {
	text := "I need to [TOOL:list_files] then [TOOL:read_file:{\"path\":\"a.txt\"}]"
	calls := ExtractToolCalls(text)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Name != "list_files" || calls[0].Args != "" {
		t.Errorf("unexpected first call: %+v", calls[0])
	}
	if calls[1].Name != "read_file" || calls[1].Args != `{"path":"a.txt"}` {
		t.Errorf("unexpected second call: %+v", calls[1])
	}
}

func TestListTools(t *testing.T) {
	reg := NewRegistry()
	disp := &Dispatcher{registry: reg}

	// Register two tools with different names
	tool1 := &MockTool{BaseTool: BaseTool{
		name:        "tool_alpha",
		description: "First tool",
		schema:      ToolSchema{Type: "object", Properties: map[string]PropertyDef{}},
		handler:     func(args map[string]interface{}) (string, error) { return "alpha", nil },
	}}
	tool2 := &MockTool{BaseTool: BaseTool{
		name:        "tool_beta",
		description: "Second tool",
		schema:      ToolSchema{Type: "object", Properties: map[string]PropertyDef{}},
		handler:     func(args map[string]interface{}) (string, error) { return "beta", nil },
	}}
	reg.Register(tool1)
	reg.Register(tool2)

	tools := disp.ListTools()
	if len(tools) != 2 {
		t.Errorf("expected 2 tools, got %d: %v", len(tools), tools)
	}
	found := make(map[string]bool)
	for _, n := range tools {
		found[n] = true
	}
	if !found["tool_alpha"] || !found["tool_beta"] {
		t.Errorf("expected tool_alpha and tool_beta, got %v", tools)
	}
}

func TestMarshalArgs(t *testing.T) {
	args := map[string]interface{}{
		"name": "Alice",
		"age":  30,
	}
	jsonStr := MarshalArgs(args)
	var parsed map[string]interface{}
	json.Unmarshal([]byte(jsonStr), &parsed)
	if parsed["name"] != "Alice" {
		t.Errorf("name mismatch: %v", parsed["name"])
	}
}

// ---- 注册到 global registry 的辅助函数 ----

func Register(t *BaseTool) {
	globalRegistry.Register(t)
}
