package control

import (
	"context"
	"database/sql"

	"selfmind/internal/verification"
)

// A step selector is not an adoption decision. Only a completed accepted Plan
// step already carrying explicit adoption can supply an historical observation.
// Current checks keep precedence; old naked ids never use this path.
func stepAssessmentChecksTx(ctx context.Context, tx *sql.Tx, tenant, runID string, plan *RunPlan, checks []verification.Check, findings []verification.Assessment) ([]verification.Check, error) {
	needed := map[string]bool{}
	for _, a := range findings {
		if a.StepID != "" && (a.Version == 0 || a.Version == 2) {
			needed[a.StepID] = true
		}
	}
	if plan == nil || len(needed) == 0 {
		return checks, nil
	}
	for _, step := range plan.Steps {
		if !needed[step.StepID] || !step.ReusePriorVerification || step.Status != "completed" || !step.VerificationRequired {
			continue
		}
		hasCurrent := false
		for _, check := range checks {
			if check.Binding != nil && check.Binding.StepID == step.StepID {
				hasCurrent = true
				break
			}
		}
		if hasCurrent {
			continue
		}
		adopted, err := priorVerificationReuseChecksTx(ctx, tx, tenant, runID, step)
		if err != nil {
			return nil, err
		}
		checks = append(checks, adopted...)
	}
	return checks, nil
}
