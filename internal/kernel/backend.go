package kernel

// ToolBackend is the interface through which the Agent dispatches tool calls.
// It abstracts the tool registry so the kernel does not depend on the tools package.
type ToolBackend interface {
	Dispatch(name string, args map[string]interface{}) (string, error)
	GetToolDefinitions() []map[string]interface{}
}
