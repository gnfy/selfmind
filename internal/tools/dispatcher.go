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
	mu        sync.RWMutex
	tools     map[string]Tool
	clarifyFn ClarifyHandler
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
	if len(t.Schema().Properties) > 0 {
		coerced, coerceErr := CoerceArgs(t.Schema(), args)
		if coerceErr != nil {
			return "", fmt.Errorf("failed to coerce arguments for %s: %w", name, coerceErr)
		}
		args = coerced
	}
	if err := ValidateArgs(t.Schema(), args); err != nil {
		return "", fmt.Errorf("argument validation failed for %s: %w", name, err)
	}
	exec := r.Wrap(t, r.Middlewares())
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
		args["_registry"] = r
		if clarifyFn := r.ClarifyHandler(); clarifyFn != nil {
			args["_clarify_fn"] = clarifyFn
		}
		return exec(args)
	}
}

// UseMiddleware appends a middleware to the global registry
func (r *Registry) UseMiddleware(mw Middleware) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.middleware = append(r.middleware, mw)
}

func (r *Registry) SetClarifyHandler(fn ClarifyHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clarifyFn = fn
}

func (r *Registry) ClarifyHandler() ClarifyHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.clarifyFn
}

// Middlewares returns a stable snapshot of the registry middleware chain.
func (r *Registry) Middlewares() []Middleware {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Middleware{}, r.middleware...)
}

// ---- Dispatcher ----

// Dispatcher 负责工具的调度（兼容旧接口）
type Dispatcher struct {
	registry *Registry
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{registry: NewRegistry()}
}

func NewGlobalDispatcher() *Dispatcher {
	return &Dispatcher{registry: globalRegistry}
}

func NewDispatcherWithRegistry(r *Registry) *Dispatcher {
	if r == nil {
		r = NewRegistry()
	}
	return &Dispatcher{registry: r}
}

// Register implements legacy handler-based registration by wrapping into BaseTool
func (d *Dispatcher) Register(name string, handler func(args string) (string, error)) {
	d.registry.Register(&BaseTool{
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
	d.registry.Register(t)
}

// UnregisterTool removes a tool from this dispatcher's registry.
func (d *Dispatcher) UnregisterTool(name string) {
	d.registry.Unregister(name)
}

// Dispatch 调用已注册的工具，自动执行 middleware 链
func (d *Dispatcher) Dispatch(name string, args map[string]interface{}) (string, error) {
	t, ok := d.registry.Get(name)
	if !ok {
		return "", fmt.Errorf("tool %s not found", name)
	}

	// Coerce types first (LLMs often pass numbers as strings), then validate
	if len(t.Schema().Properties) > 0 {
		coerced, coerceErr := CoerceArgs(t.Schema(), args)
		if coerceErr != nil {
			return "", fmt.Errorf("failed to coerce arguments for %s: %w", name, coerceErr)
		}
		args = coerced
	}

	if err := ValidateArgs(t.Schema(), args); err != nil {
		return "", fmt.Errorf("argument validation failed for %s: %w", name, err)
	}

	exec := d.registry.Wrap(t, d.registry.Middlewares())
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
	t, ok := d.registry.Get(toolName)
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

func (d *Dispatcher) SupportsParallelTool(name string) bool {
	t, ok := d.registry.Get(name)
	if !ok {
		return false
	}
	return ToolMetadataFor(t).SupportsParallel
}

// ListTools returns all registered tool names
func (d *Dispatcher) ListTools() []string {
	return d.registry.List()
}

// GetToolDefinitions returns all tools as LLM tool definitions
func (d *Dispatcher) GetToolDefinitions() []map[string]interface{} {
	return d.registry.ToolDefinitions()
}

// InjectMiddleware adds a middleware to the dispatcher (for Approval/Auth chain)
func (d *Dispatcher) InjectMiddleware(mw Middleware) {
	d.registry.UseMiddleware(mw)
}

func (d *Dispatcher) InjectClarifyHandler(fn ClarifyHandler) {
	d.registry.SetClarifyHandler(fn)
}

// InjectSessionSearch 将 memory 模块的 searchFn 注入到 SessionSearchTool
func (d *Dispatcher) InjectSessionSearch(fn func(query string, limit int) (interface{}, error)) {
	t, ok := d.registry.Get("session_search")
	if !ok {
		return
	}
	if sst, ok := t.(*SessionSearchTool); ok {
		sst.RegisterSearchFn(fn)
	}
}

func (d *Dispatcher) InjectSessionAccess(
	searchFn func(query string, limit int) (interface{}, error),
	recentFn func(limit int) (interface{}, error),
	messagesFn func(sessionID string, aroundMessageID, window int) (interface{}, error),
) {
	t, ok := d.registry.Get("session_search")
	if !ok {
		return
	}
	if sst, ok := t.(*SessionSearchTool); ok {
		sst.RegisterAccessFns(searchFn, recentFn, messagesFn)
	}
}

func (d *Dispatcher) InjectTenantSessionAccess(
	searchFn func(tenantID, query string, limit int) (interface{}, error),
	recentFn func(tenantID string, limit int) (interface{}, error),
	messagesFn func(tenantID, sessionID string, aroundMessageID, window int) (interface{}, error),
) {
	t, ok := d.registry.Get("session_search")
	if !ok {
		return
	}
	if sst, ok := t.(*SessionSearchTool); ok {
		sst.RegisterTenantAccessFns(searchFn, recentFn, messagesFn)
	}
}

// InjectDelegateFn 将 delegate_fn 注入到 DelegateTool
func (d *Dispatcher) InjectDelegateFn(fn func(goal, context string, toolsets []string) (string, llm.UsageStats, error)) {
	t, ok := d.registry.Get("delegate_task")
	if !ok {
		return
	}
	if dt, ok := t.(*DelegateTool); ok {
		dt.RegisterDelegateFn(fn)
	}
}

// InjectVisionLLM 将视觉分析所需的 LLM 接口注入到 VisionTool
func (d *Dispatcher) InjectDelegateBatchFn(fn func(tasks []DelegateTaskSpec) ([]DelegateTaskResult, error)) {
	t, ok := d.registry.Get("delegate_task")
	if !ok {
		return
	}
	if dt, ok := t.(*DelegateTool); ok {
		dt.RegisterBatchDelegateFn(fn)
	}
}

func (d *Dispatcher) InjectVisionLLM(provider VisionLLM) {
	t, ok := d.registry.Get("vision_analyze")
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
