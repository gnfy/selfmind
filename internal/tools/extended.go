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

// RegisterExtendedTools 注册所有扩展工具到 dispatcher
func RegisterExtendedTools(d *Dispatcher) {
	d.RegisterTool(NewWebSearchTool())
	d.RegisterTool(NewWebExtractTool())
	d.RegisterTool(NewVisionTool())
	d.RegisterTool(NewTTSTool())
	d.RegisterTool(NewExecuteCodeTool())
	d.RegisterTool(NewSessionSearchTool())
	d.RegisterTool(NewTodoTool())
	d.RegisterTool(NewClarifyTool())
	d.RegisterTool(NewDelegateTool())
	// CheckpointTool 需要运行时注入 memFn/msgFn，
	// 不在这里注册，改由 main.go 中 disp.RegisterTool() 直接注入
}
