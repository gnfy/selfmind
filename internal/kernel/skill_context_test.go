package kernel

import (
	"context"
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
	runtimeCtx := withActiveSkillRuntimeState(context.Background())
	setActiveSkillWorkUnitSequence(runtimeCtx, 1)
	messages := []llm.Message{{Role: "tool", Name: "skill_select", Content: `{"success":true,"work_unit_sequence":1,"instructions":"secret procedure"}`}}
	fallback := []toolExecutionResult{{toolName: "skill_fallback", success: true}}
	if !shouldExpireActiveSkillContext(runtimeCtx, []llm.ToolCall{{Function: "skill_fallback"}}, fallback) {
		t.Fatal("fallback did not expire active skill context")
	}
	expireActiveSkillToolResults(messages)
	if strings.Contains(messages[0].Content, "secret procedure") || !strings.Contains(messages[0].Content, "context_expired") {
		t.Fatalf("skill body remained visible: %s", messages[0].Content)
	}

	messages[0].Content = `{"success":true,"work_unit_sequence":1,"instructions":"procedure"}`
	plan := []llm.ToolCall{{Function: "update_plan", Args: `{"plan":[{"step":"A","status":"completed"},{"step":"B","status":"in_progress"}]}`}}
	results := []toolExecutionResult{{toolName: "update_plan", success: true}}
	if !shouldExpireActiveSkillContext(runtimeCtx, plan, results) {
		t.Fatal("work-unit switch did not expire prior skill context")
	}
}

func TestActiveSkillSequenceStaysDaemonSide(t *testing.T) {
	parent := WithSkillRuntimeContext(context.Background(), SkillRuntimeContext{Active: &ActiveSkillContext{
		Name: "inspect", WorkUnitSequence: 7,
	}})
	runtimeCtx := withActiveSkillRuntimeState(parent)
	if got := activeSkillWorkUnitSequence(runtimeCtx); got != 7 {
		t.Fatalf("active sequence=%d, want 7", got)
	}
	clearActiveSkillWorkUnitSequence(runtimeCtx)
	if got := activeSkillWorkUnitSequence(runtimeCtx); got != 0 {
		t.Fatalf("cleared sequence=%d, want 0", got)
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

func TestActiveSkillFallbackUpdatesComposedSystemMessage(t *testing.T) {
	skill := ActiveSkillContext{Name: "old-skill", Body: "OLD SECRET PROCEDURE"}
	original := "before\n" + skill.Prompt(2048) + "\nafter"
	messages := []llm.Message{{Role: "system", Content: original}, {Role: "user", Content: "continue"}}

	expired := expireActiveSkillMessages(messages, original)
	for label, value := range map[string]string{"systemPrompt": expired, "messages[0]": messages[0].Content} {
		if strings.Contains(value, "OLD SECRET PROCEDURE") || strings.Contains(value, activeSkillPromptBegin) {
			t.Fatalf("%s retained expired Skill instructions: %s", label, value)
		}
		if !strings.Contains(value, "earlier work unit's Active Skill context has expired") {
			t.Fatalf("%s did not retain the structural expiration notice: %s", label, value)
		}
	}
}
