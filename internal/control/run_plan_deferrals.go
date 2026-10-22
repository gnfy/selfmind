package control

import (
	"fmt"
	"strings"
)

// VerificationDeferralReason explains only this proposed transition. A later
// read of the accepted Plan must not present a rejected candidate as committed.
func (step RunPlanStep) VerificationDeferralReason() string {
	return boundedPlanErrorText(step.verificationDeferralReason, 400)
}

func completionVerificationDeferrals(projection RunPlanProjection) error {
	if len(projection.VerificationDeferred) == 0 {
		return nil
	}
	var details []string
	for i, step := range projection.VerificationDeferred {
		if i == 8 {
			details = append(details, fmt.Sprintf("and %d more steps", len(projection.VerificationDeferred)-i))
			break
		}
		reason := step.VerificationDeferralReason()
		if reason == "" {
			reason = "required verification remains open; run verify with step_id=" + step.StepID + " before completing it"
		}
		details = append(details, fmt.Sprintf("step %s (%q): %s", step.StepID, boundedPlanErrorText(step.Step, 160), reason))
	}
	return &completionPreconditionError{"Final Plan candidate was not committed; the stored Plan is unchanged. Required verification reopened: " + strings.Join(details, " | ")}
}
