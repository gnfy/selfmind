package verification

import (
	"fmt"
	"strings"
)

// EvidenceIssue retains observed failure meaning across control/tool adapters.
// It describes an observation, never a completion or retry authorization.
type EvidenceIssue struct {
	Ref, StepID, Kind      string
	Criterion, Target, CWD string
}

func (e EvidenceIssue) Error() string {
	if e.Kind == "stale" {
		return fmt.Sprintf("verification evidence %q is stale after changes to its inputs", e.Ref)
	}
	return fmt.Sprintf("verification evidence %q has no confirmed execution and exit observation", e.Ref)
}

type EvidenceIssuesError struct {
	Issues  []EvidenceIssue
	Omitted int
}

func (e *EvidenceIssuesError) Error() string {
	messages := make([]string, 0, len(e.Issues)+1)
	for _, issue := range e.Issues {
		messages = append(messages, issue.Error())
	}
	if e.Omitted > 0 {
		messages = append(messages, fmt.Sprintf("%d more unusable observations omitted", e.Omitted))
	}
	return strings.Join(messages, "; ")
}
func (e *EvidenceIssuesError) add(issue EvidenceIssue) {
	for _, old := range e.Issues {
		if old == issue {
			return
		}
	}
	if len(e.Issues) >= 8 {
		e.Omitted++
		return
	}
	e.Issues = append(e.Issues, issue)
}
func observationIssue(ref string, check Check, mutations []Mutation) *EvidenceIssue {
	kind := ""
	static := check.Observation != nil && check.Observation.Kind == "file" && check.Status == "succeeded"
	if check.Invoked == nil || !*check.Invoked || !static && (check.ProcessStarted == nil || !*check.ProcessStarted || check.ExitCode < 0) || check.Status == "blocked" || check.EffectState == "unknown" || check.EffectState == "not_dispatched" {
		kind = "unconfirmed"
	} else if check.StartedAt < RelevantMutationAt(check, mutations) {
		kind = "stale"
	}
	if kind == "" {
		return nil
	}
	issue := &EvidenceIssue{Ref: ref, Kind: kind, CWD: check.CWD}
	if check.Binding != nil {
		issue.StepID = check.Binding.StepID
		issue.Criterion = check.Binding.Criterion
		issue.Target = check.Binding.Target
	}
	return issue
}
