package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

// TestBreakdownFromSections: assembly-time accounting maps categories and the
// stable/volatile split without re-parsing the joined prompt (W5).
func TestBreakdownFromSections(t *testing.T) {
	sections := []PromptSection{
		{Category: "identity", Tokens: 100, Stable: true},
		{Category: "tools", Tokens: 200, Stable: true},
		{Category: "project_context", Tokens: 300},
		{Category: "memory", Tokens: 50},
		{Category: "skill", Tokens: 40},
		{Category: "runtime", Tokens: 80},
	}
	messages := []llm.Message{
		{Role: "system", Content: "ignored"},
		{Role: "user", Content: "hello world"},
	}
	b := BreakdownFromSections(sections, messages)
	if b.Identity != 100 || b.Tools != 200 || b.ProjectContext != 300 || b.Memory != 50 || b.Skill != 40 || b.Runtime != 80 {
		t.Fatalf("category mapping wrong: %+v", b)
	}
	if b.History <= 0 {
		t.Fatal("non-system messages must count as history")
	}
	if b.Total != 770+b.History {
		t.Fatalf("total mismatch: %+v", b)
	}
	stable, volatile := StableVolatileTokens(sections)
	if stable != 300 || volatile != 470 {
		t.Fatalf("stable/volatile split wrong: %d/%d", stable, volatile)
	}
}

func TestSplitRuntimePromptSectionsAndProviderCallBreakdown(t *testing.T) {
	runtimePrompt := strings.Join([]string{
		"# SELECTED RUNTIME CONTEXT",
		"## Skill Candidates for Current Work Unit",
		"- release-inspection: inspect release metadata",
		"## Workspace",
		"workspace_root: /repo",
		"# DURABLE TASK CONTEXT",
		"status: in_progress",
		"## Relevant Artifacts",
		"- report.json",
		"## Semantic Recall",
		"- canonical mem_1: preferred report format",
		"## Selected Indexed Memory",
		"- user: concise replies",
	}, "\n")
	sections := append([]PromptSection{
		newPromptSection("identity", "stable persona", true),
	}, SplitRuntimePromptSections(runtimePrompt)...)
	payload := ProviderCallContextBreakdown(sections, []llm.Message{
		{Role: "system", Content: "ignored"},
		{Role: "user", Content: "prepare report"},
		{Role: "tool", Content: "bounded command output"},
	}, []llm.ToolDefinition{{Name: "read_file", Description: "Read one file"}})
	for _, key := range []string{"stable_system", "tool_schemas", "tool_schema_count", "history", "current_tool_results", "recall", "workspace", "artifacts", "memory", "skill", "task_runtime", "estimated_total"} {
		value, ok := payload[key].(int)
		if !ok || value <= 0 {
			t.Fatalf("expected positive %s in %+v", key, payload)
		}
	}
	if dynamic, ok := payload["dynamic_skill_tools"].(int); !ok || dynamic != 0 {
		t.Fatalf("dynamic Skill tool schemas=%v", payload["dynamic_skill_tools"])
	}
}

// TestBuildSystemPromptAccountsSections: the real assembly returns sections
// whose token sum tracks the joined prompt, and the stable prefix is
// non-empty (identity + guidance at minimum).
func TestBuildSystemPromptAccountsSections(t *testing.T) {
	agent := NewAgent(nil, nil, &fakeSummarizer{reply: "x"}, "You are SelfMind.", 4, 1, nil)
	prompt, sections, err := agent.buildSystemPrompt(context.Background(), "tenant", DefaultTaskStrategy(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) == 0 {
		t.Fatal("expected accounted sections")
	}
	sum := 0
	for _, s := range sections {
		if s.Tokens <= 0 {
			t.Fatalf("section with non-positive tokens: %+v", s)
		}
		sum += s.Tokens
	}
	promptTokens := estimateTokens(prompt)
	// Join separators make the totals differ slightly; accounting must stay
	// within a small tolerance of the joined prompt estimate.
	if sum < promptTokens*9/10 || sum > promptTokens*11/10 {
		t.Fatalf("accounted %d tokens vs joined prompt %d — drifted", sum, promptTokens)
	}
	stable, _ := StableVolatileTokens(sections)
	if stable == 0 {
		t.Fatal("stable prefix must be accounted")
	}
	if StablePrefixFingerprint(sections) == "" {
		t.Fatal("stable prefix fingerprint must be recorded")
	}
}

func TestStablePrefixFingerprintIgnoresVolatileChanges(t *testing.T) {
	base := []PromptSection{
		newPromptSection("identity", "stable persona", true),
		newPromptSection("tools", "stable tool contract", true),
		newPromptSection("runtime", "turn one", false),
	}
	changedVolatile := []PromptSection{
		newPromptSection("identity", "stable persona", true),
		newPromptSection("tools", "stable tool contract", true),
		newPromptSection("runtime", "turn two", false),
	}
	changedStable := []PromptSection{
		newPromptSection("identity", "changed persona", true),
		newPromptSection("tools", "stable tool contract", true),
		newPromptSection("runtime", "turn two", false),
	}
	if StablePrefixFingerprint(base) != StablePrefixFingerprint(changedVolatile) {
		t.Fatal("volatile suffix changes must not change the stable prefix fingerprint")
	}
	if StablePrefixFingerprint(base) == StablePrefixFingerprint(changedStable) {
		t.Fatal("stable content changes must change the stable prefix fingerprint")
	}
}
