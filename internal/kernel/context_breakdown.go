package kernel

import (
	"strings"

	"selfmind/internal/kernel/llm"
)

// ContextBreakdown attributes a turn's prompt tokens to the components that
// produced them, so a solo owner can SEE where the window goes on a long
// session (P1-2, docs/STATUS.md "ACTIVE PLAN"). Estimates use the same
// heuristic tokenizer as the budget path — proportions, not exact counts, are
// the point. Categories partition the system prompt by its stable section
// markers; History is the replayed spine/message tail.
type ContextBreakdown struct {
	Identity       int `json:"identity"`        // soul + base guidance (default bucket)
	Tools          int `json:"tools"`           // # TOOL USE INSTRUCTIONS block
	ProjectContext int `json:"project_context"` // # PROJECT CONTEXT (AGENTS.md et al.)
	Memory         int `json:"memory"`          // <user-profile> + <memory-context>
	Runtime        int `json:"runtime"`         // # SELECTED RUNTIME CONTEXT (task/ws/recall)
	History        int `json:"history"`         // replayed conversation messages
	Total          int `json:"total"`
}

// breakdownMarker maps a system-prompt section header to a category. Order does
// not matter; categorization is a running-section scan, robust to the order the
// parts were assembled in.
var breakdownMarkers = []struct {
	marker   string
	category string
}{
	{"# PROJECT CONTEXT", "project_context"},
	{"# TOOL USE INSTRUCTIONS", "tools"},
	{"# SELECTED RUNTIME CONTEXT", "runtime"},
	{"<user-profile>", "memory"},
	{"<memory-context>", "memory"},
}

// ComputeContextBreakdown categorizes the assembled system prompt by section and
// sums the non-system messages as History.
func ComputeContextBreakdown(systemPrompt string, messages []llm.Message) ContextBreakdown {
	var b ContextBreakdown
	section := "identity"
	for _, line := range strings.Split(systemPrompt, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, m := range breakdownMarkers {
			if strings.HasPrefix(trimmed, m.marker) {
				section = m.category
				break
			}
		}
		tok := estimateTokens(line)
		switch section {
		case "project_context":
			b.ProjectContext += tok
		case "tools":
			b.Tools += tok
		case "runtime":
			b.Runtime += tok
		case "memory":
			b.Memory += tok
		default:
			b.Identity += tok
		}
	}
	for _, msg := range messages {
		if msg.Role == "system" {
			continue
		}
		b.History += estimateTokens(msg.Content)
	}
	b.Total = b.Identity + b.Tools + b.ProjectContext + b.Memory + b.Runtime + b.History
	return b
}

// Payload renders the breakdown as an event payload (component → tokens, plus
// total), for the context.breakdown run event that /diag reads back.
func (b ContextBreakdown) Payload() map[string]interface{} {
	return map[string]interface{}{
		"identity":        b.Identity,
		"tools":           b.Tools,
		"project_context": b.ProjectContext,
		"memory":          b.Memory,
		"runtime":         b.Runtime,
		"history":         b.History,
		"total":           b.Total,
	}
}
