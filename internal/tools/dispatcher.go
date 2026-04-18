package tools

import (
	"encoding/json"
	"fmt"
	"selfmind/internal/kernel/llm"
	"strings"
	"sync"
)

// Registry 是全局工具注册表
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	// middleware 链
	middleware []Middleware
}

var globalRegistry = &Registry{
	tools: make(map[string]Tool),
}

// GlobalRegistry returns the singleton global tool registry.
// Use this to share the registry between Dispatcher and SkillLoader.
func GlobalRegistry() *Registry {
	return globalRegistry
}

// NewRegistry creates a new tool registry (can be used for isolation)
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Register adds a tool to the registry
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Unregister removes a tool from the registry
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// Get returns a tool by name
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// ListTools returns all registered tool names
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	return names
}

// Dispatch executes a registered tool by name (used by SkillTool to call execute_command)
func (r *Registry) Dispatch(name string, args map[string]interface{}) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("tool %s not found", name)
	}
	exec := r.Wrap(t, r.middleware)
	return exec(args)
}

// ToolDefinitions returns all tools as LLM-compatible tool definitions
func (r *Registry) ToolDefinitions() []map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]map[string]interface{}, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, ToToolDefinition(t))
	}
	return defs
}

// ---- Middleware pipeline ----

// Middleware defines a tool execution middleware
type Middleware func(next ToolExecutor) ToolExecutor

// ToolExecutor 执行工具的函数签名
type ToolExecutor func(args map[string]interface{}) (string, error)

// Wrap wraps a handler with middleware chain
func (r *Registry) Wrap(t Tool, mw []Middleware) ToolExecutor {
	exec := func(args map[string]interface{}) (string, error) {
		return t.Execute(args)
	}
	// 逆序应用 middleware（从最外层到最内层）
	for i := len(mw) - 1; i >= 0; i-- {
		exec = mw[i](exec)
	}
	// 返回注入元数据的最终执行器
	return func(args map[string]interface{}) (string, error) {
		if args == nil {
			args = make(map[string]interface{})
		}
		args["_tool_name"] = t.Name()
		return exec(args)
	}
}

// UseMiddleware appends a middleware to the global registry
func (r *Registry) UseMiddleware(mw Middleware) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.middleware = append(r.middleware, mw)
}

// ---- Dispatcher ----

// Dispatcher 负责工具的调度（兼容旧接口）
type Dispatcher struct {
	registry *Registry
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{registry: globalRegistry}
}

func NewDispatcherWithRegistry(r *Registry) *Dispatcher {
	return &Dispatcher{registry: r}
}

// Register implements legacy handler-based registration by wrapping into BaseTool
func (d *Dispatcher) Register(name string, handler func(args string) (string, error)) {
	globalRegistry.Register(&BaseTool{
		name:        name,
		description: fmt.Sprintf("Tool registered as %s", name),
		schema:      ToolSchema{Type: "object"},
		handler: func(args map[string]interface{}) (string, error) {
			b, _ := json.Marshal(args)
			return handler(string(b))
		},
	})
}

// RegisterTool 注册一个 Tool 接口实现
func (d *Dispatcher) RegisterTool(t Tool) {
	globalRegistry.Register(t)
}

// Dispatch 调用已注册的工具，自动执行 middleware 链
func (d *Dispatcher) Dispatch(name string, args map[string]interface{}) (string, error) {
	t, ok := globalRegistry.Get(name)
	if !ok {
		return "", fmt.Errorf("tool %s not found", name)
	}
	exec := globalRegistry.Wrap(t, globalRegistry.middleware)
	return exec(args)
}

// DispatchRaw 兼容旧接口：接收 JSON 字符串，解析后 dispatch
func (d *Dispatcher) DispatchRaw(name string, rawArgs string) (string, error) {
	var args map[string]interface{}
	if rawArgs != "" {
		json.Unmarshal([]byte(rawArgs), &args)
	}
	return d.Dispatch(name, args)
}

// RegisterSkill 动态注册新生成的技能（兼容旧接口）
func (d *Dispatcher) RegisterSkill(name string, handler func(args string) (string, error)) {
	d.Register(name, handler)
}

// CoerceArgs 将动态类型强制转换为 tool schema 声明的类型
func (d *Dispatcher) CoerceArgs(toolName string, args map[string]interface{}) (map[string]interface{}, error) {
	t, ok := globalRegistry.Get(toolName)
	if !ok {
		return nil, fmt.Errorf("tool %s not found", toolName)
	}
	return CoerceArgs(t.Schema(), args)
}

// ToolExists checks if a tool is registered
func (d *Dispatcher) ToolExists(name string) bool {
	_, ok := d.registry.Get(name)
	return ok
}

// GetTool returns a registered tool by name
func (d *Dispatcher) GetTool(name string) (Tool, bool) {
	return d.registry.Get(name)
}

// ListTools returns all registered tool names
func (d *Dispatcher) ListTools() []string {
	return d.registry.List()
}

// GetToolDefinitions returns all tools as LLM tool definitions
func (d *Dispatcher) GetToolDefinitions() []map[string]interface{} {
	return globalRegistry.ToolDefinitions()
}

// InjectMiddleware adds a middleware to the dispatcher (for Approval/Auth chain)
func (d *Dispatcher) InjectMiddleware(mw Middleware) {
	globalRegistry.UseMiddleware(mw)
}

// InjectSessionSearch 将 memory 模块的 searchFn 注入到 SessionSearchTool
func (d *Dispatcher) InjectSessionSearch(fn func(query string, limit int) (interface{}, error)) {
	t, ok := globalRegistry.Get("session_search")
	if !ok {
		return
	}
	if sst, ok := t.(*SessionSearchTool); ok {
		sst.RegisterSearchFn(fn)
	}
}

// InjectDelegateFn 将 delegate_fn 注入到 DelegateTool
func (d *Dispatcher) InjectDelegateFn(fn func(goal, context string, toolsets []string) (string, llm.UsageStats, error)) {
	t, ok := globalRegistry.Get("delegate_task")
	if !ok {
		return
	}
	if dt, ok := t.(*DelegateTool); ok {
		dt.RegisterDelegateFn(fn)
	}
}

// InjectVisionLLM 将视觉分析所需的 LLM 接口注入到 VisionTool
func (d *Dispatcher) InjectVisionLLM(provider VisionLLM) {
	t, ok := globalRegistry.Get("vision_analyze")
	if !ok {
		return
	}
	if vt, ok := t.(*VisionTool); ok {
		RegisterVisionTool(vt, provider)
	}
}

// ParseToolCalls 从 LLM 响应文本中提取 [TOOL:name] 格式的工具调用
func ParseToolCalls(text string) []string {
	var calls []string
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if strings.Contains(line, "[TOOL:") {
			idx := strings.Index(line, "[TOOL:")
			rest := line[idx+6:]
			if idx := strings.Index(rest, "]"); idx >= 0 {
				calls = append(calls, strings.TrimSpace(rest[:idx]))
			}
		}
	}
	return calls
}
