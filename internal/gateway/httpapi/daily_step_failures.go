package httpapi

import (
	"fmt"
	"sort"
	"strings"
)

const (
	dailyFailureContract = "contract"
	dailyFailureCommand  = "command"
	dailyFailureOther    = "other"
)

// dailyFailureClass separates a request the runtime refused or the call could
// not satisfy ("contract": malformed input, a stale precondition, a policy or
// protocol refusal) from the answer a command or check gave once it ran
// ("command"). The recorded error category decides; a category not named here,
// including "unknown", is "other" instead of being guessed into either side.
func dailyFailureClass(category string) string {
	switch category {
	case "invalid_input", "invalid_request", "stale_precondition", "policy", "protocol",
		"blocked_model_protocol", "tool_schema", "workspace_scope":
		return dailyFailureContract
	case "command_failed", "exit_status", "not_found", "exec_not_found", "timeout", "syntax",
		"check_definition", "sandbox_fs_denied", "sandbox_no_network", "sandbox_denied",
		"credential_state_readonly", "auth", "permission", "network", "environment":
		return dailyFailureCommand
	default:
		return dailyFailureOther
	}
}

// dailyStepFailure is one failed tool call inside a model step.
type dailyStepFailure struct{ tool, class string }

// dailyCallFailure reads a tool.completed payload as a failure of the model's
// work. A human wait is not one, a policy redirect is counted apart by the
// report, and a call the Plan boundary held is already counted as held.
func dailyCallFailure(tool, message, category, code string) (dailyStepFailure, bool) {
	if strings.TrimSpace(message) == "" {
		return dailyStepFailure{}, false
	}
	category = fallback(strings.TrimSpace(category), "unknown")
	if category == "human_wait" || category == "policy_redirect" || code == "plan_boundary_held" {
		return dailyStepFailure{}, false
	}
	return dailyStepFailure{tool: tool, class: dailyFailureClass(category)}, true
}

// dailyToolFailureCost is what one tool's failed steps cost, and how often the
// very next step called that tool again.
type dailyToolFailureCost struct {
	Steps, RetrySteps int
	MS, RetryMS       int64
	Classes           map[string]int
}

// dailyStepFailures is the model time spent on steps whose tool calls failed.
// A step is the tool calls one model call requested; its cost is that call's
// duration, the part the harness can influence only by changing what the
// model is told or refused.
type dailyStepFailures struct {
	Steps, RetrySteps int
	MS, RetryMS       int64
	ClassSteps        map[string]int
	ClassMS           map[string]int64
	ByTool            map[string]*dailyToolFailureCost
}

type failedStepRef struct {
	seq  int
	tool string
}

// dailyStepFailureTracker reads one Run's steps in order. Retry is the step
// immediately after a failed one that is not itself failed and calls the same
// tool again: the correction attempt a clearer refusal could shorten.
type dailyStepFailureTracker struct {
	dailyStepFailures
	seq      map[string]int
	latestMS map[string]int64
	failedAt map[string]failedStepRef
}

func newDailyStepFailureTracker() *dailyStepFailureTracker {
	return &dailyStepFailureTracker{
		dailyStepFailures: dailyStepFailures{
			ClassSteps: map[string]int{}, ClassMS: map[string]int64{}, ByTool: map[string]*dailyToolFailureCost{},
		},
		seq: map[string]int{}, latestMS: map[string]int64{}, failedAt: map[string]failedStepRef{},
	}
}

// modelCall notes that the Run made another model call.
func (t *dailyStepFailureTracker) modelCall(runID string, durationMS int64) {
	t.seq[runID]++
	t.latestMS[runID] = durationMS
}

// open stamps a step with the model call that requested its tools.
func (t *dailyStepFailureTracker) open(runID string, step *dailyToolStep) {
	step.seq, step.modelMS = t.seq[runID], t.latestMS[runID]
}

func (t *dailyStepFailureTracker) tool(name string) *dailyToolFailureCost {
	cost := t.ByTool[name]
	if cost == nil {
		cost = &dailyToolFailureCost{Classes: map[string]int{}}
		t.ByTool[name] = cost
	}
	return cost
}

// finish accounts a step once its Run's next model call, or the end of the
// events, closes it. A step whose model call precedes the window has no cost
// to attribute and is left out, not counted as free.
func (t *dailyStepFailureTracker) finish(runID string, step *dailyToolStep) {
	if step.seq == 0 {
		return
	}
	previous, followsFailure := t.failedAt[runID]
	delete(t.failedAt, runID)
	if len(step.failures) == 0 {
		if followsFailure && step.seq == previous.seq+1 && containsString(step.calls, previous.tool) {
			t.RetrySteps++
			t.RetryMS += step.modelMS
			cost := t.tool(previous.tool)
			cost.RetrySteps++
			cost.RetryMS += step.modelMS
		}
		return
	}
	class := step.failures[0].class
	for _, failure := range step.failures[1:] {
		if failure.class != class {
			class = dailyFailureOther
			break
		}
	}
	tool := step.failures[0].tool
	t.Steps++
	t.MS += step.modelMS
	t.ClassSteps[class]++
	t.ClassMS[class] += step.modelMS
	cost := t.tool(tool)
	cost.Steps++
	cost.MS += step.modelMS
	cost.Classes[class]++
	t.failedAt[runID] = failedStepRef{seq: step.seq, tool: tool}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func formatMinutes(ms int64) string { return fmt.Sprintf("%.1f min", float64(ms)/60000) }

// render reports the cost against the Run's own model time, so a share is
// never read without its denominator. Tools are bounded to the five whose
// failed steps cost most.
func (f dailyStepFailures) render(modelMS int64) string {
	if f.Steps == 0 {
		return "Failed model steps: none\n"
	}
	share := ""
	if modelMS > 0 {
		share = fmt.Sprintf(", %d%% of %s model time", f.MS*100/modelMS, formatMinutes(modelMS))
	}
	var classes []string
	for _, class := range []string{dailyFailureContract, dailyFailureCommand, dailyFailureOther} {
		if f.ClassSteps[class] > 0 {
			classes = append(classes, fmt.Sprintf("%s %d (%s)", class, f.ClassSteps[class], formatMinutes(f.ClassMS[class])))
		}
	}
	line := fmt.Sprintf("Failed model steps: %d (%s%s): %s; retried with the same tool at once: %d more steps (%s)\n",
		f.Steps, formatMinutes(f.MS), share, strings.Join(classes, ", "), f.RetrySteps, formatMinutes(f.RetryMS))

	names := make([]string, 0, len(f.ByTool))
	for name, cost := range f.ByTool {
		if cost.Steps > 0 {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := f.ByTool[names[i]], f.ByTool[names[j]]
		if a.MS != b.MS {
			return a.MS > b.MS
		}
		if a.Steps != b.Steps {
			return a.Steps > b.Steps
		}
		return names[i] < names[j]
	})
	if len(names) > 5 {
		names = names[:5]
	}
	tools := make([]string, 0, len(names))
	for _, name := range names {
		cost := f.ByTool[name]
		detail := formatCountMap(cost.Classes)
		if cost.RetrySteps > 0 {
			detail += fmt.Sprintf("; retried %d", cost.RetrySteps)
		}
		tools = append(tools, fmt.Sprintf("%s %d (%s; %s)", name, cost.Steps, detail, formatMinutes(cost.MS)))
	}
	return line + "Failed steps by tool: " + strings.Join(tools, ", ") + "\n"
}
