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
	// ObservationOnly means the dispatcher proved this exact call only reads.
	// It answers "could this call have changed anything?" and never widens
	// replay, approval, or recovery authority, which keep the retry class.
	ObservationOnly bool
}

// ToolExecutionMetadataProvider is optional so test and compatibility backends
// do not need to implement it. Production dispatchers provide the metadata from
// the registered Tool after schema validation.
type ToolExecutionMetadataProvider interface {
	ToolExecutionMetadata(name string, args map[string]interface{}) ToolExecutionMetadata
}

// ToolArgumentPreparer is the optional pre-dispatch boundary implemented by
// production registries. The Agent calls it before recovery policy, durable
// ledger claims, and tool.started so malformed input cannot become execution
// evidence. Implementations must be deterministic and safe to call again from
// compatibility dispatch paths.
type ToolArgumentPreparer interface {
	PrepareToolArguments(name string, args map[string]interface{}) (map[string]interface{}, error)
}

// ToolPreparationStateProvider identifies the current schema/catalogue state
// for exact malformed-call suppression. A changed value lets an unchanged call
// be validated again after live tool discovery changes its contract.
type ToolPreparationStateProvider interface {
	ToolPreparationState(name string) string
}
