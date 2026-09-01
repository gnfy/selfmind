package control

import (
	"context"
	"strings"
)

// RecoveryAttemptSummary is the safe, bounded part of one durable tool-ledger
// row that is useful to a person deciding how to resume. Arguments, hashes,
// result bodies, and credentials deliberately stay out of this projection.
type RecoveryAttemptSummary struct {
	ToolName   string `json:"tool_name"`
	PlanStepID string `json:"plan_step_id,omitempty"`
	Strategy   string `json:"strategy,omitempty"`
	Status     string `json:"status"`
}

// RecoveryEffectSummary identifies an unresolved external effect without
// replaying the call or exposing its arguments.
type RecoveryEffectSummary struct {
	EffectID    string `json:"effect_id,omitempty"`
	ToolName    string `json:"tool_name"`
	PlanStepID  string `json:"plan_step_id,omitempty"`
	Strategy    string `json:"strategy,omitempty"`
	EffectClass string `json:"effect_class,omitempty"`
	Status      string `json:"status"`
}

// RecoveryHandoff is the control plane's single read-only projection for an
// interrupted Run. Notifications, CLI, IM, and HTTP may render it differently,
// but none of them reconstruct recovery safety from prose or raw event JSON.
type RecoveryHandoff struct {
	TaskID              string                   `json:"task_id"`
	RunID               string                   `json:"run_id"`
	OriginalGoal        string                   `json:"original_goal,omitempty"`
	Cause               string                   `json:"cause,omitempty"`
	Reason              string                   `json:"reason"`
	CompletedSteps      []string                 `json:"completed_steps,omitempty"`
	UnresolvedSteps     []string                 `json:"unresolved_steps,omitempty"`
	UncertainEffects    []RecoveryEffectSummary  `json:"uncertain_effects,omitempty"`
	AttemptedStrategies []RecoveryAttemptSummary `json:"attempted_strategies,omitempty"`
	UnlockCondition     string                   `json:"unlock_condition"`
	ResumePath          string                   `json:"resume_path"`
}

// RecoveryHandoffForRun derives an interrupted Run's actionable handoff from
// its durable plan, effect ledger, and typed recovery decision. Person scope is
// checked before any details are returned, so the projection is safe to share
// across that person's endpoints but never across people.
func (s *Store) RecoveryHandoffForRun(ctx context.Context, tenantID, personID, runID string) (*RecoveryHandoff, error) {
	if s == nil || s.db == nil || strings.TrimSpace(runID) == "" || strings.TrimSpace(personID) == "" {
		return nil, nil
	}
	run, err := s.GetRun(ctx, tenantID, runID)
	if err != nil || run == nil || run.PersonID != personID || run.Status != "interrupted" {
		return nil, err
	}
	decision, err := s.AutomaticRunRecoveryDecisionForRun(ctx, tenantID, runID)
	if err != nil {
		return nil, err
	}
	handoff := &RecoveryHandoff{
		TaskID:       run.TaskID,
		RunID:        run.ID,
		OriginalGoal: strings.TrimSpace(run.InputSummary),
		Cause:        strings.TrimSpace(decision.Cause),
		Reason:       strings.TrimSpace(decision.Reason),
		ResumePath:   "/resume " + run.ID,
	}
	if handoff.Reason == "" {
		handoff.Reason = "automatic_recovery_unavailable"
	}
	handoff.UnlockCondition = recoveryUnlockCondition(handoff.Reason)

	plan, err := s.LatestRunPlan(ctx, tenantID, runID)
	if err != nil {
		return nil, err
	}
	if plan != nil {
		for _, step := range plan.Steps {
			switch step.Status {
			case "completed":
				handoff.CompletedSteps = append(handoff.CompletedSteps, step.Step)
			case "cancelled":
				// A cancelled step is resolved but was not completed work.
			default:
				handoff.UnresolvedSteps = append(handoff.UnresolvedSteps, step.Step)
			}
		}
	}

	uncertain, err := s.ListUncertainToolEntries(ctx, tenantID, runID, 20)
	if err != nil {
		return nil, err
	}
	for _, entry := range uncertain {
		handoff.UncertainEffects = append(handoff.UncertainEffects, RecoveryEffectSummary{
			EffectID: entry.EffectID, ToolName: entry.ToolName, PlanStepID: entry.PlanStepID,
			Strategy: entry.Strategy, EffectClass: entry.EffectClass, Status: entry.Status,
		})
	}

	rows, err := s.db.QueryContext(ctx, `SELECT tool_name, COALESCE(plan_step_id, ''), COALESCE(strategy, ''), status
		FROM tool_ledger
		WHERE tenant_id=? AND run_id=? AND tool_name NOT IN ('update_plan', 'finish_run')
		ORDER BY created_at ASC, tool_call_id ASC LIMIT 50`, normalizeTenant(tenantID), runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var attempt RecoveryAttemptSummary
		if err := rows.Scan(&attempt.ToolName, &attempt.PlanStepID, &attempt.Strategy, &attempt.Status); err != nil {
			return nil, err
		}
		handoff.AttemptedStrategies = append(handoff.AttemptedStrategies, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return handoff, nil
}

func recoveryUnlockCondition(reason string) string {
	switch strings.TrimSpace(reason) {
	case "uncertain_effect_requires_observation":
		return "Verify every uncertain external effect with read-only evidence before allowing another mutation."
	case "known_effect_requires_user_resume":
		return "Review the recorded external effect and resume explicitly; do not repeat it without current-state evidence."
	case "approval_recovery_owns_run", "specialist_origin":
		return "Resolve or resume the task's exact approval flow; generic recovery must not invent authorization."
	case "clarification_recovery_owns_run":
		return "Answer the task's pending clarification so its exact continuation can proceed."
	case "watcher_recovery_owns_run":
		return "Use the durable watcher state or its finalization path; do not replace it with model-driven polling."
	case "automatic_recovery_already_attempted":
		return "Inspect the prior recovery attempt and resume explicitly with new evidence or a genuinely different strategy."
	case "historical_recovery_contract":
		return "Historical runs require an explicit resume and keep their original recovery semantics."
	case "parent_already_claimed":
		return "Continue or inspect the child run that already claimed this interruption."
	case "missing_interruption_outcome", "invalid_interruption_outcome", "interruption_not_auto_recoverable":
		return "Inspect the interruption evidence and resume explicitly when it is safe to continue."
	default:
		return "Resume explicitly after reviewing the durable plan and effect evidence."
	}
}
