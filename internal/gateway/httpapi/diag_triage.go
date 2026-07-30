package httpapi

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

// smartTriageDiagLines reports what smart-mode approval triage actually did in
// the last 24h. It exists because "why is it asking me so much?" had no
// answer from any surface: a strict judge and a BROKEN judge (missing cheap-role
// model, unreachable provider, timeout) produce the same prompt, and the second
// silently degrades smart mode into on-request. The counts distinguish them, and
// the mode is printed alongside because a persisted /mode can outrank the smart
// product default indefinitely without ever announcing itself.
//
// Returns "" when there is nothing to say: not in smart mode and no triage
// decision was recorded in the window.
func (d *Server) smartTriageDiagLines(ctx context.Context, identity *control.IdentityContext) string {
	if d == nil || identity == nil {
		return ""
	}
	stats := tools.TriageDiagnostics(identity.TenantID, identity.PersonID)
	mode := d.effectiveApprovalModeForDiag(ctx, identity)
	if mode != tools.ApprovalSmart && stats.Total() == 0 {
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Smart triage (24h) [mode: %s]: ", mode)
	if stats.Total() == 0 {
		sb.WriteString("no dangerous operation reached triage yet\n")
	} else {
		fmt.Fprintf(&sb, "auto-approved %d, blocked %d, escalated %d, unavailable %d\n",
			stats.Approved, stats.Denied, stats.Escalated, stats.Unavailable)
	}
	// An unavailable-only window is the actionable case: the funnel is not
	// strict, it is off, so every dangerous op becomes a human ask.
	if stats.Unavailable > 0 && stats.Approved == 0 {
		sb.WriteString("- automatic triage is not ruling: every dangerous operation falls through to a human ask\n")
		sb.WriteString("- check the models.roles.background_review route (`selfmind model check`); a nil or failing judge never auto-approves\n")
	}
	if stats.LastError != "" {
		fmt.Fprintf(&sb, "- last judge error %s: %s\n",
			stats.LastErrorAt.Format("15:04"), truncate(toOneLine(tools.RedactSensitive(stats.LastError)), 140))
	}
	return sb.String()
}

// effectiveApprovalModeForDiag resolves the person's mode with the SAME
// precedence the run path uses minus the per-request override (persisted /mode,
// else the product default), so diagnostics can never disagree with what the
// next run will actually apply.
func (d *Server) effectiveApprovalModeForDiag(ctx context.Context, identity *control.IdentityContext) tools.ApprovalMode {
	pref := ""
	if d.Control != nil {
		if value, err := d.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode); err == nil {
			pref = value
		}
	}
	return tools.EffectiveApprovalMode(pref)
}
