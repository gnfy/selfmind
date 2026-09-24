package control

import (
	"context"
	"encoding/json"
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
			return nil, fmt.Errorf("verification reference %q is unavailable in this Run", b.Replaces)
		}
		var p struct {
			Evidence workUnitEvidence `json:"evidence"`
		}
		if json.Unmarshal([]byte(raw), &p) != nil || p.Evidence.Kind != "verification" || p.Evidence.Command == nil || !verification.ValidBinding(p.Evidence.Command.Binding) {
			return nil, fmt.Errorf("reference has no stable verification obligation; inspect the original evidence instead of replacing an unbound historical check")
		}
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
		b.StepID = old.StepID
		b.Version = old.Version
		// Step identity is runtime-owned execution context, not part of Main's
		// semantic proof obligation. A later work unit may deliberately recheck
		// the same condition after its declared inputs changed. Bind that attempt
		// to the one active verification step while preserving criterion, target,
		// dependencies, and the explicit replacement edge.
		plan, planErr := s.LatestRunPlan(ctx, tenant, runID)
		if planErr != nil {
			return nil, planErr
		}
		if plan != nil {
			oldStepFound := false
			oldWorkUnitClosed := false
			for _, step := range plan.Steps {
				if step.StepID == old.StepID {
					oldStepFound = true
					break
				}
			}
			if oldStepFound {
				var status string
				if queryErr := s.db.QueryRowContext(ctx, `SELECT COALESCE(w.status,'')
					FROM run_plan_steps p
					LEFT JOIN run_work_units w ON w.run_id=p.run_id AND w.id=p.work_unit_id
					WHERE p.tenant_id=? AND p.run_id=?
					  AND p.plan_version=(SELECT MAX(version) FROM run_plan_versions WHERE tenant_id=? AND run_id=?)
					  AND p.step_id=? LIMIT 1`, normalizeTenant(tenant), runID,
					normalizeTenant(tenant), runID, old.StepID).Scan(&status); queryErr != nil {
					return nil, queryErr
				}
				oldWorkUnitClosed = workUnitTerminal(status)
			}
			// A complete plan snapshot may deliberately remove an obsolete step.
			// Its historical evidence keeps the old association, but a correction
			// must attach to the current active obligation rather than an id that can
			// no longer be projected by the current plan. A completed step that is
			// still inside an active work unit retains its identity: step status is
			// model judgment, not authority to move a replacement. Once the owning
			// work unit is frozen, a declared dependency recheck belongs to the new
			// active required step and does not rewrite that historical projection.
			if old.StepID == "" || !oldStepFound || oldWorkUnitClosed {
				for _, step := range plan.Steps {
					if step.Status == "in_progress" && step.VerificationRequired {
						b.StepID = step.StepID
						b.Version = 3
						break
					}
				}
			}
		}
		if err := s.ValidateVerificationReplacement(ctx, tenant, runID, b, cwd); err != nil {
			return nil, err
		}
		return &b, nil
	}
	explicitStandalone := b.StepID == "" && (b.Criterion != "" || b.Target != "")
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
	if explicitStandalone {
		return &b, nil
	}
	return nil, nil
}
