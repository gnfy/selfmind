package verification

import (
	"fmt"
	"strings"
)

// Blockers returns bounded source references; replacement resolves full identity
// from storage so the model need not reproduce long criteria or path lists.
func Blockers(mutations []Mutation, checks []Check) string {
	type gap struct {
		ID        string `json:"evidence_id"`
		State     string `json:"state"`
		Criterion string `json:"criterion,omitempty"`
		Target    string `json:"target,omitempty"`
		CWD       string `json:"cwd,omitempty"`
	}
	bound := func(s string) string {
		r := []rune(s)
		if len(r) > 300 {
			return string(r[:300]) + "…"
		}
		return s
	}
	out := []gap{}
	latestChecks := Latest(checks)
	affectedAt := func(check Check) int64 { return RelevantMutationAt(check, mutations) }
	for _, check := range latestChecks {
		status := check.Status
		if check.StartedAt < affectedAt(check) {
			if stepCheckSupersededByFreshPass(check, latestChecks, affectedAt) {
				continue
			}
			status = "stale"
		}
		if status == "succeeded" {
			continue
		}
		g := gap{ID: check.ToolCallID, State: status, CWD: bound(check.CWD)}
		if check.Binding != nil {
			g.Criterion = bound(check.Binding.Criterion)
			g.Target = bound(check.Binding.Target)
		}
		out = append(out, g)
		if len(out) == 4 {
			break
		}
	}
	if len(out) == 0 {
		return ""
	}
	lines := []string{}
	for _, g := range out {
		lines = append(lines, fmt.Sprintf("%s: %s; criterion=%q; target=%q; cwd=%q", g.ID, g.State, g.Criterion, g.Target, g.CWD))
	}
	return " Blocking evidence (correct the referenced check, not unrelated successful checks): " + strings.Join(lines, "; ")
}
