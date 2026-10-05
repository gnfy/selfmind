package cli

import (
	"encoding/json"
	"fmt"
	"strings"
)

type renderedPlanStep struct {
	Step                    string `json:"step"`
	Status                  string `json:"status"`
	CancellationDisposition string `json:"cancellation_disposition"`
	CancellationReason      string `json:"cancellation_reason"`
}

// renderPlanCell renders update_plan as a Codex-style checklist (the "hybrid"
// look chosen for SelfMind): header `• Plan · done/total` for work planned in
// this line, `• Resumed plan · done/total` for a plan inherited from the run
// being resumed; then a
// tree-indented block — an italic/dim explanation note, then one line per step
// marked ✔ (struck-through+dim) completed / □ (cyan+bold) in-progress / □ (dim)
// pending. Long notes and steps wrap to the terminal width with a hanging
// indent rather than being truncated. We keep the `· done/total` progress in the
// header (codex puts it in a persistent status bar, which SelfMind lacks).
// Returns "" only if content isn't parseable plan JSON (caller falls back).
func renderPlanCellWithStyles(content string, duration float64, width int, styles transcriptStyles) string {
	return renderPlanCellNoted(content, duration, width, styles, "")
}

// renderPlanCellNoted renders a plan cell whose heading ends with note, such as
// how long ago the pinned plan last moved.
func renderPlanCellNoted(content string, duration float64, width int, styles transcriptStyles, note string) string {
	var payload struct {
		Explanation string             `json:"explanation"`
		Source      string             `json:"source"`
		Plan        []renderedPlanStep `json:"plan"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return ""
	}
	if width < 20 {
		width = 20
	}
	completed := 0
	cancelled := 0
	for _, s := range payload.Plan {
		if s.Status == "completed" {
			completed++
		} else if s.Status == "cancelled" {
			cancelled++
		}
	}

	// Build the indented content block (explanation note + steps) without the
	// tree prefix; the prefix is applied uniformly afterwards.
	var block []string
	if exp := strings.TrimSpace(payload.Explanation); exp != "" {
		for _, ln := range strings.Split(wrapText(exp, width-4), "\n") {
			block = append(block, styles.planExplain.Render(ln))
		}
	}
	if len(payload.Plan) == 0 {
		block = append(block, styles.planExplain.Render("(no steps provided)"))
	}
	shown := 0
	for _, s := range payload.Plan {
		if shown >= maxPlanSteps {
			break
		}
		block = append(block, planStepLinesWithStyles(strings.TrimSpace(s.Step), s.Status, width-4, styles)...)
		if label := planCancellationLabel(s); label != "" {
			detail := label
			if reason := strings.TrimSpace(sanitizeTerminalText(s.CancellationReason)); reason != "" {
				detail += ": " + reason
			}
			for _, line := range strings.Split(wrapText(detail, width-6), "\n") {
				block = append(block, "  "+styles.planExplain.Render(line))
			}
		}
		shown++
	}
	if len(payload.Plan) > maxPlanSteps {
		block = append(block, styles.planPending.Render(fmt.Sprintf("… %d more steps", len(payload.Plan)-maxPlanSteps)))
	}

	var sb strings.Builder
	// Two headings, and only two. The distinction worth a different word is
	// PROVENANCE — authored in this line of work, or inherited from the run
	// being resumed — not revision count: every update is a complete snapshot,
	// and `· done/total` already shows movement. A third "Updated plan" label
	// made one plan look like three different things as a turn progressed.
	heading := "Plan"
	// "parent_run" is the pre-v12 source tag for an inherited plan; historical
	// transcripts still carry it.
	if source := strings.TrimSpace(payload.Source); strings.EqualFold(source, "resumed_run") || strings.EqualFold(source, "parent_run") {
		heading = "Resumed plan"
	}
	progress := fmt.Sprintf(" · %d/%d", completed, len(payload.Plan))
	if cancelled > 0 {
		progress += fmt.Sprintf(" completed · %d cancelled", cancelled)
	}
	header := styles.planSecondary.Render(glyphBullet) + " " + styles.planHeader.Render(heading) +
		styles.planSecondary.Render(progress+note)
	sb.WriteString(wrapText(header, width) + "\n")
	// Tree prefix: first block line gets "  └ ", the rest a flat 4-space indent.
	for i, ln := range block {
		if i == 0 {
			sb.WriteString(styles.planSecondary.Render("  └ ") + ln + "\n")
		} else {
			sb.WriteString("    " + ln + "\n")
		}
	}
	return sb.String()
}

// planStepLines renders one plan step into one or more styled lines: the status
// glyph + text on the first line, wrapped continuation lines hanging-indented
// under the text. The glyph is dimmed/colored by status; only completed step
// text is struck through (matching codex, which never strikes the glyph).
func planStepLinesWithStyles(text, status string, contentWidth int, styles transcriptStyles) []string {
	glyph := glyphPlanBox + " "
	glyphStyle := styles.planPending
	textStyle := styles.planPending
	switch status {
	case "completed":
		glyph = glyphPlanDone + " "
		glyphStyle = styles.planSecondary
		textStyle = styles.planDone
	case "in_progress":
		glyphStyle = styles.planActive
		textStyle = styles.planActive
	case "cancelled":
		glyph = "× "
		glyphStyle = styles.planSecondary
	}
	stepWidth := contentWidth - 2 // account for the 2-col glyph / hanging indent
	if stepWidth < 4 {
		stepWidth = 4
	}
	wrapped := strings.Split(wrapText(text, stepWidth), "\n")
	out := make([]string, 0, len(wrapped))
	for i, ln := range wrapped {
		if i == 0 {
			out = append(out, glyphStyle.Render(glyph)+textStyle.Render(ln))
		} else {
			out = append(out, "  "+textStyle.Render(ln)) // hang under the glyph
		}
	}
	return out
}

func planCancellationLabel(step renderedPlanStep) string {
	if step.Status == "pending" && step.CancellationDisposition == "unfinished" {
		return "Unfinished"
	}
	if step.Status != "cancelled" {
		return ""
	}
	switch step.CancellationDisposition {
	case "not_required":
		return "Cancelled · not required"
	case "user_takeover":
		return "Cancelled · user takeover"
	default:
		return "Cancelled"
	}
}
