package control

import (
	"context"
	"database/sql"
	"errors"

	"selfmind/internal/verification"
)

// VerificationRecovery is bounded advice from one accepted read snapshot. It
// never changes progress, adopts evidence, or authorizes a tool invocation.
type VerificationRecovery struct {
	StepID       string   `json:"step_id,omitempty"`
	Action       string   `json:"action"`
	Replaces     string   `json:"replaces,omitempty"`
	CWD          string   `json:"cwd,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

func runStepCyclesTx(ctx context.Context, tx *sql.Tx, tenant, runID string, plan *RunPlan) (map[string]bool, error) {
	result := map[string]bool{}
	if plan == nil {
		return result, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.step_id,w.status FROM run_plan_steps p
 JOIN run_work_units w ON w.run_id=p.run_id AND w.id=p.work_unit_id
 WHERE p.tenant_id=? AND p.run_id=? AND p.plan_version=?`, tenant, runID, plan.Version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var step, status string
		if err := rows.Scan(&step, &status); err != nil {
			return nil, err
		}
		result[step] = workUnitTerminal(status)
	}
	return result, rows.Err()
}

// EvidenceRecovery uses the accepted windows, not the rolled-back candidate's
// terminal rows. Only exact owned identities are suggested; Main judges proof.
func (s RunPlanSnapshot) EvidenceRecovery(cause error) []VerificationRecovery {
	var issues *verification.EvidenceIssuesError
	var unavailable *verification.EvidenceUnavailableError
	if !errors.As(cause, &issues) && !errors.As(cause, &unavailable) {
		return nil
	}
	if !s.recoveryRead || !s.ClosureFacts.Available || !s.executable || s.pendingEffects {
		return []VerificationRecovery{{Action: "inspect"}}
	}
	var problems []verification.EvidenceIssue
	if issues != nil {
		problems = issues.Issues
	} else {
		// A cross-Run provenance note owns its explicit adoption path.
		if unavailable.Note != "" || unavailable.StepID == "" {
			return nil
		}
		issue := verification.EvidenceIssue{Ref: unavailable.Ref, StepID: unavailable.StepID, Kind: "unavailable"}
		for _, c := range s.checks {
			if c.Source == nil && c.ToolCallID == issue.Ref && c.Binding != nil && c.Binding.StepID == issue.StepID {
				issue.Criterion = c.Binding.Criterion
				issue.Target = c.Binding.Target
				issue.CWD = c.CWD
				break
			}
		}
		problems = []verification.EvidenceIssue{issue}
	}
	out := []VerificationRecovery{}
	for _, issue := range problems {
		if len(out) >= 8 {
			break
		}
		recovery := VerificationRecovery{StepID: issue.StepID, Action: "inspect"}
		validStep := issue.StepID == ""
		if s.Plan != nil {
			for _, step := range s.Plan.Steps {
				if step.StepID == issue.StepID {
					validStep = step.Status != "cancelled"
					break
				}
			}
		}
		if validStep && issue.Kind != "unconfirmed" {
			for _, candidate := range s.citeable {
				if candidate.StepID != issue.StepID || len(candidate.ID) > 256 {
					continue
				}
				matches := issue.Kind == "unavailable" && issue.Criterion == ""
				for _, c := range s.checks {
					if c.Source == nil && c.ToolCallID == candidate.ID && c.Binding != nil && c.Binding.StepID == issue.StepID && c.Binding.Criterion == issue.Criterion && c.Binding.Target == issue.Target && c.CWD == issue.CWD {
						matches = true
						break
					}
				}
				if matches && len(recovery.EvidenceRefs) < 8 {
					recovery.EvidenceRefs = append(recovery.EvidenceRefs, candidate.ID)
				}
			}
			if len(recovery.EvidenceRefs) > 0 {
				recovery.Action = "cite"
			} else if issue.StepID == "" {
				recovery.Action = "verify"
			} else if closed, known := s.cycles[issue.StepID]; known {
				recovery.Action = "verify"
				if closed {
					recovery.Action = "reopen"
				}
			}
		}
		if recovery.Action == "verify" || recovery.Action == "reopen" {
			for _, check := range s.checks {
				if check.Source == nil && check.ToolCallID == issue.Ref && len(issue.Ref) <= 256 && verification.ValidBinding(check.Binding) && check.Binding.StepID == issue.StepID {
					recovery.Replaces = issue.Ref
					recovery.CWD = check.CWD
					break
				}
			}
		}
		duplicate := false
		for _, old := range out {
			if old.StepID == recovery.StepID && old.Action == recovery.Action && old.Replaces == recovery.Replaces && sameRecoveryRefs(old.EvidenceRefs, recovery.EvidenceRefs) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, recovery)
		}
	}
	return out
}
func sameRecoveryRefs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
