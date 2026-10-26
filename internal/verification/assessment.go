package verification

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Assessment is Main's interpretation of observations. New input selects a
// logical step or exact current-Run references. Other identity fields, including
// adopted Source, are bound by the runtime and revalidated on every read.
// Execution status remains in Check and is not rewritten by this judgment.
type Assessment struct {
	Result        string       `json:"result"`
	EvidenceRefs  []string     `json:"evidence_refs"`
	Reason        string       `json:"reason"`
	Version       int          `json:"version,omitempty"`
	RunID         string       `json:"run_id,omitempty"`
	StepID        string       `json:"step_id,omitempty"`
	Criterion     string       `json:"criterion,omitempty"`
	Target        string       `json:"target,omitempty"`
	CWD           string       `json:"cwd,omitempty"`
	PlanCriterion string       `json:"plan_criterion,omitempty"`
	Source        *CheckSource `json:"source,omitempty"`
}

// CiteableEvidence is a verify check made in this Run that an assessment can cite.
type CiteableEvidence struct {
	ID     string `json:"id"`
	StepID string `json:"step_id,omitempty"`
	Target string `json:"target,omitempty"`
}

// EvidenceUnavailableError says an assessment cited an observation that is not an
// effective check of this Run: unknown, superseded, or made in an earlier Run.
// Call ids restart in every Run, so a bare id never selects another Run's
// observation. The error carries what can be cited instead, because a refusal
// that only says "unavailable" leaves the model to guess the way out.
type EvidenceUnavailableError struct {
	Ref      string
	StepID   string
	Citeable []CiteableEvidence
	// Note is what the runtime knows about Ref beyond this Run's own checks, for
	// example the earlier-Run observation a Plan step stands on.
	Note string
}

const evidenceUnavailableListed = 6

func (e *EvidenceUnavailableError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "verification evidence %q is unavailable or no longer effective in this Run: an assessment cites a verify check made in this Run, and call ids restart in every Run.", e.Ref)
	if len(e.Citeable) == 0 {
		// A note carries its own way forward; the generic one would contradict it.
		if e.Note == "" {
			b.WriteString(" This Run has no effective verify check yet; run verify (with step_id for a Plan step) and cite its call id.")
		}
	} else {
		b.WriteString(" Citeable now:")
		for i, c := range e.Citeable {
			if i == evidenceUnavailableListed {
				fmt.Fprintf(&b, " and %d more", len(e.Citeable)-i)
				break
			}
			if i > 0 {
				b.WriteString(";")
			}
			fmt.Fprintf(&b, " %s", c.ID)
			if c.StepID != "" {
				fmt.Fprintf(&b, " (step %s, target %q)", c.StepID, c.Target)
			} else {
				fmt.Fprintf(&b, " (target %q)", c.Target)
			}
		}
		b.WriteByte('.')
	}
	if e.Note != "" {
		b.WriteByte(' ')
		b.WriteString(e.Note)
	}
	return b.String()
}

// unusableEvidence says why a check cannot back an assessment, or "" when it can.
func unusableEvidence(ref string, check Check, mutations []Mutation) string {
	if issue := observationIssue(ref, check, mutations); issue != nil {
		return issue.Error()
	}
	return ""
}

// citeableEvidence lists the checks an assessment could cite right now.
func citeableEvidence(byID map[string]Check, mutations []Mutation) []CiteableEvidence {
	out := make([]CiteableEvidence, 0, len(byID))
	for id, check := range byID {
		if !ValidBinding(check.Binding) || unusableEvidence(id, check, mutations) != "" {
			continue
		}
		out = append(out, CiteableEvidence{ID: id, StepID: check.Binding.StepID, Target: check.Binding.Target})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

type AssessmentRequirement struct {
	Criterion        string
	PositiveRequired bool
}

type AssessmentProjection struct {
	State      string
	Conditions []Assessment
	Blockers   []string
	// ReviewComplete says that every effective failed check has an explicit,
	// conclusive Main finding. It does not turn a failed assertion into a pass.
	ReviewComplete bool
}

func ProjectAssessments(runID string, input []Assessment, checks []Check, mutations []Mutation, requirements map[string]AssessmentRequirement) (AssessmentProjection, error) {
	out := AssessmentProjection{State: "unassessed"}
	if len(input) == 0 {
		return out, nil
	}
	if len(input) > 128 {
		return out, fmt.Errorf("condition history exceeds the bounded review limit")
	}
	allChecks := checks
	checks = filterSupersededStaleStepChecks(checks, mutations)
	byID, err := currentEvidenceByID(checks)
	if err != nil {
		return out, err
	}
	covered := map[string]bool{}
	seen := map[string]bool{}
	type conditionIdentity struct{ step, criterion, target, cwd string }
	conditions := map[conditionIdentity]Assessment{}
	issues := &EvidenceIssuesError{}
	for i, assessment := range input {
		if assessment.Result != "met" && assessment.Result != "not_met" && assessment.Result != "unknown" {
			return out, fmt.Errorf("assessments[%d].result must be met, not_met or unknown", i)
		}
		assessment.Reason = strings.TrimSpace(assessment.Reason)
		if assessment.Reason == "" || len(assessment.Reason) > 1000 {
			return out, fmt.Errorf("assessments[%d] needs a reason of at most 1000 bytes", i)
		}
		if isSecondaryNoteOnAssessedStep(i, assessment, input, allChecks, byID) {
			continue
		}
		bound, err := assessmentChecks(assessment, input, checks, allChecks, byID, requirements)
		if err != nil {
			if assessment.Version == 0 && assessment.StepID != "" &&
				(len(assessment.EvidenceRefs) == 0 || allRefsBelongToOtherStepsOrPeers(assessment, input, allChecks, byID)) &&
				(assessment.Result == "unknown" || assessment.Result == "not_met") {
				if requirement, stepFound := requirements[assessment.StepID]; stepFound {
					if assessment.Result == "unknown" {
						out.Blockers = append(out.Blockers, "The condition for "+assessment.StepID+" is undetermined.")
					} else if requirement.PositiveRequired {
						out.Blockers = append(out.Blockers, "Required condition for step "+assessment.StepID+" is not met.")
					}
					if out.State == "unassessed" || assessment.Result == "not_met" || assessment.Result == "unknown" && out.State != "not_met" {
						out.State = assessment.Result
					}
					continue
				}
			}
			if isRedundantStepSummary(i, assessment, input, allChecks, byID, requirements) {
				continue
			}
			var unavailable *EvidenceUnavailableError
			if errors.As(err, &unavailable) {
				unavailable.Citeable = citeableEvidence(byID, mutations)
				unavailable.StepID = assessment.StepID
				if unavailable.StepID == "" {
					selected := ""
					ambiguous := false
					for _, check := range allChecks {
						if check.Source == nil && check.ToolCallID == unavailable.Ref && ValidBinding(check.Binding) {
							if selected != "" && selected != check.Binding.StepID {
								ambiguous = true
							}
							selected = check.Binding.StepID
						}
					}
					if !ambiguous {
						unavailable.StepID = selected
					}
				}
			}
			return out, err
		}
		var first *Check
		refs := make([]string, 0, len(bound))
		localSeen := map[string]bool{}
		var duplicateRef string
		invalidObservation := false
		for _, check := range bound {
			ref := check.ToolCallID
			if ref == "" || !ValidBinding(check.Binding) {
				return out, fmt.Errorf("finding has no bound observation identity")
			}
			if issue := observationIssue(ref, check, mutations); issue != nil {
				issues.add(*issue)
				invalidObservation = true
				continue
			}
			if first == nil {
				copy := check
				first = &copy
			} else if !sameStepAssessmentObligation(*first, check, len(assessment.EvidenceRefs) > 0) {
				return out, fmt.Errorf("assessments[%d] mixes different conditions, targets or sources; use exact current-Run evidence_refs for one condition", i)
			}
			key := assessmentReference(check, ref)
			if localSeen[key] {
				return out, fmt.Errorf("verification evidence %q has more than one assessment", ref)
			}
			localSeen[key] = true
			if seen[key] && duplicateRef == "" {
				duplicateRef = ref
			}
			refs = append(refs, ref)
		}
		if invalidObservation {
			continue
		}
		b := first.Binding
		if requirement := requirements[b.StepID]; requirement.PositiveRequired && assessment.Result == "met" {
			for _, check := range bound {
				if check.Observation != nil && check.Observation.Kind == "file" {
					return out, fmt.Errorf("step %q requires executable verification; a static file observation cannot prove it", b.StepID)
				}
			}
		}
		identity := conditionIdentity{b.StepID, b.Criterion, b.Target, first.CWD}
		if prev, exists := conditions[identity]; exists {
			if prev.Result == assessment.Result && isSubsetStringSlice(refs, prev.EvidenceRefs) && sameAssessmentSource(prev.Source, first.Source) {
				continue
			}
			if prev.Result == assessment.Result && duplicateRef == "" && sameAssessmentSource(prev.Source, first.Source) && len(prev.EvidenceRefs)+len(refs) <= 8 {
				for key := range localSeen {
					seen[key] = true
					covered[key] = assessment.Result != "unknown"
				}
				mergedRefs := append(append([]string(nil), prev.EvidenceRefs...), refs...)
				prev.EvidenceRefs = mergedRefs
				conditions[identity] = prev
				for idx := range out.Conditions {
					if out.Conditions[idx].StepID == prev.StepID && out.Conditions[idx].Criterion == prev.Criterion && out.Conditions[idx].Target == prev.Target && out.Conditions[idx].CWD == prev.CWD && sameAssessmentSource(out.Conditions[idx].Source, prev.Source) {
						out.Conditions[idx].EvidenceRefs = mergedRefs
						break
					}
				}
				continue
			}
			if duplicateRef != "" {
				return out, fmt.Errorf("verification evidence %q has more than one assessment", duplicateRef)
			}
			return out, fmt.Errorf("assessments[%d] repeats a condition; combine its observations in one finding", i)
		}
		if duplicateRef != "" {
			if isRedundantSubsetOfRecordedCondition(assessment.Result, first.Source, refs, out.Conditions) {
				continue
			}
			return out, fmt.Errorf("verification evidence %q has more than one assessment", duplicateRef)
		}
		for key := range localSeen {
			seen[key] = true
			covered[key] = assessment.Result != "unknown"
		}
		requirement, stepFound := requirements[b.StepID]
		if b.StepID != "" && !stepFound {
			return out, fmt.Errorf("verification step %q is not in the current Plan", b.StepID)
		}
		if assessment.Version != 0 {
			if (assessment.Version != 1 && assessment.Version != 2) || assessment.RunID != runID || assessment.StepID != b.StepID || assessment.Criterion != b.Criterion || assessment.Target != b.Target || assessment.CWD != first.CWD || assessment.PlanCriterion != requirement.Criterion {
				return out, fmt.Errorf("recorded condition assessment no longer matches its Run, Plan or evidence")
			}
		}
		if assessment.Version == 0 {
			assessment.Version = 1
			if first.Source != nil {
				assessment.Version = 2
				source := *first.Source
				assessment.Source = &source
			}
		}
		assessment.RunID = runID
		assessment.StepID, assessment.Criterion, assessment.Target = b.StepID, b.Criterion, b.Target
		assessment.CWD, assessment.PlanCriterion, assessment.EvidenceRefs = first.CWD, requirement.Criterion, refs
		conditions[identity] = assessment
		out.Conditions = append(out.Conditions, assessment)
		if assessment.Result == "unknown" {
			out.Blockers = append(out.Blockers, "The condition for "+b.Target+" is undetermined.")
		} else if requirement.PositiveRequired && assessment.Result != "met" {
			out.Blockers = append(out.Blockers, "Required condition for step "+b.StepID+" is not met.")
		}
		if out.State == "unassessed" || assessment.Result == "not_met" || assessment.Result == "unknown" && out.State != "not_met" {
			out.State = assessment.Result
		}
	}
	if len(issues.Issues) > 0 {
		return out, issues
	}
	out.ReviewComplete = len(out.Blockers) == 0
	for _, check := range Latest(checks) {
		if check.Status != "succeeded" && !covered[assessmentReference(check, check.ToolCallID)] {
			out.ReviewComplete = false
		}
	}
	return out, nil
}

// currentEvidenceByID is shared by discovery and acceptance; a preview must
// never advertise historical, superseded or ambiguous ids as current proof.
func currentEvidenceByID(checks []Check) (map[string]Check, error) {
	byID := map[string]Check{}
	sources := map[string]Check{}
	ambiguous := map[string]bool{}
	for _, check := range Latest(checks) {
		// A bare call id must still name exactly one condition. One confirmed
		// observation may have explicit bindings to several steps; step_id selects
		// those bindings, while bare-reference discovery omits the ambiguous id.
		if check.Source != nil || check.ToolCallID == "" {
			continue
		}
		id := check.ToolCallID
		if prior, exists := sources[id]; exists {
			if !sameObservedFact(prior, check) {
				return nil, fmt.Errorf("verification evidence id %q is ambiguous", id)
			}
			ambiguous[id] = true
			delete(byID, id)
		} else {
			sources[id] = check
		}
		if !ambiguous[id] {
			byID[id] = check
		}
	}
	return byID, nil
}

func CiteableChecks(checks []Check, mutations []Mutation) ([]CiteableEvidence, error) {
	checks = filterSupersededStaleStepChecks(checks, mutations)
	byID, err := currentEvidenceByID(checks)
	if err != nil {
		return nil, err
	}
	return citeableEvidence(byID, mutations), nil
}

func filterSupersededStaleStepChecks(checks []Check, mutations []Mutation) []Check {
	latest := Latest(checks)
	affectedAt := func(c Check) int64 { return RelevantMutationAt(c, mutations) }
	superseded := map[string]bool{}
	for _, c := range checks {
		if c.StartedAt < affectedAt(c) && stepCheckSupersededByFreshPass(c, latest, affectedAt) {
			superseded[checkReference(c, c.ToolCallID)] = true
		}
	}
	if len(superseded) == 0 {
		return checks
	}
	out := make([]Check, 0, len(checks))
	for _, c := range checks {
		if !superseded[checkReference(c, c.ToolCallID)] {
			out = append(out, c)
		}
	}
	return out
}

func sameStepAssessmentObligation(first, check Check, explicitRefs bool) bool {
	if check.Binding.StepID != first.Binding.StepID || !sameAssessmentSource(check.Source, first.Source) {
		return false
	}
	if first.Binding.StepID == "" && check.CWD != first.CWD {
		return false
	}
	if check.Binding.Criterion == first.Binding.Criterion && check.Binding.Target == first.Binding.Target {
		return true
	}
	if first.Binding.StepID == "" {
		return false
	}
	if explicitRefs || check.Binding.Target == first.Binding.Target {
		return true
	}
	return first.Binding.Target == first.Binding.StepID || check.Binding.Target == check.Binding.StepID
}

func assessmentReference(check Check, id string) string {
	key := checkReference(check, id)
	if check.Observation != nil && check.Binding != nil {
		key += "\x00" + check.Binding.StepID
	}
	return key
}
