package control

import (
	"context"
	"database/sql"
	"fmt"

	"selfmind/internal/verification"
)

// AdoptedRunVerificationChecks projects only explicit adoption in the accepted
// Plan. It reuses completion's provenance and freshness validation, performs no
// writes and does not turn historical pass status into an adoption decision.
func (s *Store) AdoptedRunVerificationChecks(ctx context.Context, tenantID, runID string) ([]verification.Check, error) {
	tenant := normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	plan, err := latestRunPlanTx(ctx, tx, tenant, runID)
	if err != nil {
		return nil, err
	}
	var checks []verification.Check
	if plan != nil {
		for _, step := range plan.Steps {
			if !step.ReusePriorVerification || step.Status != "completed" || !step.VerificationRequired {
				continue
			}
			prior, err := priorVerificationReuseChecksTx(ctx, tx, tenant, runID, step)
			if err != nil {
				return nil, fmt.Errorf("adopted verification for step %s is unavailable: %w", step.StepID, err)
			}
			checks = append(checks, prior...)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return checks, nil
}
