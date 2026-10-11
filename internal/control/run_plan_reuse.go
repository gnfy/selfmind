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
		return reusePreconditionf("step has no exact source verification")
	}
	reason := strings.TrimSpace(step.ReuseReason)
	if reason == "" || len(reason) > 1000 {
		return reusePreconditionf("reuse reason must be present and at most 1000 bytes")
	}
	var sourceRunID, personID, workspaceID, rootsJSON string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(resumes_run_id,''), person_id, COALESCE(workspace_id,''), COALESCE(execution_roots_json,'[]')
		FROM runs WHERE tenant_id=? AND id=?`, tenant, childRunID).
		Scan(&sourceRunID, &personID, &workspaceID, &rootsJSON); err != nil {
		return err
	}
	if sourceRunID == "" {
		return reusePreconditionf("run has no exact continuation parent")
	}
	var currentChecks int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events WHERE run_id=? AND type='evidence.recorded'
		AND json_extract(payload_json,'$.evidence.kind')='verification'
		AND json_extract(payload_json,'$.evidence.command.binding.step_id')=?`, childRunID, step.StepID).
		Scan(&currentChecks); err != nil {
		return err
	}
	if currentChecks > 0 {
		return reusePreconditionf("this run has already checked the step; use its current result instead")
	}
	sourceStepID, sourceVersion := step.SourceStepID, step.SourcePlanVersion
	var exactReference *InheritedPlanEvidence
	if ref, ok, err := inheritedEvidenceReferenceTx(ctx, tx, childRunID, sourceRunID, step.StepID); err != nil {
		return err
	} else if ok {
		if ref.CriterionChanged || ref.ReuseBlockedReason != "" {
			return reusePreconditionf("source acceptance condition or inputs changed: %s", ref.ReuseBlockedReason)
		}
		if ref.CheckID != "" {
			exactReference = &ref
			sourceRunID, sourceStepID, sourceVersion = ref.SourceRunID, ref.SourceStepID, ref.SourcePlanVersion
		}
	}
	for depth := 0; depth < 8; depth++ {
		var sourcePersonID, sourceWorkspaceID, sourceRootsJSON, nextRunID string
		if err := tx.QueryRowContext(ctx, `SELECT person_id, COALESCE(workspace_id,''), COALESCE(execution_roots_json,'[]'), COALESCE(resumes_run_id,'')
			FROM runs WHERE tenant_id=? AND id=?`, tenant, sourceRunID).
			Scan(&sourcePersonID, &sourceWorkspaceID, &sourceRootsJSON, &nextRunID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return reusePreconditionf("exact source run is unavailable")
			}
			return err
		}
		if sourcePersonID != personID || sourceWorkspaceID != workspaceID || sourceRootsJSON != rootsJSON {
			return reusePreconditionf("source verification belongs to a different person or execution scope")
		}
		var status, sourceStep, criterion, workUnitID, nextStepID, priorReason string
		var nextVersion, reused int
		if err := tx.QueryRowContext(ctx, `SELECT status, step_text, success_criteria, work_unit_id, source_step_id,
			source_plan_version, prior_verification_reused, reuse_reason FROM run_plan_steps
			WHERE tenant_id=? AND run_id=? AND plan_version=? AND step_id=?`,
			tenant, sourceRunID, sourceVersion, sourceStepID).
			Scan(&status, &sourceStep, &criterion, &workUnitID, &nextStepID, &nextVersion, &reused, &priorReason); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return reusePreconditionf("exact source plan step is unavailable")
			}
			return err
		}
		if status == "cancelled" || normalizeRunPlanText(sourceStep) != normalizeRunPlanText(step.Step) ||
			normalizeRunPlanText(criterion) != normalizeRunPlanText(step.SuccessCriteria) {
			return reusePreconditionf("source step is cancelled or its target or acceptance criterion changed")
		}
		var verificationState string
		if err := tx.QueryRowContext(ctx, `SELECT verification_state FROM run_work_units
			WHERE identity_tenant_id=? AND run_id=? AND id=?`, tenant, sourceRunID, workUnitID).
			Scan(&verificationState); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return reusePreconditionf("source work unit has no verification record")
			}
			return err
		}
		if reused != 0 {
			if status != "completed" || verificationState != "passed" || strings.TrimSpace(priorReason) == "" || nextRunID == "" || nextStepID == "" || nextVersion < 1 {
				return reusePreconditionf("source reuse has no complete provenance chain")
			}
			sourceRunID, sourceStepID, sourceVersion = nextRunID, nextStepID, nextVersion
			continue
		}
		state, blockers, checks, err := sourceStepVerificationTx(ctx, tx, sourceRunID, sourceStepID, workUnitID)
		if err != nil {
			return err
		}
		if len(checks) == 0 {
			return reusePreconditionf("source verification has no bound observation for this step")
		}
		if state != "passed" {
			return reusePreconditionf("source step has unresolved verification (%s): %s", state, blockers)
		}
		if exactReference != nil {
			if _, err := inheritedSourceChecksTx(ctx, tx, *exactReference); err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				return reusePreconditionf("exact source observation is not effective in its work-unit window: %v", err)
			}
		}
		// Every effective check must stay fresh; a newer independent target
		// cannot mask changes to another observation's declared inputs.
		for _, check := range checks {
			if check.ToolCallID == "" || check.Status != "succeeded" || check.Binding.Target == "" || check.FinishedAt <= 0 {
				return reusePreconditionf("source verification has no successful, bound observation")
			}
			for _, id := range []string{sourceRunID, childRunID} {
				changed, err := verificationInputsChangedTx(ctx, tx, id, check)
				if err != nil {
					return err
				}
				if changed {
					return reusePreconditionf("recorded changes affect verification inputs; reopen this step as in_progress, run verify with its step_id, then mark it completed using the fresh evidence")
				}
			}
		}
		return nil
	}
	return reusePreconditionf("source verification chain exceeds the bounded lineage depth")
}

// Missing or stale proof is correctable; storage failures retain their own
// diagnostic identity rather than asking Main to repair a plan that is valid.
func reusePreconditionf(format string, args ...interface{}) error {
	return &completionPreconditionError{fmt.Sprintf(format, args...)}
}
