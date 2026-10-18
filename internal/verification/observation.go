package verification

import "encoding/json"

// Observation describes the scope actually read by a built-in tool. It is a
// fact, not an acceptance judgment. Nil dependencies conservatively mean all
// local inputs. It grants neither execution authority nor effect settlement.
type Observation struct {
	Kind                        string    `json:"kind"`
	Target                      string    `json:"target"`
	CWD                         string    `json:"cwd,omitempty"`
	LocalDependencies           *[]string `json:"local_dependencies,omitempty"`
	ResolvedLocalDependencies   []string  `json:"resolved_local_dependencies,omitempty"`
	UnresolvedLocalDependencies []string  `json:"unresolved_local_dependencies,omitempty"`
	Offset                      int       `json:"offset,omitempty"`
	Limit                       int       `json:"limit,omitempty"`
}

// Association is not source identity. Distinct executions with colliding ids
// must still be rejected, even if Main names a step.
func sameObservedFact(a, b Check) bool {
	if a.Source != nil || b.Source != nil || a.Observation == nil || b.Observation == nil {
		return false
	}
	a.Binding, b.Binding = nil, nil
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && string(left) == string(right)
}
