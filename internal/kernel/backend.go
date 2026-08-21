package kernel

// ToolBackend is the interface through which the Agent dispatches tool calls.
// It abstracts the tool registry so the kernel does not depend on the tools package.
type ToolBackend interface {
	Dispatch(name string, args map[string]interface{}) (string, error)
	GetToolDefinitions() []map[string]interface{}
}

// ToolExecutionMetadata is trusted registry metadata captured beside a durable
// tool.started event. It describes the authority surface of the registered tool,
// not authority granted by model arguments. The Skill lifecycle uses it to keep
// publication policy separate from the procedure's eventual execution policy.
type ToolExecutionMetadata struct {
	Origin           string
	Category         string
	RiskLevel        string
	ReadOnly         bool
	OperationClasses []string
}

// ToolExecutionMetadataProvider is optional so test and compatibility backends
// do not need to implement it. Production dispatchers provide the metadata from
// the registered Tool after schema validation.
type ToolExecutionMetadataProvider interface {
	ToolExecutionMetadata(name string, args map[string]interface{}) ToolExecutionMetadata
}
