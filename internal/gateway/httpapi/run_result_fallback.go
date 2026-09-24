package httpapi

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/log"
)

// structuredResultFallback renders an already committed finish_run outcome
// when the provider did not produce separate final prose. It keeps the
// structured fields and their boundaries instead of turning the summary into
// one long answer. This path makes no additional model call.
func structuredResultFallback(outcome api.RunOutcome) string {
	var b strings.Builder
	label := "Result"
	switch outcome.Status {
	case "waiting_user":
		label = "Awaiting your decision"
	case "waiting_external", "waiting_finalization":
		label = "Waiting for external work"
	case "blocked", "interrupted", api.RunStatusVerificationPartial:
		label = "Work remains"
	}
	fmt.Fprintf(&b, "**%s**", label)
	if summary := strings.TrimSpace(outcome.Summary); summary != "" {
		// Keep paragraph boundaries: flattening the summary recreated the
		// unreadable one-line fallback that this renderer replaces. With no
		// separate prose the summary is the answer, and a waiting outcome
		// often ends with the decision it needs, so the bound counts characters
		// (CJK text keeps its visible length) and only stops a runaway field.
		fmt.Fprintf(&b, "\n\n%s", truncateRunes(summary, resultSummaryRunes))
	}
	appendResultItems(&b, "Completed", outcome.Done, 6)
	appendResultItems(&b, "Next steps", outcome.NextSteps, 8)
	appendResultItems(&b, "Risks", outcome.Risks, 8)
	return b.String()
}

func appendResultItems(b *strings.Builder, title string, items []string, limit int) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n\n**%s**", title)
	for i, item := range items {
		if i >= limit {
			fmt.Fprintf(b, "\n- … %d more recorded", len(items)-limit)
			break
		}
		if item = strings.TrimSpace(toOneLine(item)); item != "" {
			fmt.Fprintf(b, "\n- %s", truncateRunes(item, 320))
		}
	}
}

const (
	// resultSummaryRunes is above every structured summary observed in daily use.
	resultSummaryRunes = 1000
	// planStepItems lists a whole plan in nearly every observed Run.
	planStepItems = 10
)

// acceptedPlanSteps reads the latest accepted plan for a result rendered
// without model prose. A read failure renders without plan progress rather
// than failing finalization.
func (c *RunCoordinator) acceptedPlanSteps(ctx context.Context, tenantID, runID string) []taskPlanStep {
	plan, err := c.srv.Control.LatestRunPlan(ctx, tenantID, runID)
	if err != nil {
		log.Warn("run plan result projection failed", "run_id", runID, "error", err)
		return nil
	}
	if plan == nil {
		return nil
	}
	steps := make([]taskPlanStep, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		steps = append(steps, taskPlanStep{Step: step.Step, Status: step.Status})
	}
	return steps
}

// unresolvedPlanFallback describes the accepted plan projection, not the last
// assistant progress sentence. A model can narrate another retry while the
// turn is about to stop; that narration is not a result.
func unresolvedPlanFallback(outcome api.RunOutcome, steps []taskPlanStep) (string, string) {
	summary := "The run stopped with unresolved plan steps."
	progress, done, open := planProgress(steps)
	if progress != "" {
		summary = "The run stopped during plan close-out: " + progress
	}
	var b strings.Builder
	b.WriteString("**Work remains**\n\n")
	b.WriteString(summary)
	if outcome.External != nil && outcome.External.Status != "" {
		fmt.Fprintf(&b, "\n\nExternal observation: %s.", outcome.External.Status)
	}
	appendResultItems(&b, "Completed plan steps", done, planStepItems)
	appendResultItems(&b, "Open plan steps", open, planStepItems)
	return b.String(), summary
}

// planProgress states how much of the accepted plan resolved and lists its
// completed and open steps. An empty plan yields no progress sentence.
func planProgress(steps []taskPlanStep) (string, []string, []string) {
	resolved := 0
	done := make([]string, 0, len(steps))
	open := make([]string, 0, len(steps))
	for _, step := range steps {
		if step.Status == "completed" || step.Status == "cancelled" {
			resolved++
			if step.Status == "completed" {
				done = append(done, step.Step)
			}
			continue
		}
		open = append(open, step.Step)
	}
	if len(steps) == 0 {
		return "", done, open
	}
	progress := fmt.Sprintf("%d of %d steps resolved.", resolved, len(steps))
	if len(open) > 0 {
		progress += " Open plan step: " + truncateRunes(toOneLine(open[0]), 180)
	}
	return progress, done, open
}
