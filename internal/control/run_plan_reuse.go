package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"selfmind/internal/verification"
)

// validatePriorVerificationReuseTx checks the runtime-owned provenance of an
// explicit Main judgment. The reason is Main's freshness assessment; this
// method never guesses that an old external observation remains current.
func validatePriorVerificationReuseTx(ctx context.Context, tx *sql.Tx, tenant, childRunID string, step RunPlanStep) error {
	_, err := priorVerificationReuseChecksTx(ctx, tx, tenant, childRunID, step)
	return err
}

// Validation and outcome projection resolve the same exact, fresh source checks.
func priorVerificationReuseChecksTx(ctx context.Context, tx *sql.Tx, tenant, childRunID string, step RunPlanStep) ([]verification.Check, error) {
	if step.SourceStepID == "" || step.SourcePlanVersion < 1 {
		return nil, reusePreconditionf("step has no exact source verification")
	}
	reason := strings.TrimSpace(step.ReuseReason)
	if reason == "" || len(reason) > 1000 {
		return nil, reusePreconditionf("reuse reason must be present and at most 1000 bytes")
	}
	var sourceRunID, personID, workspaceID, rootsJSON string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(resumes_run_id,''), person_id, COALESCE(workspace_id,''), COALESCE(execution_roots_json,'[]')
		FROM runs WHERE tenant_id=? AND id=?`, tenant, childRunID).
		Scan(&sourceRunID, &personID, &workspaceID, &rootsJSON); err != nil {
		return nil, err
	}
	if sourceRunID == "" {
		return nil, reusePreconditionf("run has no exact continuation parent")
	}
	checked, err := runStepHasCheckTx(ctx, tx, childRunID, step.StepID)
	if err != nil {
		return nil, err
	}
	if checked {
		if step.ReuseReason == carriedVerificationReason {
			// The runtime's own carry is a default for a step nobody looked at
			// again. What this Run has since observed for the step is the
			// evidence, so the carry adopts nothing; only Main's explicit
			// adoption conflicts with a current check.
			return nil, nil
		}
		return nil, reusePreconditionf("this run has already checked the step; use its current result instead")
	}
	sourceStepID, sourceVersion := step.SourceStepID, step.SourcePlanVersion
	// Every run between the original observation and this one can have changed
	// what that observation read, not only its first and last.
	lineage := []string{sourceRunID}
	var exactReference *InheritedPlanEvidence
	if ref, ok, err := inheritedEvidenceReferenceTx(ctx, tx, childRunID, sourceRunID, step.StepID); err != nil {
		return nil, err
	} else if ok {
		if ref.CriterionChanged || ref.ReuseBlockedReason != "" {
			return nil, reusePreconditionf("source acceptance condition or inputs changed: %s", ref.ReuseBlockedReason)
		}
		if ref.CheckID != "" {
			exactReference = &ref
			between, err := continuationLineageTx(ctx, tx, tenant, sourceRunID, ref.SourceRunID, personID, workspaceID, rootsJSON)
			if err != nil {
				return nil, err
			}
			lineage = between
			sourceRunID, sourceStepID, sourceVersion = ref.SourceRunID, ref.SourceStepID, ref.SourcePlanVersion
		}
	}
	for depth := 0; depth < 8; depth++ {
		var sourcePersonID, sourceWorkspaceID, sourceRootsJSON, nextRunID string
		if err := tx.QueryRowContext(ctx, `SELECT person_id, COALESCE(workspace_id,''), COALESCE(execution_roots_json,'[]'), COALESCE(resumes_run_id,'')
			FROM runs WHERE tenant_id=? AND id=?`, tenant, sourceRunID).
			Scan(&sourcePersonID, &sourceWorkspaceID, &sourceRootsJSON, &nextRunID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, reusePreconditionf("exact source run is unavailable")
			}
			return nil, err
		}
		if sourcePersonID != personID || sourceWorkspaceID != workspaceID || sourceRootsJSON != rootsJSON {
			return nil, reusePreconditionf("source verification belongs to a different person or execution scope")
		}
		var status, sourceStep, criterion, workUnitID, nextStepID, priorReason string
		var nextVersion, reused int
		if err := tx.QueryRowContext(ctx, `SELECT status, step_text, success_criteria, work_unit_id, source_step_id,
			source_plan_version, prior_verification_reused, reuse_reason FROM run_plan_steps
			WHERE tenant_id=? AND run_id=? AND plan_version=? AND step_id=?`,
			tenant, sourceRunID, sourceVersion, sourceStepID).
			Scan(&status, &sourceStep, &criterion, &workUnitID, &nextStepID, &nextVersion, &reused, &priorReason); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, reusePreconditionf("exact source plan step is unavailable")
			}
			return nil, err
		}
		if status == "cancelled" || normalizeRunPlanText(sourceStep) != normalizeRunPlanText(step.Step) ||
			normalizeRunPlanText(criterion) != normalizeRunPlanText(step.SuccessCriteria) {
			return nil, reusePreconditionf("source step is cancelled or its target or acceptance criterion changed")
		}
		if blocker, err := sourceConditionBlockTx(ctx, tx, sourceRunID, sourceStepID, criterion); err != nil {
			return nil, err
		} else if blocker != "" {
			return nil, reusePreconditionf("%s", blocker)
		}
		var verificationState, unitStatus string
		if err := tx.QueryRowContext(ctx, `SELECT verification_state, status FROM run_work_units
			WHERE identity_tenant_id=? AND run_id=? AND id=?`, tenant, sourceRunID, workUnitID).
			Scan(&verificationState, &unitStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, reusePreconditionf("source work unit has no verification record")
			}
			return nil, err
		}
		if reused != 0 {
			// A work unit that is still open has no verdict yet (a Plan is one unit
			// unless Main splits it), so an adopted step in it stands on its own
			// chain: the original observation is validated below and every run on
			// the way is checked for changes to that observation's inputs. A unit
			// that finished must have finished passed.
			if status != "completed" || (workUnitTerminal(unitStatus) && verificationState != "passed") ||
				strings.TrimSpace(priorReason) == "" || nextRunID == "" || nextStepID == "" || nextVersion < 1 {
				return nil, reusePreconditionf("source reuse has no complete provenance chain")
			}
			// A continuation saves a direct reference to the observation it
			// inherited, so a chain of adopted steps is not walked hop by hop: an
			// exact continuation validates its carried steps before its own
			// reference exists, and its parent's is the nearest one. The reference
			// only shortens the walk; every run it skips is still checked for
			// changes and for its execution scope, and one that does not hold
			// falls back to the walk, which names the exact failure.
			if ref, ok, err := inheritedEvidenceReferenceTx(ctx, tx, sourceRunID, "", sourceStepID); err != nil {
				return nil, err
			} else if ok && ref.CheckID != "" && !ref.CriterionChanged && ref.ReuseBlockedReason == "" && ref.SourceRunID != sourceRunID {
				between, err := continuationLineageTx(ctx, tx, tenant, sourceRunID, ref.SourceRunID, personID, workspaceID, rootsJSON)
				if err != nil {
					return nil, err
				}
				exactReference = &ref
				lineage = append(lineage[:len(lineage)-1], between...)
				sourceRunID, sourceStepID, sourceVersion = ref.SourceRunID, ref.SourceStepID, ref.SourcePlanVersion
				continue
			}
			lineage = append(lineage, nextRunID)
			sourceRunID, sourceStepID, sourceVersion = nextRunID, nextStepID, nextVersion
			continue
		}
		state, blockers, checks, err := sourceStepVerificationTx(ctx, tx, sourceRunID, sourceStepID, workUnitID)
		if err != nil {
			return nil, err
		}
		if len(checks) == 0 {
			return nil, reusePreconditionf("source verification has no bound observation for this step")
		}
		if state != "passed" {
			return nil, reusePreconditionf("source step has unresolved verification (%s): %s", state, blockers)
		}
		if exactReference != nil {
			if _, err := inheritedSourceChecksTx(ctx, tx, *exactReference); err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
				return nil, reusePreconditionf("exact source observation is not effective in its work-unit window: %v", err)
			}
		}
		// Every effective check must stay fresh; a newer independent target
		// cannot mask changes to another observation's declared inputs.
		for _, check := range checks {
			if check.ToolCallID == "" || check.Status != "succeeded" || check.Binding.Target == "" || check.FinishedAt <= 0 {
				return nil, reusePreconditionf("source verification has no successful, bound observation")
			}
			for _, id := range append(append([]string(nil), lineage...), childRunID) {
				changed, err := verificationInputsChangedTx(ctx, tx, id, check)
				if err != nil {
					return nil, err
				}
				if changed {
					return nil, reusePreconditionf("recorded changes affect verification inputs; reopen this step as in_progress, run verify with its step_id, then mark it completed using the fresh evidence")
				}
			}
		}
		for i := range checks {
			binding := *checks[i].Binding
			binding.StepID = step.StepID
			checks[i].Binding = &binding
			checks[i].Source = &verification.CheckSource{
				RunID: sourceRunID, StepID: sourceStepID, PlanVersion: sourceVersion, Reason: reason,
			}
		}
		return checks, nil
	}
	return nil, reusePreconditionf("source verification chain exceeds the bounded lineage depth")
}

// runStepHasCheckTx reports whether this Run has recorded a verify check bound to
// the step, whatever its outcome.
func runStepHasCheckTx(ctx context.Context, tx *sql.Tx, runID, stepID string) (bool, error) {
	var checks int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_events WHERE run_id=? AND type='evidence.recorded'
		AND json_extract(payload_json,'$.evidence.kind')='verification'
		AND json_extract(payload_json,'$.evidence.command.binding.step_id')=?`, runID, stepID).Scan(&checks); err != nil {
		return false, err
	}
	return checks > 0, nil
}

// maxReuseLineage bounds how many runs a carried observation may span.
const maxReuseLineage = 32

// continuationLineageTx lists the runs from fromRunID (inclusive) along the exact
// continuation edges to toRunID (inclusive). Every run before toRunID must share
// the child's person and execution scope; toRunID is checked by the caller as
// the run holding the observation. A lineage that cannot be walked is a
// correctable precondition, never an assumption that nothing changed in between.
func continuationLineageTx(ctx context.Context, tx *sql.Tx, tenant, fromRunID, toRunID, personID, workspaceID, rootsJSON string) ([]string, error) {
	lineage := []string{fromRunID}
	for current := fromRunID; current != toRunID; {
		if len(lineage) > maxReuseLineage {
			return nil, reusePreconditionf("source verification chain exceeds the bounded lineage depth")
		}
		var next, runPersonID, runWorkspaceID, runRootsJSON string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(resumes_run_id,''), person_id, COALESCE(workspace_id,''), COALESCE(execution_roots_json,'[]')
			FROM runs WHERE tenant_id=? AND id=?`, tenant, current).Scan(&next, &runPersonID, &runWorkspaceID, &runRootsJSON); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, reusePreconditionf("exact source run is unavailable")
			}
			return nil, err
		}
		if runPersonID != personID || runWorkspaceID != workspaceID || runRootsJSON != rootsJSON {
			return nil, reusePreconditionf("source verification belongs to a different person or execution scope")
		}
		if next == "" {
			return nil, reusePreconditionf("source verification lineage is not an exact continuation chain")
		}
		lineage = append(lineage, next)
		current = next
	}
	return lineage, nil
}

// Missing or stale proof is correctable; storage failures retain their own
// diagnostic identity rather than asking Main to repair a plan that is valid.
func reusePreconditionf(format string, args ...interface{}) error {
	return &completionPreconditionError{fmt.Sprintf(format, args...)}
}
