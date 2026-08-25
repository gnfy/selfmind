package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
)

const (
	// Keep the automatic cohort inert until the active plan's seven-day
	// fingerprint baseline is reviewed. Explicitly deferred tool metadata still
	// exercises the activation protocol; enabling the cohort is a reviewed code
	// release, not a hidden runtime or user configuration switch.
	deferredExternalRolloutEnabled = false
)

// deferredExternalReviewedCohort is populated only from a reviewed seven-day
// usage report. Name hashing is deterministic but is not evidence that a tool
// is cold; keeping that old selector behind a false flag merely deferred a
// production regression until someone enabled it. An empty allowlist means no
// automatic deferral even if the rollout gate is accidentally flipped.
var deferredExternalReviewedCohort = map[string]struct{}{}

// Registry 是全局工具注册表
type Registry struct {
	mu        sync.RWMutex
	tools     map[string]Tool
	schemas   map[string]compiledToolSchema
	clarifyFn ClarifyHandler
	// middleware 链
	middleware []Middleware
}

var globalRegistry = &Registry{
	tools:   make(map[string]Tool),
	schemas: make(map[string]compiledToolSchema),
}

// GlobalRegistry returns the singleton global tool registry.
// Use this to share the registry between Dispatcher and SkillLoader.
func GlobalRegistry() *Registry {
	return globalRegistry
}

// NewRegistry creates a new tool registry (can be used for isolation)
func NewRegistry() *Registry {
	return &Registry{
		tools:   make(map[string]Tool),
		schemas: make(map[string]compiledToolSchema),
	}
}

// Register adds a tool to the registry
func (r *Registry) Register(t Tool) {
	compiled := compileToolSchema(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureMaps()
	r.tools[t.Name()] = t
	r.schemas[t.Name()] = compiled
}

// Unregister removes a tool from the registry
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
	delete(r.schemas, name)
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
		if compiled, ok := r.schemas[name]; ok && compiled.Report.Status == ToolSchemaQuarantined {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Dispatch is the single registry execution path shared by Dispatcher and
// direct compatibility callers.
func (r *Registry) Dispatch(name string, args map[string]interface{}) (string, error) {
	originalArgs := args
	if err := r.schemaAvailabilityError(name); err != nil {
		return "", err
	}
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
	result, err := exec(args)
	// Coercion deliberately creates a detached argument map. Copy back only the
	// daemon-owned execution-boundary fields needed to record what actually ran;
	// never reflect arbitrary tool mutations into the caller's map.
	if originalArgs != nil && args != nil {
		for _, key := range []string{"_effective_sandbox_mode", "_sandbox_mode"} {
			if value, ok := args[key]; ok {
				originalArgs[key] = value
			}
		}
	}
	return result, err
}

// ToolDefinitions returns all tools as LLM-compatible tool definitions
func (r *Registry) ToolDefinitions() []map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]map[string]interface{}, 0, len(r.tools))
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	exposures := r.effectiveToolExposuresLocked(deferredExternalRolloutEnabled)
	for _, name := range names {
		compiled, ok := r.schemas[name]
		if !ok || compiled.Report.Status == ToolSchemaQuarantined {
			continue
		}
		parameters, err := detachedSchemaMap(compiled.Parameters)
		if err != nil {
			continue
		}
		definition := toolDefinitionFromCompiled(r.tools[name], parameters)
		if metadata, ok := definition["selfmind"].(map[string]interface{}); ok {
			metadata["exposure"] = string(exposures[name])
		}
		defs = append(defs, definition)
	}
	return defs
}

// LookupToolExposure answers ONE tool's catalogue exposure without building the
// full definition list. The availability guard on the tool-dispatch hot path
// needs exactly this question; materializing every definition to answer it ran
// a JSON marshal/unmarshal round trip per registered tool (114 pairs per call
// in this workspace) inside the streaming loop.
//
// known is false for an unregistered or quarantined tool, matching what
// ToolDefinitions omits, so callers keep treating those as "not in the model
// tool surface" rather than as hidden.
func (r *Registry) LookupToolExposure(name string) (ToolExposure, bool) {
	if r == nil {
		return "", false
	}
	name = strings.TrimSpace(name)
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	if !ok {
		return "", false
	}
	if compiled, ok := r.schemas[name]; !ok || compiled.Report.Status == ToolSchemaQuarantined {
		return "", false
	}
	// While the automatic cohort is code-gated off, a tool's own metadata is the
	// whole answer, so the common path allocates nothing.
	if !deferredExternalRolloutEnabled {
		return ToolMetadataFor(tool).Exposure, true
	}
	return r.effectiveToolExposuresLocked(deferredExternalRolloutEnabled)[name], true
}

// EffectiveToolExposure returns the catalogue policy seen by the model.
// Explicit hidden/deferred metadata always wins. The reviewed external cohort
// remains code-gated until its evidence review enables it.
func (r *Registry) EffectiveToolExposure(name string) ToolExposure {
	if r == nil {
		return ToolExposureHidden
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.effectiveToolExposuresLocked(deferredExternalRolloutEnabled)[strings.TrimSpace(name)]
}

func (r *Registry) effectiveToolExposuresLocked(automaticCohort bool) map[string]ToolExposure {
	exposures := make(map[string]ToolExposure, len(r.tools))
	for name, tool := range r.tools {
		metadata := ToolMetadataFor(tool)
		exposures[name] = metadata.Exposure
	}
	if !automaticCohort {
		return exposures
	}
	for name := range deferredExternalReviewedCohort {
		tool, ok := r.tools[name]
		if !ok || exposures[name] != ToolExposureDirect || executionPolicyForTool(tool).Origin != ToolSchemaOriginExternal {
			continue
		}
		exposures[name] = ToolExposureDeferred
	}
	return exposures
}

func (r *Registry) ensureMaps() {
	if r.tools == nil {
		r.tools = make(map[string]Tool)
	}
	if r.schemas == nil {
		r.schemas = make(map[string]compiledToolSchema)
	}
}

func (r *Registry) schemaAvailabilityError(name string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	compiled, ok := r.schemas[name]
	if !ok || compiled.Report.Status != ToolSchemaQuarantined {
		return nil
	}
	reason := "invalid schema"
	for _, issue := range compiled.Report.Issues {
		if issue.Severity == ToolSchemaError {
			reason = issue.Code + " at " + issue.Path
			break
		}
	}
	return fmt.Errorf("tool %s unavailable: schema quarantined (%s)", name, reason)
}

// ToolSchemaReport returns a stable, detached diagnostic catalogue.
func (r *Registry) ToolSchemaReport() []ToolSchemaReport {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reports := make([]ToolSchemaReport, 0, len(r.schemas))
	exposures := r.effectiveToolExposuresLocked(deferredExternalRolloutEnabled)
	for _, compiled := range r.schemas {
		report := compiled.Report
		report.Exposure = exposures[report.Name]
		report.Issues = append([]ToolSchemaIssue(nil), report.Issues...)
		reports = append(reports, report)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].Name < reports[j].Name })
	return reports
}

// ValidateInternalToolSchemas makes built-in schema mistakes a startup error.
// External schemas are isolated at registration and intentionally excluded.
func (r *Registry) ValidateInternalToolSchemas() error {
	for _, report := range r.ToolSchemaReport() {
		if report.Origin == ToolSchemaOriginExternal || report.Status == ToolSchemaActive {
			continue
		}
		reason := "invalid schema"
		if len(report.Issues) > 0 {
			reason = report.Issues[0].Code + " at " + report.Issues[0].Path
		}
		return fmt.Errorf("built-in tool %s schema is not strict: %s", report.Name, reason)
	}
	return nil
}

// ---- Middleware pipeline ----

// Middleware defines a tool execution middleware
type Middleware func(next ToolExecutor) ToolExecutor

// ToolExecutor 执行工具的函数签名
type ToolExecutor func(args map[string]interface{}) (string, error)

// Wrap wraps a handler with middleware chain
func (r *Registry) Wrap(t Tool, mw []Middleware) ToolExecutor {
	exec := func(args map[string]interface{}) (string, error) {
		if contextual, ok := t.(ContextTool); ok {
			return contextual.ExecuteContext(ContextFromArgs(args), args)
		}
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
		args[toolExecutionPolicyArg] = executionPolicyForTool(t)
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
	return d.registry.Dispatch(name, args)
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
	if err := d.registry.schemaAvailabilityError(name); err != nil {
		return false
	}
	_, ok := d.registry.Get(name)
	return ok
}

// GetTool returns a registered tool by name
func (d *Dispatcher) GetTool(name string) (Tool, bool) {
	return d.registry.Get(name)
}

// ToolExecutionMetadata exposes daemon-owned registry facts to the kernel's
// durable event stream. Model-provided arguments cannot override this view.
func (d *Dispatcher) ToolExecutionMetadata(name string, args map[string]interface{}) kernel.ToolExecutionMetadata {
	t, ok := d.registry.Get(name)
	if !ok {
		return kernel.ToolExecutionMetadata{Origin: string(ToolSchemaOriginExternal), RiskLevel: string(ToolRiskHigh)}
	}
	policy := executionPolicyForTool(t)
	classifiedArgs := make(map[string]interface{}, len(args)+2)
	for key, value := range args {
		classifiedArgs[key] = value
	}
	classifiedArgs[toolExecutionPolicyArg] = policy
	classifiedArgs["_tool_name"] = name
	annotateEffectiveSandboxMode(classifiedArgs)
	// A completed execution records its actual plan mode. It is stronger than
	// the preflight prediction and prevents a runtime host fallback from being
	// published as isolated evidence.
	if actual := SandboxMode(strings.ToLower(strings.TrimSpace(stringArg(classifiedArgs, "_sandbox_mode")))); actual == SandboxHost || actual == SandboxIsolated {
		classifiedArgs["_effective_sandbox_mode"] = string(actual)
	}
	projectRoot := ""
	if scope, ok := currentExecutionScopeAny(classifiedArgs); ok {
		projectRoot = approvalProjectRoot("", scope, classifiedArgs)
	}
	dangerous, _ := dangerousToolCall(projectRoot, name, classifiedArgs)
	classes := operationClassesFor(name, classifiedArgs, dangerous)
	classes = uniqueOperationClasses(classes)
	classNames := make([]string, 0, len(classes))
	for _, class := range classes {
		classNames = append(classNames, string(class))
	}
	return kernel.ToolExecutionMetadata{
		Origin: string(policy.Origin), Category: policy.Category,
		RiskLevel: string(policy.Risk), ReadOnly: policy.ReadOnly,
		OperationClasses: classNames,
	}
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

// ResolveToolExposure is the optional single-lookup capability the kernel's
// dispatch-time availability guard prefers over materializing the whole
// catalogue. Returning known=false lets the caller fall back to its previous
// behaviour for unregistered names.
func (d *Dispatcher) ResolveToolExposure(name string) (exposure string, known bool) {
	if d == nil {
		return "", false
	}
	value, ok := d.registry.LookupToolExposure(name)
	if !ok {
		return "", false
	}
	return string(value), true
}

func (d *Dispatcher) ToolSchemaReport() []ToolSchemaReport {
	return d.registry.ToolSchemaReport()
}

func (d *Dispatcher) ValidateInternalToolSchemas() error {
	return d.registry.ValidateInternalToolSchemas()
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
func (d *Dispatcher) InjectDelegateFn(fn func(context.Context, string, string, []string) (string, llm.UsageStats, error)) {
	t, ok := d.registry.Get("delegate_task")
	if !ok {
		return
	}
	if dt, ok := t.(*DelegateTool); ok {
		dt.RegisterDelegateFn(fn)
	}
}

// InjectVisionLLM 将视觉分析所需的 LLM 接口注入到 VisionTool
func (d *Dispatcher) InjectDelegateBatchFn(fn func(context.Context, []DelegateTaskSpec) ([]DelegateTaskResult, error)) {
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
