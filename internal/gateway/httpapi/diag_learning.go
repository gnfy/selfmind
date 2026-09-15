package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
)

func (d *Server) learningDiagReply(ctx context.Context, identity *control.IdentityContext) (string, error) {
	if d == nil || d.Control == nil || identity == nil {
		return "Learning diagnostics unavailable.", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ws, err := d.Control.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return "Learning diagnostics unavailable: workspace lookup failed.", nil
	}
	workspaceID, workspaceName := "", "unscoped"
	if ws != nil {
		workspaceID, workspaceName = ws.ID, ws.Name
	}
	h, err := d.Control.SkillLearningHealthForWorkspace(ctx, identity.TenantID, identity.PersonID, workspaceID, SkillCurationJobVersion)
	if err != nil {
		return "Learning diagnostics unavailable: evidence query failed or exceeded its time budget.", nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Skill learning (current default workspace: %s)\nGenerated: %s\n", truncate(toOneLine(workspaceName), 80), time.Now().Format(time.RFC3339))
	fmt.Fprintf(&sb, "Observation collection: %t; curator configured: %t\n", d.SelfEvolution.Enabled, d.SkillCurator != nil)
	fmt.Fprintf(&sb, "Selection window: %d / %d latest observations; current snapshot, not historical readiness\n", h.Observations, h.WindowLimit)
	if !h.NewestAt.IsZero() {
		fmt.Fprintf(&sb, "Evidence coverage: %s to %s\n", h.OldestAt.Format(time.RFC3339), h.NewestAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&sb, "Procedural successes in window: %d; explicitly verified: %d\n", h.ProceduralSuccesses, h.VerifiedSuccesses)
	fmt.Fprintf(&sb, "Largest creation cohort: %d independent successful runs; 3 required\n", h.MaxIndependentSuccesses)
	fmt.Fprintf(&sb, "Evidence gate by anchor: %s\n", formatCountMap(h.Reasons))
	sb.WriteString("Ready evidence still needs deduplication, curator validation and publication review.\n")
	fmt.Fprintf(&sb, "Curator jobs (stored state): %s\n", formatCountMap(h.Jobs))
	fmt.Fprintf(&sb, "Attributed versions (stored state): %s; managed activations: %d\n", formatCountMap(h.Versions), h.Activations)
	if h.Observations == 0 {
		sb.WriteString("No workflow observations in this workspace.\n")
	}
	if h.Activations == 0 {
		sb.WriteString("Reading a SKILL.md file does not record a managed version activation.\n")
	}
	usage, err := d.Control.MaintenanceProviderUsageForPersonSince(ctx, identity.TenantID, identity.PersonID, time.Now().Add(-24*time.Hour))
	if err != nil {
		sb.WriteString("Curator provider calls (person, 24h): unavailable\n")
	} else {
		calls, failed := 0, 0
		for _, item := range usage {
			if item.Role == "skill_curator" {
				calls += item.Calls
				failed += item.Failed
			}
		}
		fmt.Fprintf(&sb, "Curator provider calls (person, 24h): %d; failed: %d\n", calls, failed)
	}
	sb.WriteString(d.memoryGovernanceDiagLines(ctx, identity))
	return strings.TrimSpace(sb.String()), nil
}

func (d *Server) memoryGovernanceDiagLines(ctx context.Context, identity *control.IdentityContext) string {
	mode := "disabled"
	if d.MemoryConsolidator != nil {
		mode = d.MemoryConsolidator.Mode()
	}
	reply := "Governance mode: " + mode
	if mode == "shadow" {
		reply += "\nShadow evaluates consolidation without automatic merge/archive; preference intake and explicit memory writes remain separate."
	}
	if summarizer, ok := d.MemoryConsolidator.(interface {
		PassSummary(context.Context, string) string
	}); ok {
		if line := summarizer.PassSummary(ctx, identity.PersonID); line != "" {
			reply += "\n" + line
		}
	}
	if d.Control == nil {
		return reply + "\nGovernance scheduler: unavailable"
	}
	if schedule, ok, err := d.Control.MemoryGovernanceScheduleForPerson(ctx, identity.TenantID, identity.PersonID); err != nil {
		reply += "\nGovernance scheduler: unavailable"
	} else if !ok {
		reply += "\nGovernance scheduler: not initialized"
	} else {
		reply += "\n" + memoryGovernanceScheduleSummary(schedule, time.Now())
	}
	return reply
}
