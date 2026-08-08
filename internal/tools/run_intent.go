package tools

import "strings"

// RunIntentSnapshot separates durable control-plane facts from user-authored
// evidence before an approval judge sees them. GoalSummary is advisory context;
// none of these fields bypasses deterministic approval floors or stored grants.
type RunIntentSnapshot struct {
	RawUserText   string   `json:"raw_user_text,omitempty"`
	GoalSummary   string   `json:"goal_summary,omitempty"`
	WorkKey       string   `json:"work_key,omitempty"`
	WorkspaceID   string   `json:"workspace_id,omitempty"`
	Source        string   `json:"source,omitempty"`
	ExplicitAllow []string `json:"explicit_allow,omitempty"`
	ExplicitDeny  []string `json:"explicit_deny,omitempty"`
}

// UserAuthored reports whether RawUserText came from a person in the current
// turn. System prompts may describe desired work, but they are never current
// human authorization for a side effect.
func (s RunIntentSnapshot) UserAuthored() bool {
	source := strings.ToLower(strings.TrimSpace(s.Source))
	return source == "" || source == "direct" || source == "continuation"
}

// HasExplicitDeny prevents an automatic containment approval. A deny remains a
// reason to ask or reject, never an instruction the model may reinterpret.
func (s RunIntentSnapshot) HasExplicitDeny() bool {
	for _, item := range s.ExplicitDeny {
		if strings.TrimSpace(item) != "" {
			return true
		}
	}
	return false
}
