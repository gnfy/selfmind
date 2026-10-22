package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"selfmind/internal/verification"
)

// ResolveVerificationBinding carries durable obligation identity into a new
// attempt. Main chooses what to check and whether a method proves the criterion;
// the runtime resolves only owned references and never infers semantic equivalence.
func (s *Store) ResolveVerificationBinding(ctx context.Context, tenant, runID string, b verification.Binding, cwd string) (*verification.Binding, error) {
	run, err := s.GetRun(ctx, tenant, runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("verification Run is unavailable")
	}
	if b.Replaces != "" {
		var raw string
		err := s.db.QueryRowContext(ctx, `SELECT payload_json FROM task_events WHERE run_id=? AND type='evidence.recorded' AND json_extract(payload_json,'$.evidence.tool_call_id')=? ORDER BY cursor DESC LIMIT 1`, runID, b.Replaces).Scan(&raw)
		if err != nil {
			message := fmt.Sprintf("verification reference %q is unavailable in this Run: replaces names a check made in this Run, and call ids restart in every Run.", b.Replaces)
			if note := s.earlierRunEvidenceNote(ctx, tenant, runID, b.Replaces); note != "" {
				message += " " + note
			} else {
				message += " To check a Plan step again, bind verify to its step_id."
			}
			return nil, errors.New(message)
		}
		var p struct {
			Evidence workUnitEvidence `json:"evidence"`
		}
		if json.Unmarshal([]byte(raw), &p) != nil || p.Evidence.Kind != "verification" || p.Evidence.Command == nil || !verification.ValidBinding(p.Evidence.Command.Binding) {
			if (p.Evidence.Kind == "command" || p.Evidence.Kind == "observation") && b.StepID != "" {
				b.Replaces = ""
			} else if p.Evidence.Kind == "command" || p.Evidence.Kind == "observation" {
				return nil, fmt.Errorf("reference %q is a %s observation, not a verify check; omit replaces and pass step_id to bind verify to the Plan step", b.Replaces, p.Evidence.Kind)
			} else {
				return nil, fmt.Errorf("reference has no stable verification obligation; inspect the original evidence instead of replacing an unbound historical check")
			}
		} else if p.Evidence.Command.Binding.StepID != "" &&
			(b.StepID == "" || b.StepID == p.Evidence.Command.Binding.StepID) &&
			(b.Criterion == "" || b.Criterion == p.Evidence.Command.Binding.Criterion) &&
			(b.Target == "" || b.Target == p.Evidence.Command.Binding.Target) &&
			strings.TrimSpace(cwd) != "" && strings.TrimSpace(p.Evidence.Command.CWD) != "" &&
			strings.TrimSpace(cwd) != strings.TrimSpace(p.Evidence.Command.CWD) {
			old := p.Evidence.Command.Binding
			b.StepID = old.StepID
			if b.Criterion == "" {
				b.Criterion = old.Criterion
			}
			if b.Target == "" {
				b.Target = old.Target
			}
			if b.LocalDependencies == nil {
				b.LocalDependencies = old.LocalDependencies
			}
			b.Replaces = ""
		} else {
			old := p.Evidence.Command.Binding
			if b.Criterion == "" {
				b.Criterion = old.Criterion
			}
			if b.Target == "" {
				b.Target = old.Target
			}

			if b.StepID == "" {
				b.StepID = old.StepID
			}
			if b.LocalDependencies == nil {
				b.LocalDependencies = old.LocalDependencies
			}
			// A replacement preserves its obligation unless Main explicitly
			// selects another server-issued step. Current Plan position is not a
			// semantic relationship between two checks.
			b.Version = old.Version
			// A different current step is valid only when Main explicitly selects it.
			// Keep the original criterion, target and replacement edge for diagnosis.
			plan, planErr := s.LatestRunPlan(ctx, tenant, runID)
			if planErr != nil {
				return nil, planErr
			}
			if b.StepID != old.StepID {
				b.Version = 3
			}
			if err := s.ValidateVerificationReplacement(ctx, tenant, runID, b, cwd); err != nil {
				return nil, err
			}
			if err := s.requireOpenVerificationCycle(ctx, tenant, runID, plan, b.StepID); err != nil {
				return nil, err
			}

			return &b, nil
		}
	}
	explicitStandalone := b.StepID == "" && (b.Criterion != "" || b.Target != "")
	if explicitStandalone {
		return &b, nil
	}
	plan, err := s.LatestRunPlan(ctx, tenant, runID)
	if err != nil {
		return nil, err
	}
	if plan != nil {
		selected := -1
		if b.StepID != "" {
			for i := range plan.Steps {
				if plan.Steps[i].StepID == b.StepID {
					selected = i
					break
				}
			}
		} else {
			// Main may run a check after producing the input but before sending
			// the next plan snapshot. Select only from the active work unit:
			// its active required step wins, otherwise plan order selects the
			// earliest pending obligation. This carries runtime identity without
			// matching model prose or jumping into a later independent objective.
			active := -1
			for i := range plan.Steps {
				if plan.Steps[i].Status == "in_progress" {
					active = i
					break
				}
			}
			if active >= 0 {
				start, end := active, len(plan.Steps)
				for start > 0 && !isRunPlanBoundary(plan.Steps, start) {
					start--
				}
				for i := active + 1; i < len(plan.Steps); i++ {
					if isRunPlanBoundary(plan.Steps, i) {
						end = i
						break
					}
				}
				for i := start; i < end; i++ {
					step := plan.Steps[i]
					if !step.VerificationRequired || (step.Status != "pending" && step.Status != "in_progress") {
						continue
					}
					if step.Status == "in_progress" {
						selected = i
						break
					}
					if selected < 0 {
						// Plan order is the runtime-owned execution order. If Main
						// verifies after finishing a diagnostic step but before sending
						// the transition snapshot, the earliest pending obligation is
						// the only deterministic target that does not inspect prose.
						selected = i
					}
				}
			}
		}
		if selected >= 0 {
			step := plan.Steps[selected]
			if err := s.requireOpenVerificationCycle(ctx, tenant, runID, plan, step.StepID); err != nil {
				return nil, err
			}
			if strings.TrimSpace(step.SuccessCriteria) == "" {
				if b.StepID == "" {
					return nil, nil
				}
				return nil, fmt.Errorf("declare an observable plan criterion before binding verification to step %s", step.StepID)
			}
			if b.Criterion == "" {
				b.Criterion = step.SuccessCriteria
			}
			// Once the runtime selects a plan step, persist that server-issued
			// identity even when Main omitted step_id. The active step is only a
			// selection hint; the recorded evidence must not depend on whichever
			// step happens to be active when it is projected later.
			b.StepID = step.StepID
			b.Version = 3
			if b.Target == "" {
				b.Target = step.StepID
			}
			return &b, nil
		}
	}
	if b.StepID != "" {
		return nil, fmt.Errorf("verification step %q does not belong to the current Run plan", b.StepID)
	}
	return nil, nil
}

// A closed Work Unit owns a frozen evidence window. New observations must use a
// deliberately reopened cycle, never extend historical closure by accident.
type verificationCycleClosedError struct {
	stepID  string
	version int
}

func (e *verificationCycleClosedError) Error() string {
	return fmt.Sprintf("verification step %q has a closed observation cycle in accepted Plan version %d; use update_plan with mode=patch to reopen this step as pending, preserving the current in_progress step; then run verify with the same step_id", e.stepID, e.version)
}
func (*verificationCycleClosedError) VerificationCycleClosed() bool { return true }

func (s *Store) requireOpenVerificationCycle(ctx context.Context, tenant, runID string, plan *RunPlan, stepID string) error {
	if stepID == "" {
		return nil
	} // Explicit standalone checks keep their own identity.
	if plan != nil {
		for _, step := range plan.Steps {
			if step.StepID != stepID {
				continue
			}
			if step.Status == "cancelled" {
				return fmt.Errorf("verification step %q is cancelled; select a current open step explicitly", stepID)
			}
			var status string
			if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(w.status,'') FROM run_plan_steps p
				LEFT JOIN run_work_units w ON w.run_id=p.run_id AND w.id=p.work_unit_id
				WHERE p.tenant_id=? AND p.run_id=? AND p.plan_version=? AND p.step_id=? LIMIT 1`,
				normalizeTenant(tenant), runID, plan.Version, stepID).Scan(&status); err != nil {
				return err
			}
			if workUnitTerminal(status) {
				return &verificationCycleClosedError{stepID: stepID, version: plan.Version}
			}
			return nil
		}
	}
	return fmt.Errorf("verification step %q does not belong to the current Run plan", stepID)
}
