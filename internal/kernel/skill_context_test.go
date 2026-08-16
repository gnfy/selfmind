package kernel

import (
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
)

func TestActiveSkillPromptIsBoundedAndAuthorityNeutral(t *testing.T) {
	ctx := ActiveSkillContext{
		ActivationID: "activation_1", WorkUnitID: "wu_1", Key: "skill_1", Name: "deploy",
		VersionHash: "v1", Scope: "workspace", Source: "agent-created",
		Body: strings.Repeat("step\n", 5000), LinkedFiles: []string{"references/checks.md"},
	}
	prompt := ctx.Prompt(4096)
	if len(prompt) > 4096 || !strings.Contains(prompt, "grants no tool") || !strings.Contains(prompt, "skill_fallback") {
		t.Fatalf("unexpected active skill prompt len=%d:\n%s", len(prompt), prompt)
	}
}

func TestSkillContextExpiresOnFallbackOrWorkUnitSwitch(t *testing.T) {
	messages := []llm.Message{{Role: "tool", Name: "skill_select", Content: `{"success":true,"work_unit_sequence":1,"instructions":"secret procedure"}`}}
	fallback := []toolExecutionResult{{toolName: "skill_fallback", success: true}}
	if !shouldExpireActiveSkillContext([]llm.ToolCall{{Function: "skill_fallback"}}, fallback, messages) {
		t.Fatal("fallback did not expire active skill context")
	}
	expireActiveSkillToolResults(messages)
	if strings.Contains(messages[0].Content, "secret procedure") || !strings.Contains(messages[0].Content, "context_expired") {
		t.Fatalf("skill body remained visible: %s", messages[0].Content)
	}

	messages[0].Content = `{"success":true,"work_unit_sequence":1,"instructions":"procedure"}`
	plan := []llm.ToolCall{{Function: "update_plan", Args: `{"plan":[{"step":"A","status":"completed"},{"step":"B","status":"in_progress"}]}`}}
	results := []toolExecutionResult{{toolName: "update_plan", success: true}}
	if !shouldExpireActiveSkillContext(plan, results, messages) {
		t.Fatal("work-unit switch did not expire prior skill context")
	}
}

func TestInitialActiveSkillSystemPromptExpiresStructurally(t *testing.T) {
	skill := ActiveSkillContext{Name: "old-skill", Body: "OLD SECRET PROCEDURE"}
	prompt := "before\n" + skill.Prompt(2048) + "\nafter"
	got := expireActiveSkillSystemPrompt(prompt)
	if strings.Contains(got, "OLD SECRET PROCEDURE") || strings.Contains(got, activeSkillPromptBegin) || !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("active Skill prompt did not expire cleanly: %s", got)
	}
}
