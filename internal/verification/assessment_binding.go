package verification

import (
	"fmt"
	"sort"
)

// Same source means the same durable observation, not a same-looking call id.
// Reason is an explanation of adoption, not part of its evidence identity.
func sameAssessmentSource(a, b *CheckSource) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.RunID == b.RunID && a.StepID == b.StepID && a.PlanVersion == b.PlanVersion
}

// assessmentChecks resolves an explicit logical step or exact references.
// Historical checks can enter only through a runtime-validated adopted source;
// the old bare-reference path remains current-Run-only.
func assessmentChecks(a Assessment, input []Assessment, checks, allChecks []Check, current map[string]Check, requirements map[string]AssessmentRequirement) ([]Check, error) {
	if a.Version < 0 || a.Version > 2 {
		return nil, fmt.Errorf("unsupported condition assessment version %d", a.Version)
	}
	if a.Version == 0 && a.Source != nil {
		return nil, fmt.Errorf("condition evidence source is runtime-owned")
	}
	if a.Version == 0 && a.StepID != "" {
		var selected []Check
		for _, c := range Latest(checks) {
			if ValidBinding(c.Binding) && c.Binding.StepID == a.StepID {
				selected = append(selected, c)
			}
		}
		if len(selected) == 0 && len(a.EvidenceRefs) > 0 && len(a.EvidenceRefs) <= 8 {
			if _, stepFound := requirements[a.StepID]; stepFound {
				if resolved, ok := resolveCurrentStepEvidenceRefs(a.EvidenceRefs, allChecks, current); ok {
					selected = resolved
				}
			}
		}
		if len(selected) == 0 {
			return nil, fmt.Errorf("step %q has no associated observation; with finish_run.plan_patch, associate an existing confirmed observation using step_id and evidence_refs. Run verify only for missing executable proof; prior evidence requires explicit adoption", a.StepID)
		}
		selected = narrowStepChecksToCitedSubset(selected, a.Result, a.EvidenceRefs, input)
		if len(selected) > 8 {
			return nil, fmt.Errorf("step %q has more than 8 effective observations; use explicit current-Run evidence_refs for one condition", a.StepID)
		}
		if len(a.EvidenceRefs) > 0 && !compatibleStepEvidenceRefs(selected, a.EvidenceRefs, allChecks, current) {
			return nil, fmt.Errorf("choose step_id or evidence_refs for one finding, not both")
		}
		sort.Slice(selected, func(i, j int) bool {
			return checkReference(selected[i], selected[i].ToolCallID) < checkReference(selected[j], selected[j].ToolCallID)
		})
		return selected, nil
	}
	if len(a.EvidenceRefs) == 0 || len(a.EvidenceRefs) > 8 {
		return nil, fmt.Errorf("a finding needs step_id or 1–8 evidence_refs")
	}
	if a.Version == 0 && a.StepID == "" && len(a.EvidenceRefs) > 1 {
		if resolved, ok := resolveUnsteppedMixedEvidenceRefs(a.EvidenceRefs, allChecks, current); ok {
			return resolved, nil
		}
	}
	if a.Version == 2 && (a.Source == nil || a.StepID == "" || a.Source.RunID == "" || a.Source.StepID == "" || a.Source.PlanVersion < 1) {
		return nil, fmt.Errorf("recorded adopted assessment has no supported source identity")
	}
	if a.Version != 2 && a.Source != nil {
		return nil, fmt.Errorf("recorded condition assessment has an unsupported evidence source")
	}
	selected := make([]Check, 0, len(a.EvidenceRefs))
	for _, ref := range a.EvidenceRefs {
		if a.Version == 2 {
			found := false
			for _, c := range Latest(checks) {
				if c.ToolCallID == ref && c.Binding != nil && c.Binding.StepID == a.StepID && sameAssessmentSource(c.Source, a.Source) {
					if found {
						return nil, fmt.Errorf("adopted verification reference %q is ambiguous", ref)
					}
					selected = append(selected, c)
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("recorded adopted verification %q is no longer available for step %s", ref, a.StepID)
			}
		} else if a.Version == 1 && a.StepID != "" {
			found := false
			for _, c := range Latest(checks) {
				if c.Source == nil && c.ToolCallID == ref && ValidBinding(c.Binding) && c.Binding.StepID == a.StepID {
					if found {
						return nil, fmt.Errorf("recorded observation %q is ambiguous for step %s", ref, a.StepID)
					}
					selected = append(selected, c)
					found = true
				}
			}
			if !found {
				return nil, &EvidenceUnavailableError{Ref: ref}
			}
		} else {
			c, ok := current[ref]
			if !ok || !ValidBinding(c.Binding) {
				return nil, &EvidenceUnavailableError{Ref: ref}
			}
			selected = append(selected, c)
		}
	}
	return selected, nil
}

func narrowStepChecksToCitedSubset(selected []Check, result string, refs []string, input []Assessment) []Check {
	if len(selected) <= 1 || len(refs) == 0 {
		return selected
	}
	citedHere := make(map[string]bool, len(refs))
	for _, ref := range refs {
		citedHere[ref] = true
	}
	var subset []Check
	for _, c := range selected {
		if c.ToolCallID != "" && citedHere[c.ToolCallID] {
			subset = append(subset, c)
		}
	}
	if len(subset) == 0 || len(subset) == len(selected) {
		return selected
	}
	citedAcrossSameResult := make(map[string]bool, len(selected))
	for _, peer := range input {
		if peer.Result != result {
			continue
		}
		for _, ref := range peer.EvidenceRefs {
			citedAcrossSameResult[ref] = true
		}
	}
	for _, c := range selected {
		if c.ToolCallID == "" || !citedAcrossSameResult[c.ToolCallID] {
			return selected
		}
	}
	return subset
}

func resolveCurrentStepEvidenceRefs(refs []string, checks []Check, current map[string]Check) ([]Check, bool) {
	if len(refs) == 0 || len(refs) > 8 {
		return nil, false
	}
	var resolved []Check
	seen := make(map[string]bool, len(refs))
	var missing []string
	for _, ref := range refs {
		if c, ok := current[ref]; ok && ValidBinding(c.Binding) {
			if !seen[c.ToolCallID] {
				seen[c.ToolCallID] = true
				resolved = append(resolved, c)
			}
			continue
		}
		missing = append(missing, ref)
	}
	if len(resolved) == 0 {
		return nil, false
	}
	first := resolved[0]
	for _, c := range resolved[1:] {
		if c.Binding.StepID != first.Binding.StepID || !sameAssessmentSource(c.Source, first.Source) {
			return nil, false
		}
		if first.Binding.StepID == "" && (c.CWD != first.CWD || c.Binding.Criterion != first.Binding.Criterion || c.Binding.Target != first.Binding.Target) {
			return nil, false
		}
	}
	for _, ref := range missing {
		if !isEarlierSameStepAttempt(ref, first, checks) {
			return nil, false
		}
	}
	return resolved, true
}

func isEarlierSameStepAttempt(ref string, active Check, checks []Check) bool {
	if ref == "" || !ValidBinding(active.Binding) || active.Binding.Version != 3 || active.Source != nil {
		return false
	}
	for _, c := range checks {
		if c.ToolCallID == ref && c.Source == nil && ValidBinding(c.Binding) && c.Binding.Version == 3 &&
			c.Binding.StepID == active.Binding.StepID &&
			(c.Binding.Target == active.Binding.Target || active.Binding.StepID != "" && (c.Binding.Target == c.Binding.StepID || active.Binding.Target == active.Binding.StepID)) &&
			(active.Binding.StepID != "" || (c.Binding.Criterion == active.Binding.Criterion && c.CWD == active.CWD)) {
			return true
		}
	}
	return false
}

func compatibleStepEvidenceRefs(selected []Check, refs []string, checks []Check, current map[string]Check) bool {
	if len(selected) == 0 || len(refs) == 0 || len(refs) > 8 {
		return false
	}
	first := selected[0]
	if !ValidBinding(first.Binding) {
		return false
	}
	byCallID := make(map[string]bool, len(selected))
	uniform := true
	for _, c := range selected {
		if !ValidBinding(c.Binding) || c.Binding.StepID != first.Binding.StepID ||
			(first.Binding.StepID == "" && c.CWD != first.CWD) || !sameAssessmentSource(c.Source, first.Source) {
			return false
		}
		if !sameStepAssessmentObligation(c, first, false) {
			uniform = false
		}
		if c.ToolCallID != "" {
			byCallID[c.ToolCallID] = true
		}
	}
	refSet := make(map[string]bool, len(refs))
	matchedStepRef := false
	for _, ref := range refs {
		refSet[ref] = true
		if byCallID[ref] {
			matchedStepRef = true
			continue
		}
		if isEarlierSameStepAttempt(ref, first, checks) {
			matchedStepRef = true
			continue
		}
		if c, ok := current[ref]; ok && ValidBinding(c.Binding) {
			if first.Source != nil {
				matchedStepRef = true
			}
			continue
		}
		return false
	}
	if !matchedStepRef {
		return false
	}
	if !uniform {
		for id := range byCallID {
			if !refSet[id] {
				return false
			}
		}
	}
	return true
}

func isSecondaryNoteOnAssessedStep(index int, a Assessment, input []Assessment, checks []Check, current map[string]Check) bool {
	if len(input) <= 1 || a.Version != 0 || a.StepID != "" || len(a.EvidenceRefs) > 8 {
		return false
	}
	if len(a.EvidenceRefs) == 0 {
		if a.Result != "met" && a.Result != "unknown" && a.Result != "not_met" {
			return false
		}
		for j, peer := range input {
			if j != index && peer.Result == "met" && (peer.StepID != "" || peerResolvesToAnyStep(peer, checks, current)) {
				return true
			}
		}
		return false
	}
	for _, ref := range a.EvidenceRefs {
		stepID := resolveRefStepID(ref, checks, current)
		if stepID == "" {
			return false
		}
		coveredByStepPeer := false
		for j, peer := range input {
			if j == index || peer.Result != "met" {
				continue
			}
			if peer.StepID == stepID || (j < index && peer.StepID == "" && peerResolvesToStep(peer, stepID, checks, current)) {
				coveredByStepPeer = true
				break
			}
		}
		if !coveredByStepPeer {
			return false
		}
	}
	return true
}

func resolveRefStepID(ref string, checks []Check, current map[string]Check) string {
	if c, ok := current[ref]; ok && ValidBinding(c.Binding) {
		return c.Binding.StepID
	}
	for _, c := range checks {
		if c.ToolCallID == ref && ValidBinding(c.Binding) && c.Binding.StepID != "" {
			return c.Binding.StepID
		}
	}
	return ""
}

func peerResolvesToAnyStep(peer Assessment, checks []Check, current map[string]Check) bool {
	if len(peer.EvidenceRefs) == 0 {
		return false
	}
	for _, ref := range peer.EvidenceRefs {
		if resolveRefStepID(ref, checks, current) != "" {
			return true
		}
	}
	return false
}

func peerResolvesToStep(peer Assessment, stepID string, checks []Check, current map[string]Check) bool {
	if stepID == "" || len(peer.EvidenceRefs) == 0 {
		return false
	}
	matched := false
	for _, ref := range peer.EvidenceRefs {
		s := resolveRefStepID(ref, checks, current)
		if s == "" {
			continue
		}
		if s != stepID {
			return false
		}
		matched = true
	}
	return matched
}

func resolveUnsteppedMixedEvidenceRefs(refs []string, allChecks []Check, current map[string]Check) ([]Check, bool) {
	var kept []Check
	seen := make(map[string]bool, len(refs))
	var missing []string
	for _, ref := range refs {
		if seen[ref] {
			return nil, false
		}
		seen[ref] = true
		if c, ok := current[ref]; ok && ValidBinding(c.Binding) {
			kept = append(kept, c)
			continue
		}
		missing = append(missing, ref)
	}
	if len(kept) == 0 || len(missing) == 0 {
		return nil, false
	}
	first := kept[0]
	if first.Binding.StepID == "" {
		return nil, false
	}
	for _, c := range kept[1:] {
		if c.Binding.StepID != first.Binding.StepID || !sameAssessmentSource(c.Source, first.Source) {
			return nil, false
		}
	}
	for _, ref := range missing {
		if isEarlierSameStepAttempt(ref, first, allChecks) {
			continue
		}
		for _, c := range allChecks {
			if c.Source == nil && c.ToolCallID == ref {
				return nil, false
			}
		}
	}
	return kept, true
}

func isSubsetStringSlice(sub, super []string) bool {
	if len(sub) == 0 || len(sub) > len(super) {
		return false
	}
	set := make(map[string]bool, len(super))
	for _, s := range super {
		set[s] = true
	}
	for _, s := range sub {
		if !set[s] {
			return false
		}
	}
	return true
}

func isRedundantSubsetOfRecordedCondition(result string, source *CheckSource, refs []string, recorded []Assessment) bool {
	if len(refs) == 0 {
		return false
	}
	for _, prev := range recorded {
		if prev.Result == result && sameAssessmentSource(prev.Source, source) && isSubsetStringSlice(refs, prev.EvidenceRefs) {
			return true
		}
	}
	return false
}

func isRedundantStepSummary(index int, a Assessment, input []Assessment, checks []Check, current map[string]Check, requirements map[string]AssessmentRequirement) bool {
	if len(input) <= 1 || a.Version != 0 || a.Result != "met" || len(a.EvidenceRefs) > 8 {
		return false
	}
	if a.StepID == "" && len(a.EvidenceRefs) == 0 {
		if len(current) == 0 {
			return false
		}
		for id, c := range current {
			if !ValidBinding(c.Binding) || !peerCoversCheckOrRef(index, id, c, input) {
				return false
			}
		}
		return true
	}
	if len(a.EvidenceRefs) == 0 {
		return false
	}
	if a.StepID != "" {
		if _, ok := requirements[a.StepID]; !ok {
			return false
		}
		for _, c := range Latest(checks) {
			if ValidBinding(c.Binding) && c.Binding.StepID == a.StepID {
				return false
			}
		}
	}
	validRefs := 0
	for _, ref := range a.EvidenceRefs {
		c, ok := current[ref]
		if !ok || !ValidBinding(c.Binding) {
			var activeMatch *Check
			for _, latestCheck := range Latest(checks) {
				if isEarlierSameStepAttempt(ref, latestCheck, checks) {
					copy := latestCheck
					activeMatch = &copy
					break
				}
			}
			if activeMatch == nil {
				return false
			}
			c = *activeMatch
		}
		validRefs++
		if !peerCoversCheckOrRef(index, ref, c, input) && !peerCoversCheckOrRef(index, c.ToolCallID, c, input) {
			return false
		}
	}
	return validRefs > 0
}

func peerCoversCheckOrRef(index int, ref string, c Check, input []Assessment) bool {
	for j, peer := range input {
		if j == index || peer.Result != "met" {
			continue
		}
		if peer.StepID != "" && ValidBinding(c.Binding) && peer.StepID == c.Binding.StepID {
			return true
		}
		for _, peerRef := range peer.EvidenceRefs {
			if peerRef == ref {
				return true
			}
		}
	}
	return false
}
