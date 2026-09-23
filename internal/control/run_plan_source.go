package control

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// InheritedPlanEvidence is historical evidence for Main to assess, not a
// child-Run verification verdict. External conditions and unrecorded changes
// can make an earlier successful observation insufficient today.
type InheritedPlanEvidence struct {
	StepID            string
	SourceRunID       string
	SourceStepID      string
	SourceStatus      string
	SourceCriterion   string
	CriterionChanged  bool
	PriorVerification string
	LatestCheck       string
	Target            string
	CheckedAt         time.Time
}

// ListInheritedPlanEvidence follows only the child Run's exact parent edge and
// runtime-imported step ids. It never matches steps by sequence or prose and
// never writes a new evidence event or changes the child's verification state.
func (s *Store) ListInheritedPlanEvidence(ctx context.Context, tenantID, childRunID string) ([]InheritedPlanEvidence, error) {
	if s == nil || s.db == nil || strings.TrimSpace(childRunID) == "" {
		return nil, nil
	}
	tenant := normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var parentRunID, childPersonID, parentPersonID string
	if err := tx.QueryRowContext(ctx, `SELECT resumes_run_id, person_id FROM runs WHERE tenant_id=? AND id=?`, tenant, childRunID).
		Scan(&parentRunID, &childPersonID); err != nil {
		return nil, err
	}
	if parentRunID == "" {
		return nil, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT person_id FROM runs WHERE tenant_id=? AND id=?`, tenant, parentRunID).
		Scan(&parentPersonID); err != nil {
		return nil, err
	}
	if childPersonID != parentPersonID {
		return nil, nil
	}
	childPlan, err := latestRunPlanTx(ctx, tx, tenant, childRunID)
	if err != nil || childPlan == nil {
		return nil, err
	}
	result := make([]InheritedPlanEvidence, 0, len(childPlan.Steps))
	for _, step := range childPlan.Steps {
		if len(result) >= 12 {
			break
		}
		if step.SourceStepID == "" || step.SourcePlanVersion == 0 {
			continue
		}
		entry := InheritedPlanEvidence{
			StepID: step.StepID, SourceRunID: parentRunID, SourceStepID: step.SourceStepID,
		}
		sourceRunID, sourceStepID, sourceVersion := parentRunID, step.SourceStepID, step.SourcePlanVersion
		for depth := 0; depth < 8 && sourceStepID != "" && sourceVersion > 0; depth++ {
			var sourceStatus, sourceCriterion, sourceWorkUnitID, nextStepID string
			var nextVersion int
			err := tx.QueryRowContext(ctx, `SELECT status, success_criteria, work_unit_id, source_step_id, source_plan_version FROM run_plan_steps
				WHERE tenant_id=? AND run_id=? AND plan_version=? AND step_id=?`,
				tenant, sourceRunID, sourceVersion, sourceStepID).
				Scan(&sourceStatus, &sourceCriterion, &sourceWorkUnitID, &nextStepID, &nextVersion)
			if err == sql.ErrNoRows {
				break
			}
			if err != nil {
				return nil, err
			}
			entry.SourceRunID, entry.SourceStepID = sourceRunID, sourceStepID
			entry.SourceStatus, entry.SourceCriterion = sourceStatus, sourceCriterion
			entry.CriterionChanged = entry.CriterionChanged || normalizeRunPlanText(sourceCriterion) != normalizeRunPlanText(step.SuccessCriteria)
			entry.PriorVerification = ""
			if sourceWorkUnitID != "" {
				err = tx.QueryRowContext(ctx, `SELECT verification_state FROM run_work_units
					WHERE identity_tenant_id=? AND run_id=? AND id=?`,
					tenant, sourceRunID, sourceWorkUnitID).Scan(&entry.PriorVerification)
				if err != nil && err != sql.ErrNoRows {
					return nil, err
				}
			}
			var checkedAt sql.NullInt64
			err = tx.QueryRowContext(ctx, `SELECT COALESCE(json_extract(payload_json,'$.evidence.status'),''),
				COALESCE(json_extract(payload_json,'$.evidence.command.binding.target'),''),
				json_extract(payload_json,'$.evidence.finished_at_unix_nano')
				FROM task_events WHERE run_id=? AND type='evidence.recorded'
				  AND json_extract(payload_json,'$.evidence.kind')='verification'
				  AND json_extract(payload_json,'$.evidence.command.binding.step_id')=?
				ORDER BY cursor DESC LIMIT 1`, sourceRunID, sourceStepID).
				Scan(&entry.LatestCheck, &entry.Target, &checkedAt)
			if err != nil && err != sql.ErrNoRows {
				return nil, err
			}
			if checkedAt.Valid {
				entry.CheckedAt = time.Unix(0, checkedAt.Int64)
				break
			}
			if nextStepID == "" || nextVersion == 0 {
				break
			}
			var nextRunID, nextPersonID string
			if err := tx.QueryRowContext(ctx, `SELECT resumes_run_id, person_id FROM runs WHERE tenant_id=? AND id=?`, tenant, sourceRunID).
				Scan(&nextRunID, &nextPersonID); err != nil {
				return nil, err
			}
			if nextRunID == "" || nextRunID == sourceRunID || nextPersonID != childPersonID {
				break
			}
			var nextParentPersonID string
			if err := tx.QueryRowContext(ctx, `SELECT person_id FROM runs WHERE tenant_id=? AND id=?`, tenant, nextRunID).
				Scan(&nextParentPersonID); err == sql.ErrNoRows {
				break
			} else if err != nil {
				return nil, err
			}
			if nextParentPersonID != childPersonID {
				break
			}
			sourceRunID, sourceStepID, sourceVersion = nextRunID, nextStepID, nextVersion
		}
		if entry.SourceStatus != "" {
			result = append(result, entry)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
