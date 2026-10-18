package control

import (
	"context"
	"database/sql"

	"selfmind/internal/verification"
)

// RunPlanSnapshot is one read transaction over the accepted Plan and its
// applicable evidence. Preview failure never erases an accepted Plan.
type RunPlanSnapshot struct {
	Plan         *RunPlan
	ClosureFacts verification.ClosureFacts
	factsErr     error
}

// ReadRunPlanSnapshot scopes current execution state to the exact person/Run.
// It is a read projection, never a new writable state or a completion verdict.
func (s *Store) ReadRunPlanSnapshot(ctx context.Context, tenant, person, runID string) (RunPlanSnapshot, error) {
	if person == "" {
		return RunPlanSnapshot{}, sql.ErrNoRows
	}
	return s.readRunPlanSnapshot(ctx, tenant, person, runID)
}

// RunClosureFacts retains the strict preview API used by accepted update
// receipts. Snapshot consumers may still display the Plan when only facts fail.
func (s *Store) RunClosureFacts(ctx context.Context, tenant, runID string) (verification.ClosureFacts, error) {
	snapshot, err := s.readRunPlanSnapshot(ctx, tenant, "", runID)
	if err != nil {
		return verification.ClosureFacts{}, err
	}
	return snapshot.ClosureFacts, snapshot.factsErr
}

func (s *Store) readRunPlanSnapshot(ctx context.Context, tenant, person, runID string) (RunPlanSnapshot, error) {
	var snapshot RunPlanSnapshot
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback()
	tenant = normalizeTenant(tenant)
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT person_id FROM runs WHERE tenant_id=? AND id=?`, tenant, runID).Scan(&owner); err != nil {
		return snapshot, err
	}
	if person != "" && person != owner {
		return snapshot, sql.ErrNoRows
	}
	snapshot.Plan, err = latestRunPlanTx(ctx, tx, tenant, runID)
	if err != nil {
		return RunPlanSnapshot{}, err
	}
	snapshot.ClosureFacts, snapshot.factsErr = runClosureFactsTx(ctx, tx, tenant, runID, snapshot.Plan)
	if snapshot.factsErr != nil {
		snapshot.ClosureFacts = verification.ClosureFacts{Available: false}
	}
	if err := tx.Commit(); err != nil {
		return RunPlanSnapshot{}, err
	}
	return snapshot, nil
}

func runClosureFactsTx(ctx context.Context, tx *sql.Tx, tenant, runID string, plan *RunPlan) (verification.ClosureFacts, error) {
	out := verification.ClosureFacts{Evidence: []verification.CiteableEvidence{}}
	checks, mutations, _, err := conditionEvidenceTx(ctx, tx, tenant, runID, plan)
	if err != nil {
		return out, err
	}
	evidence, err := verification.CiteableChecks(checks, mutations)
	if err != nil {
		return out, err
	}
	if plan != nil {
		out.PlanVersion = plan.Version
		for _, step := range plan.Steps {
			if step.Status == "completed" || step.Status == "cancelled" {
				continue
			}
			if len(out.OpenStepIDs) < 8 {
				out.OpenStepIDs = append(out.OpenStepIDs, step.StepID)
			} else {
				out.OpenStepsOmitted++
			}
		}
	}
	for _, item := range evidence {
		// Preserve ids exactly; an oversized identity is omitted, never shortened
		// into a different reference. Target is only a bounded display hint.
		if len(out.Evidence) >= 8 || len(item.ID) > 256 || len(item.StepID) > 256 {
			out.EvidenceOmitted++
			continue
		}
		if target := []rune(item.Target); len(target) > 240 {
			item.Target = string(target[:240]) + "…"
		}
		out.Evidence = append(out.Evidence, item)
	}
	out.Available = true
	return out, nil
}
