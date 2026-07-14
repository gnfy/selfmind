package kernel

// RunEvidence is a durable, provider-neutral observation produced by the tool
// runtime. It deliberately contains observed facts only; model claims remain in
// RunOutcome and are compared with this evidence during finalization.
type RunEvidence struct {
	ToolCallID string            `json:"tool_call_id,omitempty"`
	ToolName   string            `json:"tool_name"`
	Kind       string            `json:"kind"`
	Status     string            `json:"status"`
	StartedAt  int64             `json:"started_at_unix_nano"`
	FinishedAt int64             `json:"finished_at_unix_nano"`
	Files      []FileEffect      `json:"files,omitempty"`
	Command    *CommandEvidence  `json:"command,omitempty"`
	Error      string            `json:"error,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type FileEffect struct {
	Path         string `json:"path"`
	Operation    string `json:"operation"`
	BeforeSHA256 string `json:"before_sha256,omitempty"`
	AfterSHA256  string `json:"after_sha256,omitempty"`
}

type CommandEvidence struct {
	Command  string `json:"command"`
	CWD      string `json:"cwd,omitempty"`
	Kind     string `json:"kind,omitempty"`
	ExitCode int    `json:"exit_code"`
}
