package kernel

import "encoding/json"

// A failed precondition is not a successful finish. Permit one correction,
// only after the runtime actually applied a changed plan or a check succeeded.
// Other failures (including rejection and unknown effects) never unlock it.
type finishCorrection struct {
	pending bool
	used    bool
}

func (f *finishCorrection) observe(results []toolExecutionResult, counts map[string]int) {
	for _, result := range results {
		if result.toolName == "finish_run" {
			f.pending = !result.success && result.errorCode == "completion_precondition"
			continue
		}
		if !f.pending || f.used || !result.success {
			continue
		}
		changed := result.toolName == "verify"
		if result.toolName == "update_plan" {
			var state struct {
				Changed bool `json:"changed"`
			}
			changed = json.Unmarshal([]byte(result.rawResult), &state) == nil && state.Changed
		}
		if changed {
			counts["finish_run"] = max(0, counts["finish_run"]-1)
			f.used, f.pending = true, false
		}
	}
}
