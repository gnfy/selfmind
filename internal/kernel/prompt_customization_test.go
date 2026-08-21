package kernel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel/memory"
	"selfmind/internal/promptassets"
)

type fixedPromptBackend struct {
	defs []map[string]interface{}
}

func (b fixedPromptBackend) Dispatch(string, map[string]interface{}) (string, error) {
	return "", nil
}

func (b fixedPromptBackend) GetToolDefinitions() []map[string]interface{} {
	return b.defs
}

func TestMissingPromptFilesPreserveSystemPromptBytes(t *testing.T) {
	build := func(snapshot *promptassets.Snapshot) string {
		agent := NewAgent(memory.NewMemoryManager(nil), promptToolBackend{}, &textOnlyProvider{}, "You are SelfMind.", 1, 1, nil)
		agent.SetPromptSnapshot(snapshot)
		prompt, _, err := agent.buildSystemPrompt(context.Background(), "tenant", DefaultTaskStrategy(), "build a frontend page")
		if err != nil {
			t.Fatal(err)
		}
		return prompt
	}
	baseline := build(nil)
	snapshot, err := promptassets.Load(filepath.Join(t.TempDir(), "prompts"))
	if err != nil {
		t.Fatal(err)
	}
	if got := build(snapshot); got != baseline {
		t.Fatal("an empty prompt workspace changed the default system prompt")
	}
}

func TestForegroundPromptPreviewExplainsEmptyPersona(t *testing.T) {
	preview := ForegroundPromptDefaults("")
	if !strings.Contains(preview, "no legacy agent.soul persona is configured") || !strings.Contains(preview, "agent.md / Persona") {
		t.Fatalf("empty persona preview is ambiguous:\n%s", preview)
	}
}

func TestInterfaceGuidanceDoesNotUseKeywordClassification(t *testing.T) {
	build := func(input string) string {
		agent := NewAgent(memory.NewMemoryManager(nil), promptToolBackend{}, &textOnlyProvider{}, "", 1, 1, nil)
		prompt, _, err := agent.buildSystemPrompt(context.Background(), "tenant", DefaultTaskStrategy(), input)
		if err != nil {
			t.Fatal(err)
		}
		return prompt
	}
	ordinary := build("please guide me through the migration and build the parser")
	interfaceWork := build("replace the account settings screen")
	if ordinary != interfaceWork {
		t.Fatal("system prompt must not vary with an incomplete frontend keyword taxonomy")
	}
	for _, want := range []string{"# USER-FACING INTERFACE QUALITY (conditional)", "Ignore it for all other work"} {
		if !strings.Contains(ordinary, want) {
			t.Fatalf("semantic applicability boundary missing %q:\n%s", want, ordinary)
		}
	}
}

func TestForegroundPromptCustomizationKeepsLockedQualityFloor(t *testing.T) {
	root := t.TempDir()
	content := `## Persona

Use concise Chinese.

## Working Style

Prefer small reversible edits.

## Progress Updates

off
`
	if err := os.WriteFile(filepath.Join(root, "agent.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := promptassets.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	agent := NewAgent(memory.NewMemoryManager(nil), promptToolBackend{}, &textOnlyProvider{}, "old soul", 1, 1, nil)
	agent.SetPromptSnapshot(snapshot)
	prompt, _, err := agent.buildSystemPrompt(context.Background(), "tenant", DefaultTaskStrategy(), "do work")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Use concise Chinese.", "Prefer small reversible edits.", "# RESPONSE & INTERACTION", "# WORK QUALITY & VERIFICATION"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	if strings.Contains(prompt, "old soul") || strings.Contains(prompt, "# PROGRESS NARRATION") {
		t.Fatalf("replace/off semantics were not applied:\n%s", prompt)
	}
}

func TestForegroundPromptKeepsDeliveryAndQualityWithoutTools(t *testing.T) {
	agent := NewAgent(memory.NewMemoryManager(nil), nil, &textOnlyProvider{}, "", 1, 1, nil)
	prompt, _, err := agent.buildSystemPrompt(context.Background(), "tenant", DefaultTaskStrategy(), "answer directly")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# RESPONSE & INTERACTION",
		"Respond in the language of the user's latest message",
		"# WORK QUALITY & VERIFICATION",
		"Never claim work was completed or verified when it was not",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("tool-free foreground prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBackgroundReviewUsesOnlyItsBoundedRoleContract(t *testing.T) {
	agent := NewAgent(memory.NewMemoryManager(nil), promptToolBackend{}, &textOnlyProvider{}, "review soul", 1, 1, nil)
	agent.SetPromptProfile(PromptProfileBackgroundReview)
	prompt, _, err := agent.buildSystemPrompt(context.Background(), "tenant", DefaultTaskStrategy(), "build frontend")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "review soul") {
		t.Fatal("background role soul missing")
	}
	for _, excluded := range []string{"# RESPONSE & INTERACTION", "# WORK QUALITY & VERIFICATION", "# Persistent Learning Guidance", "# PROGRESS NARRATION", "# USER-FACING INTERFACE QUALITY", "update_plan", "finish_run", "watch_external", "tool_search"} {
		if strings.Contains(prompt, excluded) {
			t.Errorf("background review inherited unrelated foreground guidance %q", excluded)
		}
	}
}

// TestBackgroundReviewIgnoresOperatorPromptLayer pins the trust boundary that
// the profile does own: operator agent.md sections never reach a background
// role, even though the locked floors do.
func TestBackgroundReviewIgnoresOperatorPromptLayer(t *testing.T) {
	root := t.TempDir()
	content := `## Persona

Operator persona override.

## Working Style

Operator working style addition.

## Learning Preferences

Operator learning addition.
`
	if err := os.WriteFile(filepath.Join(root, "agent.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := promptassets.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	agent := NewAgent(memory.NewMemoryManager(nil), promptToolBackend{}, &textOnlyProvider{}, "review soul", 1, 1, nil)
	agent.SetPromptSnapshot(snapshot)
	agent.SetPromptProfile(PromptProfileBackgroundReview)
	prompt, _, err := agent.buildSystemPrompt(context.Background(), "tenant", DefaultTaskStrategy(), "review the turn")
	if err != nil {
		t.Fatal(err)
	}
	for _, excluded := range []string{"Operator persona override.", "Operator working style addition.", "Operator learning addition."} {
		if strings.Contains(prompt, excluded) {
			t.Errorf("operator prompt layer reached a background role: %q", excluded)
		}
	}
	if !strings.Contains(prompt, "review soul") {
		t.Error("the background role soul must survive an operator Persona override")
	}
	if strings.Contains(prompt, "# WORK QUALITY & VERIFICATION") || strings.Contains(prompt, "# Persistent Learning Guidance") {
		t.Error("background role must use its exact role contract rather than foreground floors")
	}
}

func TestDelegationProfileInheritsQualityButPreservesRoleIdentity(t *testing.T) {
	root := t.TempDir()
	content := `## Persona

Primary persona override.

## Working Style

Prefer delegated verification evidence.
`
	if err := os.WriteFile(filepath.Join(root, "agent.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := promptassets.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	agent := NewAgent(memory.NewMemoryManager(nil), promptToolBackend{}, &textOnlyProvider{}, "specialized delegation soul", 1, 1, nil)
	agent.SetPromptProfile(PromptProfileDelegation)
	agent.SetPromptSnapshot(snapshot)
	prompt, _, err := agent.buildSystemPrompt(context.Background(), "tenant", DefaultTaskStrategy(), "inspect the code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "specialized delegation soul") || !strings.Contains(prompt, "Prefer delegated verification evidence.") {
		t.Fatalf("delegation prompt lost identity or operator quality:\n%s", prompt)
	}
	if strings.Contains(prompt, "Primary persona override.") {
		t.Fatalf("primary persona replaced delegation role identity:\n%s", prompt)
	}
	for _, excluded := range []string{"# PROGRESS NARRATION", "# Persistent Learning Guidance"} {
		if strings.Contains(prompt, excluded) {
			t.Fatalf("delegation inherited primary-only guidance %q:\n%s", excluded, prompt)
		}
	}
}

func TestCapabilityAwareToolPromptMentionsOnlyAvailableLifecycleTools(t *testing.T) {
	def := func(name string) map[string]interface{} {
		return map[string]interface{}{"name": name, "description": name, "parameters": map[string]interface{}{"type": "object"}}
	}
	strategy := DefaultTaskStrategy()
	watcher := buildToolUsePrompt([]map[string]interface{}{def("read_file"), def("watch_external")}, true, strategy, PromptProfileForeground)
	if !strings.Contains(watcher, "waiting_external") || !strings.Contains(watcher, "watch_external") {
		t.Fatalf("available watcher contract missing:\n%s", watcher)
	}
	for _, absent := range []string{"update_plan", "finish_run", "tool_search", "command failures"} {
		if strings.Contains(watcher, absent) {
			t.Fatalf("prompt invented unavailable capability %q:\n%s", absent, watcher)
		}
	}
}

func TestUpdatePlanGuidanceMatchesWorkUnitBoundary(t *testing.T) {
	def := func(name string) map[string]interface{} {
		return map[string]interface{}{"name": name, "description": name, "parameters": map[string]interface{}{"type": "object"}}
	}
	prompt := buildToolUsePrompt([]map[string]interface{}{def("read_file"), def("update_plan")}, true, DefaultTaskStrategy(), PromptProfileForeground)
	if !strings.Contains(prompt, "Call update_plan by itself") || !strings.Contains(prompt, "do not batch it with reads or other tools") {
		t.Fatalf("update_plan prompt does not explain the enforced boundary:\n%s", prompt)
	}
}

func TestCapabilityChangesDoNotChangeAssembledStableHead(t *testing.T) {
	backend := fixedPromptBackend{defs: []map[string]interface{}{
		{"name": "read_file", "description": "read", "parameters": map[string]interface{}{"type": "object"}},
		{"name": "update_plan", "description": "plan", "parameters": map[string]interface{}{"type": "object"}},
	}}
	agent := NewAgent(memory.NewMemoryManager(nil), backend, &nativeToolsProvider{}, "", 1, 1, nil)
	direct := DefaultTaskStrategy()
	direct.PlanPolicy = PlanPolicyDisabled
	multi := DefaultTaskStrategy()
	multi.PlanPolicy = PlanPolicyRequired

	_, directSections, err := agent.buildSystemPrompt(context.Background(), "tenant", direct, "answer this")
	if err != nil {
		t.Fatal(err)
	}
	_, multiSections, err := agent.buildSystemPrompt(context.Background(), "tenant", multi, "change several files")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := StablePrefixFingerprint(directSections), StablePrefixFingerprint(multiSections); got != want {
		t.Fatalf("capability/strategy change altered stable head: direct=%s multi=%s", got, want)
	}
	if stable, _ := StableVolatileTokens(directSections); stable == 0 {
		t.Fatal("stable prompt head unexpectedly empty")
	}
}

func TestPersistentLearningGuidanceMentionsOnlyAvailableSurfaces(t *testing.T) {
	defs := []map[string]interface{}{{"name": "session_search"}}
	guidance := selfImprovementGuidanceForDefinitions(defs)
	if !strings.Contains(guidance, "session_search") {
		t.Fatalf("available learning surface missing: %s", guidance)
	}
	for _, absent := range []string{"skill_manage", "memory:"} {
		if strings.Contains(guidance, absent) {
			t.Fatalf("learning prompt invented unavailable surface %q: %s", absent, guidance)
		}
	}
}

func TestSetContextWindowPreservesPromptSnapshotForSummarizer(t *testing.T) {
	snapshot := promptassets.Empty(filepath.Join(t.TempDir(), "prompts"))
	agent := NewAgent(memory.NewMemoryManager(nil), promptToolBackend{}, &textOnlyProvider{}, "", 1, 1, nil)
	agent.SetPromptSnapshot(snapshot)
	agent.SetContextWindow(16384)
	if agent.contextEngine == nil || agent.contextEngine.promptSnapshot != snapshot {
		t.Fatal("context-window reconfiguration discarded the process prompt snapshot")
	}
}

func TestBackgroundReviewPromptExplainsSessionSearch(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		reviewMemory, skills bool
	}{
		{name: "memory", reviewMemory: true},
		{name: "skills-only", skills: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := buildBackgroundReviewPrompt(nil, tc.reviewMemory, tc.skills)
			if !strings.Contains(prompt, "Use session_search when the conversation refers to earlier sessions") {
				t.Fatalf("background review lost session_search usage guidance:\n%s", prompt)
			}
		})
	}
}
