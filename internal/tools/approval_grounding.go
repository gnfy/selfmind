package tools

import (
	"fmt"
	"strconv"
	"strings"
)

// RestrictionEvidence is a citation, not a runtime interpretation of language.
// The judge decides applicability; the runtime checks the quoted source exists.
type RestrictionEvidence struct {
	Source    string `json:"source"`
	Quote     string `json:"quote"`
	AppliesTo string `json:"applies_to"`
}

const groundedApprovalContract = `For a deny decision also include "restriction":{"source":"person_asked|person_added:N|authorization_evidence:N|assistant_offered","quote":"exact short quotation","applies_to":"actual effect forbidden by that quotation"}. Indices are zero-based. Cite a clear applicable restriction, not merely missing command wording. Missing authorization or uncertain applicability means escalate. An accepted goal includes necessary observation and verification unless a restriction actually excludes their effects. Available host/network capabilities alone do not establish those effects. Safety-floor prohibitions are already enforced independently; uncertainty about an additional prohibition means escalate. For approve or escalate, restriction may be omitted.`

func validateRestrictionCitation(a TriageAssessment, intent RunIntentSnapshot) error {
	if a.Outcome != "deny" {
		return nil
	}
	r := a.Restriction
	if r == nil || strings.TrimSpace(r.Quote) == "" || strings.TrimSpace(r.AppliesTo) == "" || len(r.Quote) > 2000 || len(r.AppliesTo) > 1000 {
		return fmt.Errorf("denial needs an applicable restriction and its attributed quotation")
	}
	source := ""
	switch r.Source {
	case "person_asked":
		if intent.UserAuthored() {
			source = intent.RawUserText
		}
	case "assistant_offered":
		if intent.UserAuthored() && strings.TrimSpace(intent.RawUserText) != "" {
			source = intent.PriorAssistantOffer
		}
	default:
		parts := strings.Split(r.Source, ":")
		if len(parts) == 2 {
			i, err := strconv.Atoi(parts[1])
			if err == nil && i >= 0 {
				switch parts[0] {
				case "person_added":
					if i < len(intent.AddedRequirements) {
						source = intent.AddedRequirements[i]
					}
				case "authorization_evidence":
					if i < len(intent.AuthorizationEvidence) {
						source = intent.AuthorizationEvidence[i].UserText
					}
				}
			}
		}
	}
	if source == "" || !strings.Contains(source, r.Quote) {
		return fmt.Errorf("denial quotation is not present in the attributed human evidence")
	}
	return nil
}
