package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// PlanVersionChangedError refuses a patch based on a different accepted Plan.
// The expected version comes from the runtime, never a model-supplied argument.
type PlanVersionChangedError struct{ Expected, Current int }

func (e *PlanVersionChangedError) Error() string {
	if e.Current == 0 {
		return "cannot patch Plan: no accepted Plan exists yet (current version 0); submit the initial complete Plan with mode=\"snapshot\" first"
	}
	return fmt.Sprintf("accepted Plan changed: execution snapshot version %d, current version %d; inspect the current Plan and submit a complete snapshot before patching again", e.Expected, e.Current)
}
func (e *PlanVersionChangedError) CurrentPlanVersion() int { return e.Current }

// PatchRunPlan expands an explicitly selected patch inside the same transaction
// that validates and commits the complete Plan. Omission preserves an obligation;
// this path has no positional matching, guessed status or implicit deletion.
func (s *Store) PatchRunPlan(ctx context.Context, tenant, runID string, expected int, explanation string, changes []RunPlanStepInput) (RunPlanProjection, error) {
	if s == nil || s.db == nil {
		return RunPlanProjection{}, fmt.Errorf("control store is unavailable")
	}
	if strings.TrimSpace(runID) == "" || len(changes) == 0 {
		return RunPlanProjection{}, fmt.Errorf("run id and at least one changed step are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunPlanProjection{}, err
	}
	defer tx.Rollback()
	tenant = normalizeTenant(tenant)
	projection, err := s.patchRunPlanTx(ctx, tx, tenant, runID, expected, explanation, changes)
	if err != nil {
		return RunPlanProjection{}, err
	}
	if err = tx.Commit(); err != nil {
		return RunPlanProjection{}, err
	}
	return projection, nil
}

func (s *Store) patchRunPlanTx(ctx context.Context, tx *sql.Tx, tenant, runID string, expected int, explanation string, changes []RunPlanStepInput) (RunPlanProjection, error) {
	previous, err := latestRunPlanTx(ctx, tx, tenant, runID)
	if err != nil {
		return RunPlanProjection{}, err
	}
	current := 0
	if previous != nil {
		current = previous.Version
	}
	if expected < 1 || expected != current {
		return RunPlanProjection{}, &PlanVersionChangedError{Expected: expected, Current: current}
	}
	full, err := mergeRunPlanPatch(runID, previous, changes)
	if err != nil {
		return RunPlanProjection{}, err
	}
	projection, err := s.syncRunPlanTx(ctx, tx, tenant, runID, explanation, full, nil, false, false)
	if err != nil {
		return RunPlanProjection{}, err
	}
	return projection, nil
}

func mergeRunPlanPatch(runID string, previous *RunPlan, changes []RunPlanStepInput) ([]RunPlanStepInput, error) {
	full := make([]RunPlanStepInput, 0, len(previous.Steps)+len(changes))
	positions := map[string]int{}
	var refs []StalePlanStepRef
	var ids []string
	for i, step := range previous.Steps {
		input := step.RunPlanStepInput
		if input.ReusePriorVerification {
			input.implicitPriorReuse = true
		}
		full = append(full, input)
		positions[step.StepID] = i
		ids = append(ids, step.StepID)
		refs = append(refs, StalePlanStepRef{StepID: step.StepID, Status: step.Status, Step: step.Step})
	}
	seen := map[string]bool{}
	for i, item := range changes {
		item.StepID = strings.TrimSpace(item.StepID)
		if item.StepID == "" {
			return nil, fmt.Errorf("plan[%d].step_id is required in patch mode; use snapshot mode to add or reorder steps", i)
		}
		index, ok := positions[item.StepID]
		if !ok {
			return nil, &StalePlanStepReferenceError{StepID: item.StepID, RunID: runID, Current: ids, Index: i, Steps: refs}
		}
		if seen[item.StepID] {
			return nil, fmt.Errorf("plan step %s appears more than once in the patch", item.StepID)
		}
		seen[item.StepID] = true
		full[index] = item
	}
	var active []string
	for _, step := range full {
		if strings.TrimSpace(step.Status) == "in_progress" {
			active = append(active, step.StepID)
		}
	}
	if len(active) > 1 {
		return nil, fmt.Errorf("patch would leave more than one plan step in_progress (%s); explicitly finish or pause the previous active step", strings.Join(active, ", "))
	}
	return full, nil
}
