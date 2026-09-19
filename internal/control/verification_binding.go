package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/verification"
)

// ValidateVerificationReplacement resolves version-2 input obligations within
// the owned Run, including rechecks after a work unit closes. Historical bindings
// retain their current-work-unit boundary; closed projections are never rewritten.
func (s *Store) ValidateVerificationReplacement(ctx context.Context, tenant, runID string, b verification.Binding, cwd string) error {
	startedCursor := int64(0)
	if b.Version >= 2 && verification.ValidBinding(&b) {
		run, err := s.GetRun(ctx, tenant, runID)
		if err != nil {
			return err
		}
		if run == nil {
			return fmt.Errorf("verification Run is unavailable")
		}
	} else {
		units, err := s.ListRunWorkUnits(ctx, tenant, runID)
		if err != nil {
			return err
		}
		var unit *RunWorkUnit
		for i := range units {
			if units[i].Status == WorkUnitActive {
				unit = &units[i]
			}
		}
		if unit == nil {
			return fmt.Errorf("keep the work unit open before replacing historical verification")
		}
		startedCursor = unit.StartedCursor
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT payload_json FROM task_events WHERE run_id=? AND type='evidence.recorded' AND cursor>? AND json_extract(payload_json,'$.evidence.tool_call_id')=? ORDER BY cursor DESC LIMIT 1`, runID, startedCursor, b.Replaces).Scan(&raw)
	if err != nil {
		return fmt.Errorf("verification reference %q is outside the permitted Run evidence window", b.Replaces)
	}
	var payload struct {
		Evidence workUnitEvidence `json:"evidence"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil || payload.Evidence.Kind != "verification" || payload.Evidence.Command == nil {
		return fmt.Errorf("reference is not verification evidence")
	}
	e := payload.Evidence
	old := verification.Check{ToolCallID: e.ToolCallID, Binding: e.Command.Binding, CWD: e.Command.CWD, StartedAt: e.StartedAt, FinishedAt: e.FinishedAt}
	next := verification.Check{Binding: &b, CWD: cwd, StartedAt: time.Now().UnixNano()}
	if !verification.CanReplace(old, next) {
		return fmt.Errorf("verification replacement must preserve the recorded criterion, target, local dependencies and working directory; original criterion=%q target=%q cwd=%q", bindingCriterion(old.Binding), bindingTarget(old.Binding), old.CWD)
	}
	return nil
}

func bindingCriterion(b *verification.Binding) string {
	if b == nil {
		return ""
	}
	return strings.TrimSpace(b.Criterion)
}
func bindingTarget(b *verification.Binding) string {
	if b == nil {
		return ""
	}
	return strings.TrimSpace(b.Target)
}
