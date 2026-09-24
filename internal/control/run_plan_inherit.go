package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// EnsureInheritedRunPlan imports one exact parent's plan before Main starts.
// An already established child plan wins on retry; inheritance never rewrites
// work that Main has since planned or executed.
func (s *Store) EnsureInheritedRunPlan(ctx context.Context, tenantID, childRunID string) (*RunPlanProjection, error) {
	if s == nil || s.db == nil || strings.TrimSpace(childRunID) == "" {
		return nil, fmt.Errorf("continuation plan store and child run are required")
	}
	tenant := normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
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
		return nil, fmt.Errorf("run %s has no exact continuation parent", childRunID)
	}
	if err := tx.QueryRowContext(ctx, `SELECT person_id FROM runs WHERE tenant_id=? AND id=?`, tenant, parentRunID).
		Scan(&parentPersonID); err != nil {
		return nil, err
	}
	if childPersonID != parentPersonID {
		return nil, fmt.Errorf("continuation parent belongs to another person")
	}
	projection, err := s.inheritRunPlanTx(ctx, tx, tenant, childRunID, parentRunID, false)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return projection, nil
}

func (s *Store) inheritRunPlanTx(ctx context.Context, tx *sql.Tx, tenant, childRunID, parentRunID string, replace bool) (*RunPlanProjection, error) {
	current, err := latestRunPlanTx(ctx, tx, tenant, childRunID)
	if err != nil {
		return nil, err
	}
	if current != nil && !replace {
		return &RunPlanProjection{Plan: *current}, nil
	}
	parent, err := latestRunPlanTx(ctx, tx, tenant, parentRunID)
	if err != nil {
		return nil, err
	}
	if parent == nil || len(parent.Steps) == 0 {
		if !replace || current == nil {
			return nil, nil
		}
		// A corrected claim cannot leave the abandoned parent's Plan attached
		// to this Run. Publish an empty snapshot while retaining old versions as
		// audit history; Main can then plan against the corrected parent.
		reset, err := s.syncRunPlanTx(ctx, tx, tenant, childRunID, "Continuation parent changed; no plan to inherit", nil, nil, true)
		if err != nil {
			return nil, err
		}
		return &reset, nil
	}
	steps := make([]RunPlanStepInput, 0, len(parent.Steps))
	for _, source := range parent.Steps {
		steps = append(steps, RunPlanStepInput{
			Step: source.Step, Status: source.Status, SuccessCriteria: source.SuccessCriteria,
			VerificationRequired: source.VerificationRequired, RelatedTaskID: source.RelatedTaskID,
			WorkUnit: source.WorkUnit,
		})
	}
	projection, err := s.syncRunPlanTx(ctx, tx, tenant, childRunID, parent.Explanation, steps, parent, replace)
	if err != nil {
		return nil, err
	}
	return &projection, nil
}

// A continued Step retains its logical id, but the coarser Work Unit is a
// child-Run execution object with a new id. A model may echo the parent id
// from its prior turn. Translate only an id proven to be that exact step's
// source Work Unit; arbitrary stale ids remain invalid.
func translateInheritedWorkUnitIDsTx(ctx context.Context, tx *sql.Tx, tenant, childRunID string, input []RunPlanStepInput, previous *RunPlan) ([]RunPlanStepInput, error) {
	if previous == nil || len(input) == 0 {
		return input, nil
	}
	byStepID := make(map[string]RunPlanStep, len(previous.Steps))
	needsTranslation := false
	for _, step := range previous.Steps {
		byStepID[step.StepID] = step
	}
	for _, item := range input {
		if old, ok := byStepID[item.StepID]; ok && item.WorkUnitID != "" &&
			old.SourceStepID != "" && old.SourcePlanVersion > 0 && item.WorkUnitID != old.WorkUnitID {
			needsTranslation = true
			break
		}
	}
	if !needsTranslation {
		return input, nil
	}
	var parentRunID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(resumes_run_id,'') FROM runs WHERE tenant_id=? AND id=?`, tenant, childRunID).Scan(&parentRunID); err != nil {
		return nil, err
	}
	if parentRunID == "" {
		return input, nil
	}
	translated := append([]RunPlanStepInput(nil), input...)
	for i := range translated {
		item := &translated[i]
		if item.StepID == "" || item.WorkUnitID == "" {
			continue
		}
		old, ok := byStepID[item.StepID]
		if !ok || old.SourceStepID == "" || old.SourcePlanVersion < 1 {
			continue
		}
		var childWorkUnitID, sourceWorkUnitID string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(work_unit_id,'') FROM run_plan_steps
			WHERE tenant_id=? AND run_id=? AND plan_version=? AND step_id=?`, tenant, childRunID, previous.Version, item.StepID).
			Scan(&childWorkUnitID); err != nil {
			return nil, err
		}
		if item.WorkUnitID == childWorkUnitID {
			continue
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(work_unit_id,'') FROM run_plan_steps
			WHERE tenant_id=? AND run_id=? AND plan_version=? AND step_id=?`, tenant, parentRunID, old.SourcePlanVersion, old.SourceStepID).
			Scan(&sourceWorkUnitID); err != nil {
			return nil, err
		}
		if item.WorkUnitID == sourceWorkUnitID && sourceWorkUnitID != "" {
			item.WorkUnitID = childWorkUnitID
		}
	}
	return translated, nil
}
