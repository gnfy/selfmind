package api

// RunView is an observation of durable work, never a selection of execution
// authority. Options are exact Run ids, so a changing list cannot route input.
type RunView struct {
	RunID       string          `json:"run_id,omitempty"`
	Title       string          `json:"title,omitempty"`
	Status      string          `json:"status,omitempty"`
	Content     string          `json:"content,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	PlanJSON    string          `json:"plan_json,omitempty"`
	CanContinue bool            `json:"can_continue,omitempty"`
	Options     []RunViewOption `json:"options,omitempty"`
}

// RunObservation is an observation's recorded state, not proof that the goal
// completed. Losing a subscription or finding no active Run proves neither.
type RunObservation struct {
	RunID       string
	Status      string
	Summary     string
	FinalAnswer string
}

type RunViewOption struct {
	RunID  string `json:"run_id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}
