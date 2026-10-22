package httpapi

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
)

func (c *RunCoordinator) externalEffectContext(ctx context.Context, tenant, person, runID string) []kernel.ExternalEffectContext {
	facts, err := c.srv.Control.InspectExternalEffectConflicts(ctx, tenant, person, runID, []string{control.UnknownExternalTarget})
	if err != nil {
		log.Warn("external effect context unavailable", "run_id", runID, "error", err)
		return nil
	}
	var out []kernel.ExternalEffectContext
	seen := map[string]bool{}
	for _, claim := range facts.All {
		key := claim.RunID + "/" + claim.EffectID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, kernel.ExternalEffectContext{ClaimID: claim.ID, RunID: claim.RunID, Target: claim.TargetKey, State: claim.Disposition, SameChain: claim.SameChain, Summary: claim.Summary, Outcome: claim.Outcome})
		if len(out) == 5 {
			break
		}
	}
	return out
}

func (c *RunCoordinator) withExternalEffectFacts(ctx context.Context, tenant, person, runID, content string, outcome *api.RunOutcome) string {
	claims, err := c.srv.Control.RunRelevantExternalEffects(ctx, tenant, person, runID)
	if err != nil {
		log.Warn("external effect result facts unavailable", "run_id", runID, "error", err)
		note := "External-effect state was unavailable when preparing this result. Use /effects to confirm before treating an effect as settled."
		outcome.Risks = appendUnique(outcome.Risks, note, 8)
		if strings.TrimSpace(content) == "" {
			content = structuredResultFallback(*outcome)
		}
		return strings.TrimSpace(content) + "\n\n" + note
	}
	if len(claims) == 0 {
		return content
	}
	if strings.TrimSpace(content) == "" {
		content = structuredResultFallback(*outcome)
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(content))
	b.WriteString("\n\n**Unresolved external effects**\n")
	for i, claim := range claims {
		outcome.UnresolvedEffects = append(outcome.UnresolvedEffects, api.ExternalEffectFact{ClaimID: claim.ID, RunID: claim.RunID, Target: claim.TargetKey, State: claim.Disposition})
		if i < 4 {
			fmt.Fprintf(&b, "\n- %s · %s · target %s", claim.ID, claim.Disposition, truncateRunes(claim.TargetKey, 120))
		}
	}
	if len(claims) > 4 {
		fmt.Fprintf(&b, "\n- %d more recorded", len(claims)-4)
	}
	b.WriteString("\n\nThese claims remain unresolved in the runtime snapshot used for this result. Approving a different action does not settle them. Use /effects for the observation and settlement paths.")
	return b.String()
}
