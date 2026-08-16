package kernel

import (
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

// TestComputeContextBreakdown pins P1-2 attribution: each system-prompt section
// lands in its category, history sums non-system messages, and Total is the sum.
func TestComputeContextBreakdown(t *testing.T) {
	sys := strings.Join([]string{
		"You are SelfMind, a capable agent.", // identity
		"",
		"# TOOL USE INSTRUCTIONS",
		"Use local tools. Treat errors as diagnostic evidence.",
		"",
		"# PROJECT CONTEXT",
		"[System note: workspace conventions]",
		"Always write tests. Use two-space indent.",
		"",
		"# SELECTED RUNTIME CONTEXT",
		activeSkillPromptBegin,
		"# ACTIVE SKILL FOR CURRENT WORK UNIT",
		"Inspect the release metadata with the bounded procedure.",
		"## Workspace",
		"workspace_root: /repo",
		"",
		"<memory-context>",
		"## User Preferences",
		"- prefers concise answers",
		"</memory-context>",
	}, "\n")
	msgs := []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: "please refactor the config loader end to end"},
		{Role: "assistant", Content: "Here is the plan and the result of the refactor."},
	}

	b := ComputeContextBreakdown(sys, msgs)

	if b.Identity <= 0 {
		t.Error("identity bucket should capture the soul line")
	}
	if b.Tools <= 0 {
		t.Error("tools bucket should capture the tool-use block")
	}
	if b.ProjectContext <= 0 {
		t.Error("project_context bucket should capture the AGENTS.md block")
	}
	if b.Runtime <= 0 {
		t.Error("runtime bucket should capture the selected-runtime block")
	}
	if b.Skill <= 0 {
		t.Error("skill bucket should capture the active-skill block")
	}
	if b.Memory <= 0 {
		t.Error("memory bucket should capture the memory-context block")
	}
	if b.History <= 0 {
		t.Error("history should sum the non-system messages")
	}
	// System message content is NOT counted as history (only user/assistant).
	want := b.Identity + b.Tools + b.ProjectContext + b.Memory + b.Skill + b.Runtime + b.History
	if b.Total != want {
		t.Fatalf("Total = %d, want sum of parts %d", b.Total, want)
	}

	// The payload round-trips the same numbers.
	p := b.Payload()
	if p["total"].(int) != b.Total || p["project_context"].(int) != b.ProjectContext {
		t.Fatalf("payload mismatch: %+v vs %+v", p, b)
	}
}

// TestComputeContextBreakdownEmpty: a bare prompt attributes everything to
// identity and produces a consistent zero-history total.
func TestComputeContextBreakdownEmpty(t *testing.T) {
	sys := "You are an agent."
	b := ComputeContextBreakdown(sys, []llm.Message{{Role: "system", Content: sys}})
	if b.History != 0 {
		t.Errorf("no user/assistant messages → history 0, got %d", b.History)
	}
	if b.Total != b.Identity {
		t.Errorf("bare prompt total should equal identity, got total=%d identity=%d", b.Total, b.Identity)
	}
}
