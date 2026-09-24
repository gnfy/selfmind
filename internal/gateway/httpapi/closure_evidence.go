package httpapi

import (
	"fmt"
	"strings"

	"selfmind/internal/gateway/api"
)

// Missing model prose must not erase the evidence already collected by the
// run. This fallback reports observations, never an inferred business success:
// the accepted plan's progress first, then recorded files and verification.
func missingFinalEvidenceSummary(runID string, outcome api.RunOutcome, steps []taskPlanStep) (string, string) {
	summary := "The run stopped without a final response."
	progress, done, open := planProgress(steps)
	if progress != "" {
		summary = "The run stopped without a final response: " + progress
	}
	var b strings.Builder
	b.WriteString("**Work remains**\n\n" + summary)
	appendResultItems(&b, "Completed plan steps", done, planStepItems)
	appendResultItems(&b, "Open plan steps", open, planStepItems)
	if len(outcome.Files) > 0 {
		b.WriteString("\n\nRecorded file changes:")
		for _, path := range outcome.Files[:min(5, len(outcome.Files))] {
			b.WriteString("\n- " + truncate(toOneLine(path), 240))
		}
		if len(outcome.Files) > 5 {
			fmt.Fprintf(&b, "\n- %d additional changed files are retained in the run evidence.", len(outcome.Files)-5)
		}
	}
	if outcome.Verification != nil {
		b.WriteString("\n\nRecorded verification: " + outcome.Verification.State + ". " + truncateRunes(toOneLine(outcome.Verification.Summary), 400))
	}
	if runID != "" {
		b.WriteString("\n\nResume: /resume " + runID)
	}
	return b.String(), summary
}
