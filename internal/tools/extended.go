package tools

// =============================================================================
// Extended Tools — registration entry point
//
// Individual tools are in separate files:
//   web_search.go   — WebSearchTool, WebExtractTool
//   vision.go       — VisionTool
//   tts.go          — TTSTool
//   execute_code.go — ExecuteCodeTool
//   session_search.go — SessionSearchTool
//   todo.go         — TodoTool
//   checkpoint.go   — CheckpointTool (runtime injection)
//   delegate.go     — DelegateTool
// =============================================================================

// RegisterExtendedTools 注册所有扩展工具到 dispatcher. An optional
// WebSearchOptions configures the web_search backend from config; when
// omitted, web_search falls back to env-var credentials (backward compatible).
func RegisterExtendedTools(d *Dispatcher, webOpts ...WebSearchOptions) *PlanStore {
	if len(webOpts) > 0 {
		d.RegisterTool(NewWebSearchToolWithOptions(webOpts[0]))
	} else {
		d.RegisterTool(NewWebSearchTool())
	}
	d.RegisterTool(NewWebExtractTool())
	d.RegisterTool(NewVisionTool())
	d.RegisterTool(NewTTSTool())
	d.RegisterTool(NewExecuteCodeTool())
	d.RegisterTool(NewBatchReadTool(d.Dispatch, d.ToolExecutionMetadata))
	d.RegisterTool(NewSessionSearchTool())
	planStore := NewPlanStore()
	d.RegisterTool(NewUpdatePlanToolWithStore(planStore))
	d.RegisterTool(NewFinishRunToolWithStore(planStore))
	d.RegisterTool(NewToolSearchTool())
	d.RegisterTool(NewTodoTool())
	d.RegisterTool(NewClarifyTool())
	d.RegisterTool(NewDelegateTool())
	// request_permissions is the reverse side of the approval funnel: one ask for
	// the roots and hosts a task needs, instead of one ask per operation.
	d.RegisterTool(NewRequestPermissionsTool())
	// CheckpointTool 需要运行时注入 memFn/msgFn，
	// 不在这里注册，改由 main.go 中 disp.RegisterTool() 直接注入
	return planStore
}
