package kernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
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
	Skill          int `json:"skill"`           // active Skill body or bounded candidate metadata
	Runtime        int `json:"runtime"`         // task/run state and other runtime metadata
	Recall         int `json:"recall"`          // semantic/session/canonical recall slices
	Artifacts      int `json:"artifacts"`       // artifact references selected for this turn
	History        int `json:"history"`         // replayed conversation messages
	ToolResults    int `json:"tool_results"`    // bounded tool-result messages replayed to the model
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
	{"<!-- SELFMIND_ACTIVE_SKILL_BEGIN -->", "skill"},
	{"## Skill Candidates for Current Work Unit", "skill"},
	{"<user-profile>", "memory"},
	{"<memory-context>", "memory"},
}

// PromptSection is one assembled system-prompt component, recorded AT
// assembly time (execution-quality W5): category for the breakdown event,
// token estimate, and the P1-3 stable/volatile classification. Accounting at
// the append site is exact where the marker scan below is a heuristic.
type PromptSection struct {
	Category    string // identity | tools | project_context | memory | runtime
	Tokens      int
	Stable      bool
	Fingerprint string // content hash for cache-prefix stability diagnostics
	Content     string // transient assembly-only copy used for detailed accounting
}

func newPromptSection(category, content string, stable bool) PromptSection {
	digest := sha256.Sum256([]byte(content))
	return PromptSection{
		Category:    category,
		Tokens:      estimateTokens(content),
		Stable:      stable,
		Fingerprint: hex.EncodeToString(digest[:8]),
		Content:     content,
	}
}

// BreakdownFromSections builds the breakdown from assembly-time accounting
// instead of re-parsing the joined prompt. Sections carry the prompt shares;
// messages contribute History exactly as before.
func BreakdownFromSections(sections []PromptSection, messages []llm.Message) ContextBreakdown {
	var b ContextBreakdown
	for _, s := range sections {
		switch s.Category {
		case "project_context":
			b.ProjectContext += s.Tokens
		case "tools":
			b.Tools += s.Tokens
		case "runtime":
			b.Runtime += s.Tokens
		case "recall":
			b.Recall += s.Tokens
		case "artifacts":
			b.Artifacts += s.Tokens
		case "memory":
			b.Memory += s.Tokens
		case "skill":
			b.Skill += s.Tokens
		default:
			b.Identity += s.Tokens
		}
	}
	for _, msg := range messages {
		if msg.Role == "system" {
			continue
		}
		if msg.Role == "tool" {
			b.ToolResults += estimateSingleMessageTokens(msg)
		} else {
			b.History += estimateSingleMessageTokens(msg)
		}
	}
	b.Total = b.Identity + b.Tools + b.ProjectContext + b.Memory + b.Skill + b.Runtime + b.Recall + b.Artifacts + b.History + b.ToolResults
	return b
}

// SplitRuntimePromptSections attributes the bounded runtime bundle without
// changing its rendered bytes. This keeps the prompt-cache prefix stable while
// making task state, workspace, recall, memory, and artifacts independently
// visible in diagnostics.
func SplitRuntimePromptSections(content string) []PromptSection {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	category := "runtime"
	parts := map[string]*strings.Builder{}
	order := make([]string, 0, 5)
	write := func(cat, line string) {
		b := parts[cat]
		if b == nil {
			b = &strings.Builder{}
			parts[cat] = b
			order = append(order, cat)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "## Workspace"):
			category = "project_context"
		case strings.HasPrefix(trimmed, "## Semantic Recall"),
			strings.HasPrefix(trimmed, "## [Recall"):
			category = "recall"
		case strings.HasPrefix(trimmed, "## Selected Indexed Memory"):
			category = "memory"
		case strings.HasPrefix(trimmed, "## Relevant Artifacts"):
			category = "artifacts"
		case strings.HasPrefix(trimmed, activeSkillPromptBegin),
			strings.HasPrefix(trimmed, "## Skill Candidates for Current Work Unit"):
			category = "skill"
		case strings.HasPrefix(trimmed, "## Delivery Continuity"),
			strings.HasPrefix(trimmed, "## Recent Events"),
			strings.HasPrefix(trimmed, "# DURABLE TASK CONTEXT"),
			strings.HasPrefix(trimmed, "## Current Summary"),
			strings.HasPrefix(trimmed, "## Next Steps"),
			strings.HasPrefix(trimmed, "## Open Blockers"),
			strings.HasPrefix(trimmed, "## Latest Handoff"):
			category = "runtime"
		}
		write(category, line)
	}
	sections := make([]PromptSection, 0, len(order))
	for _, cat := range order {
		sections = append(sections, newPromptSection(cat, parts[cat].String(), false))
	}
	return sections
}

// ProviderCallContextBreakdown reports the estimated request shape for one
// physical provider call. Provider usage remains authoritative for the total;
// these fields explain which SelfMind-owned components produced that total.
func ProviderCallContextBreakdown(sections []PromptSection, messages []llm.Message, tools []llm.ToolDefinition) map[string]interface{} {
	b := BreakdownFromSections(sections, messages)
	stableSystem := 0
	for _, section := range sections {
		if section.Stable {
			stableSystem += section.Tokens
		}
	}
	toolSchemaTokens := 0
	if raw, err := json.Marshal(tools); err == nil {
		toolSchemaTokens = estimateTokens(string(raw))
	}
	return map[string]interface{}{
		"stable_system":        stableSystem,
		"tool_schemas":         toolSchemaTokens,
		"history":              b.History,
		"current_tool_results": b.ToolResults,
		"recall":               b.Recall,
		"workspace":            b.ProjectContext,
		"artifacts":            b.Artifacts,
		"memory":               b.Memory,
		"skill":                b.Skill,
		"task_runtime":         b.Runtime,
		"estimated_total":      b.Total + toolSchemaTokens,
	}
}

func estimateSingleMessageTokens(msg llm.Message) int {
	total := estimateTokens(msg.Content)
	for _, part := range msg.MultiContent {
		total += estimateTokens(part.Text)
		if part.ImageURL != "" || part.Data != "" {
			// Images are provider-priced differently; record a conservative
			// placeholder so their presence is not silently invisible.
			total += 256
		}
	}
	for _, call := range msg.ToolCalls {
		total += estimateTokens(call.Function) + estimateTokens(call.Args)
	}
	return total
}

// StableVolatileTokens sums the accounted sections by mutability — the
// authoritative boundary of the P1-3 cacheable prefix.
func StableVolatileTokens(sections []PromptSection) (stable, volatile int) {
	for _, s := range sections {
		if s.Stable {
			stable += s.Tokens
		} else {
			volatile += s.Tokens
		}
	}
	return stable, volatile
}

// StablePrefixFingerprint identifies the ordered stable prompt prefix without
// exposing its content. A changed value between otherwise similar turns means
// provider-side prefix caching cannot be expected to hit.
func StablePrefixFingerprint(sections []PromptSection) string {
	h := sha256.New()
	count := 0
	for _, section := range sections {
		if !section.Stable {
			continue
		}
		fingerprint := section.Fingerprint
		if fingerprint == "" {
			// Legacy/manual sections without a content fingerprint still affect
			// the diagnostic deterministically through their metadata.
			fingerprint = section.Category + ":" + strconv.Itoa(section.Tokens)
		}
		_, _ = h.Write([]byte(fingerprint))
		_, _ = h.Write([]byte{0})
		count++
	}
	if count == 0 {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
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
		case "skill":
			b.Skill += tok
		default:
			b.Identity += tok
		}
	}
	for _, msg := range messages {
		if msg.Role == "system" {
			continue
		}
		if msg.Role == "tool" {
			b.ToolResults += estimateSingleMessageTokens(msg)
		} else {
			b.History += estimateSingleMessageTokens(msg)
		}
	}
	b.Total = b.Identity + b.Tools + b.ProjectContext + b.Memory + b.Skill + b.Runtime + b.Recall + b.Artifacts + b.History + b.ToolResults
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
		"skill":           b.Skill,
		"runtime":         b.Runtime,
		"recall":          b.Recall,
		"artifacts":       b.Artifacts,
		"history":         b.History,
		"tool_results":    b.ToolResults,
		"total":           b.Total,
	}
}
