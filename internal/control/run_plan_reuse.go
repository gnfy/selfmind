package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// validatePriorVerificationReuseTx checks the runtime-owned provenance of an
// explicit Main judgment. The reason is Main's freshness assessment; this
// method never guesses that an old external observation remains current.
func validatePriorVerificationReuseTx(ctx context.Context, tx *sql.Tx, tenant, childRunID string, step RunPlanStep) error {
	if step.SourceStepID == "" || step.SourcePlanVersion < 1 {
		return fmt.Errorf("step has no exact source verification")
	}
	reason := strings.TrimSpace(step.ReuseReason)
	if reason == "" || len(reason) > 1000 {
		return fmt.Errorf("reuse reason must be present and at most 1000 bytes")
	}
	var sourceRunID, personID, workspaceID, rootsJSON string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(resumes_run_id,''), person_id, COALESCE(workspace_id,''), COALESCE(execution_roots_json,'[]')
		FROM runs WHERE tenant_id=? AND id=?`, tenant, childRunID).
		Scan(&sourceRunID, &personID, &workspaceID, &rootsJSON); err != nil {
		return err
	}
	if sourceRunID == "" {
		return fmt.Errorf("run has no exact continuation parent")
	}
	var laterEffects int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_ledger
		WHERE tenant_id=? AND run_id=? AND strategy='mutate' AND status<>'prepared'`, tenant, childRunID).
		Scan(&laterEffects); err != nil {
		return err
	}
	if laterEffects > 0 {
		return fmt.Errorf("this run has already dispatched an effect; observe the current state instead")
	}
	var mutations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events WHERE run_id=? AND type='evidence.recorded'
		AND json_extract(payload_json,'$.evidence.kind')='mutation'`, childRunID).Scan(&mutations); err != nil {
		return err
	}
	if mutations > 0 {
		return fmt.Errorf("this run recorded a mutation; observe the current state instead")
	}
	var currentChecks int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events WHERE run_id=? AND type='evidence.recorded'
		AND json_extract(payload_json,'$.evidence.kind')='verification'
		AND json_extract(payload_json,'$.evidence.command.binding.step_id')=?`, childRunID, step.StepID).
		Scan(&currentChecks); err != nil {
		return err
	}
	if currentChecks > 0 {
		return fmt.Errorf("this run has already checked the step; use its current result instead")
	}
	sourceStepID, sourceVersion := step.SourceStepID, step.SourcePlanVersion
	for depth := 0; depth < 8; depth++ {
		var sourcePersonID, sourceWorkspaceID, sourceRootsJSON, nextRunID string
		if err := tx.QueryRowContext(ctx, `SELECT person_id, COALESCE(workspace_id,''), COALESCE(execution_roots_json,'[]'), COALESCE(resumes_run_id,'')
			FROM runs WHERE tenant_id=? AND id=?`, tenant, sourceRunID).
			Scan(&sourcePersonID, &sourceWorkspaceID, &sourceRootsJSON, &nextRunID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("exact source run is unavailable")
			}
			return err
		}
		if sourcePersonID != personID || sourceWorkspaceID != workspaceID || sourceRootsJSON != rootsJSON {
			return fmt.Errorf("source verification belongs to a different person or execution scope")
		}
		var status, sourceStep, criterion, workUnitID, nextStepID, priorReason string
		var nextVersion, reused int
		if err := tx.QueryRowContext(ctx, `SELECT status, step_text, success_criteria, work_unit_id, source_step_id,
			source_plan_version, prior_verification_reused, reuse_reason FROM run_plan_steps
			WHERE tenant_id=? AND run_id=? AND plan_version=? AND step_id=?`,
			tenant, sourceRunID, sourceVersion, sourceStepID).
			Scan(&status, &sourceStep, &criterion, &workUnitID, &nextStepID, &nextVersion, &reused, &priorReason); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("exact source plan step is unavailable")
			}
			return err
		}
		if status != "completed" || normalizeRunPlanText(sourceStep) != normalizeRunPlanText(step.Step) ||
			normalizeRunPlanText(criterion) != normalizeRunPlanText(step.SuccessCriteria) {
			return fmt.Errorf("source step is incomplete or its target or acceptance criterion changed")
		}
		var verificationState string
		if err := tx.QueryRowContext(ctx, `SELECT verification_state FROM run_work_units
			WHERE identity_tenant_id=? AND run_id=? AND id=?`, tenant, sourceRunID, workUnitID).
			Scan(&verificationState); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("source work unit has no verification record")
			}
			return err
		}
		if verificationState != "passed" {
			return fmt.Errorf("source work unit has no passed verification")
		}
		if reused != 0 {
			if strings.TrimSpace(priorReason) == "" || nextRunID == "" || nextStepID == "" || nextVersion < 1 {
				return fmt.Errorf("source reuse has no complete provenance chain")
			}
			sourceRunID, sourceStepID, sourceVersion = nextRunID, nextStepID, nextVersion
			continue
		}
		var checkStatus, target string
		var checkedAt sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(json_extract(payload_json,'$.evidence.status'),''),
			COALESCE(json_extract(payload_json,'$.evidence.command.binding.target'),''),
			json_extract(payload_json,'$.evidence.finished_at_unix_nano')
			FROM task_events WHERE run_id=? AND type='evidence.recorded'
			  AND json_extract(payload_json,'$.evidence.kind')='verification'
			  AND json_extract(payload_json,'$.evidence.command.binding.step_id')=?
			ORDER BY cursor DESC LIMIT 1`, sourceRunID, sourceStepID).
			Scan(&checkStatus, &target, &checkedAt)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("source verification has no bound observation for this step")
			}
			return err
		}
		if checkStatus != "succeeded" || target == "" || !checkedAt.Valid || checkedAt.Int64 <= 0 {
			return fmt.Errorf("source verification has no successful, bound observation")
		}
		return nil
	}
	return fmt.Errorf("source verification chain exceeds the bounded lineage depth")
}
