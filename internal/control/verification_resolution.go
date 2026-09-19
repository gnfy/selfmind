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
		b.Version = old.Version
		if err := s.ValidateVerificationReplacement(ctx, tenant, runID, b, cwd); err != nil {
			return nil, err
		}
		return &b, nil
	}
	// An explicit standalone obligation remains standalone. Only a requested
	// plan reference, or an otherwise omitted binding, derives plan identity.
	if b.StepID == "" && (b.Criterion != "" || b.Target != "") {
		return &b, nil
	}
	plan, err := s.LatestRunPlan(ctx, tenant, runID)
	if err != nil {
		return nil, err
	}
	if plan != nil {
		for _, step := range plan.Steps {
			if (b.StepID != "" && step.StepID != b.StepID) || (b.StepID == "" && step.Status != "in_progress") {
				continue
			}
			if b.StepID == "" && !step.VerificationRequired {
				return nil, nil
			}
			if strings.TrimSpace(step.SuccessCriteria) == "" {
				if b.StepID == "" {
					return nil, nil
				}
				return nil, fmt.Errorf("declare an observable plan criterion before binding verification to step %s", step.StepID)
			}
			if b.Criterion != "" && b.Criterion != step.SuccessCriteria {
				return nil, fmt.Errorf("verification must preserve plan criterion %q", step.SuccessCriteria)
			}
			b.Criterion = step.SuccessCriteria
			if b.StepID != "" {
				b.Version = 3
			} else if b.LocalDependencies != nil {
				b.Version = 2
			} else {
				b.Version = 1
			}
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
