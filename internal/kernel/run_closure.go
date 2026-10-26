package kernel

import "encoding/json"

const maxFinishStateRecoveries = 3

// A failed precondition is not a successful finish. Each bounded correction
// needs a subsequent accepted Plan change or successful check; a previous
// correction cannot permanently exclude a later valid completion.
// Other failures (including rejection and unknown effects) never unlock it.
type finishCorrection struct {
	pending    bool
	recoveries int
}

func (f *finishCorrection) observe(results []toolExecutionResult, counts map[string]int) {
	for _, result := range results {
		if result.heldBeforeDispatch {
			continue
		}
		if result.toolName == "finish_run" {
			if !result.success && result.errorCode == "completion_context_changed" && result.effectState == "not_dispatched" {
				counts["finish_run"] = max(0, counts["finish_run"]-1)
				f.pending = false
				continue
			}
			// A preparation failure with a proven not-dispatched effect did not
			// consume a committed lifecycle action. Input repair is bounded by
			// the Run's iteration budget and unchanged-input recovery policy,
			// independently of evidence-based completion recovery below.
			if !result.success &&
				result.failurePhase == "preparation" &&
				result.retryability == "corrected_input" &&
				result.effectState == "not_dispatched" {
				counts["finish_run"] = max(0, counts["finish_run"]-1)
				f.pending = false
				continue
			}
			f.pending = !result.success && result.effectState == "not_dispatched" &&
				(result.errorCode == "completion_precondition" || result.errorCode == "condition_assessment_invalid")
			if f.pending && f.recoveries < maxFinishStateRecoveries && counts["update_plan"] >= lifecycleToolCap("update_plan") {
				counts["update_plan"] = lifecycleToolCap("update_plan") - 1
			}
			continue
		}
		if !f.pending || f.recoveries >= maxFinishStateRecoveries || !result.success {
			continue
		}
		changed := result.toolName == "verify" || (!isLifecycleToolName(result.toolName) && result.toolName != "")
		if result.toolName == "update_plan" {
			var state struct {
				Changed bool `json:"changed"`
			}
			changed = json.Unmarshal([]byte(result.rawResult), &state) == nil && state.Changed
		}
		if changed {
			counts["finish_run"] = max(0, counts["finish_run"]-1)
			f.recoveries++
			f.pending = false
		}
	}
}
