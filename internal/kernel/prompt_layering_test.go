package kernel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel/memory"
)

// TestPromptStablePrefixOrdering pins P1-3: static behavior stays before the
// volatile suffix, while capability/strategy-derived tool guidance joins the
// suffix before selected Run state without being mislabeled as cache-stable.
func TestPromptStablePrefixOrdering(t *testing.T) {
	mem := memory.NewMemoryManager(nil)
	agent := NewAgent(mem, promptToolBackend{}, &textOnlyProvider{}, "You are SelfMind.", 1, 1, nil)

	rc := TaskRuntimeContext{TaskID: "task_x", Title: "VOLATILE-RUNTIME-MARKER"}
	ctx := WithTaskRuntimeContext(context.Background(), rc)

	prompt, _, err := agent.buildSystemPrompt(ctx, "tenant", DefaultTaskStrategy(), "do some work")
	if err != nil {
		t.Fatal(err)
	}
	toolIdx := strings.Index(prompt, "# TOOL USE INSTRUCTIONS")
	progressIdx := strings.Index(prompt, "# PROGRESS NARRATION")
	runtimeIdx := strings.Index(prompt, "VOLATILE-RUNTIME-MARKER")
	if toolIdx < 0 || progressIdx < 0 {
		t.Fatal("prompt guidance block missing")
	}
	if runtimeIdx < 0 {
		t.Fatal("runtime context missing")
	}
	if progressIdx > runtimeIdx {
		t.Fatalf("stable progress block (%d) must precede volatile runtime context (%d)", progressIdx, runtimeIdx)
	}
	if toolIdx > runtimeIdx {
		t.Fatalf("capability-derived tool block (%d) must precede changing runtime context (%d)", toolIdx, runtimeIdx)
	}
}

// A fresh Run changes selected state, not the tool contract or repository
// instructions. Pin the real system-prompt prefix used by the loop.
func TestRunStateDoesNotPrecedeReusablePromptContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(path, []byte("PROJECT-CONVENTION: keep generated files intact"), 0600); err != nil {
		t.Fatal(err)
	}
	agent := NewAgent(memory.NewMemoryManager(nil), promptToolBackend{}, &textOnlyProvider{}, "You are SelfMind.", 1, 1, nil)
	agent.contextScanner = NewContextScanner()
	build := func(runID, correction string) string {
		ctx := WithWorkspaceContext(context.Background(), WorkspaceContext{ID: "workspace", Root: root})
		runtime := TaskRuntimeContext{RunID: runID, Title: "Inspect the same project", UserRequirements: []string{correction}}
		ctx = WithRuntimeContextBundle(WithTaskRuntimeContext(ctx, runtime), RuntimeContextBundle{Task: &runtime, Budget: DefaultRuntimeContextBudget()})
		prompt, _, err := agent.buildSystemPrompt(ctx, "tenant", DefaultTaskStrategy(), "inspect")
		if err != nil {
			t.Fatal(err)
		}
		return prompt
	}
	first := build("run-first", "Do not publish")
	second := build("run-second", "Do not change credentials")
	cut := strings.Index(first, "# SELECTED RUNTIME CONTEXT")
	if cut < 0 {
		t.Fatal("selected context missing")
	}
	for _, marker := range []string{"# TOOL USE INSTRUCTIONS", "PROJECT-CONVENTION"} {
		if index := strings.Index(first, marker); index < 0 || index > cut {
			t.Fatalf("reusable %q is missing or after changing Run state", marker)
		}
	}
	if !strings.HasPrefix(second, first[:cut]) || !strings.Contains(second, "Do not change credentials") || strings.Contains(second, "run-first") {
		t.Fatal("changing Run state rewrote the reusable prefix or retained stale state")
	}
	t.Logf("unchanged system prefix before selected Run state: %d bytes", cut)
	if err := os.WriteFile(path, []byte("PROJECT-CONVENTION: require review before publishing"), 0600); err != nil {
		t.Fatal(err)
	}
	changed := build("run-third", "Do not publish")
	if strings.HasPrefix(changed, first[:cut]) || !strings.Contains(changed, "require review before publishing") {
		t.Fatal("changed project instructions reused stale context")
	}
}

// TestPromptPrefixStableAcrossMemoryChange verifies the whole point of the
// split: adding memory (volatile) must not change the stable prefix (everything
// up to the first volatile block). If it did, every memory write would bust the
// provider prompt cache.
func TestPromptPrefixStableAcrossMemoryChange(t *testing.T) {
	build := func(withFact bool) []PromptSection {
		mem := memory.NewMemoryManager(nil)
		if withFact {
			_ = mem.AddFact(context.Background(), "tenant", "user", "prefers concise answers")
		}
		agent := NewAgent(mem, promptToolBackend{}, &textOnlyProvider{}, "You are SelfMind.", 1, 1, nil)
		_, sections, err := agent.buildSystemPrompt(context.Background(), "tenant", DefaultTaskStrategy(), "do some work")
		if err != nil {
			t.Fatal(err)
		}
		return sections
	}

	without := build(false)
	with := build(true)
	if a, b := StablePrefixFingerprint(without), StablePrefixFingerprint(with); a != b {
		t.Fatalf("adding memory changed the stable prefix: without=%s with=%s", a, b)
	}
}

// TestPromptByteStableAcrossConsecutiveTurns is a prompt-cache diagnostic:
// building the system prompt twice for the SAME run state must produce
// byte-identical output — stable prefix AND volatile suffix. Any per-turn
// variation (e.g. a wall-clock timestamp rendered into the prompt) would bust
// the provider prompt cache on every turn even though nothing changed.
func TestPromptByteStableAcrossConsecutiveTurns(t *testing.T) {
	mem := memory.NewMemoryManager(nil)
	if err := mem.AddFact(context.Background(), "tenant", "user", "prefers concise answers"); err != nil {
		t.Logf("AddFact: %v", err)
	}
	agent := NewAgent(mem, promptToolBackend{}, &textOnlyProvider{}, "You are SelfMind.", 1, 1, nil)

	ctx := WithWorkspaceContext(context.Background(), WorkspaceContext{ID: "ws_1", Root: "/tmp/ws"})
	ctx = WithTaskRuntimeContext(ctx, TaskRuntimeContext{TaskID: "task_x", Title: "same run state"})

	first, _, err := agent.buildSystemPrompt(ctx, "tenant", DefaultTaskStrategy(), "do some work")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := agent.buildSystemPrompt(ctx, "tenant", DefaultTaskStrategy(), "do some work")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		return
	}
	// Locate the first divergence so the failure names the varying section.
	i := 0
	for i < len(first) && i < len(second) && first[i] == second[i] {
		i++
	}
	lo := i - 80
	if lo < 0 {
		lo = 0
	}
	hiFirst, hiSecond := i+80, i+80
	if hiFirst > len(first) {
		hiFirst = len(first)
	}
	if hiSecond > len(second) {
		hiSecond = len(second)
	}
	t.Fatalf("system prompt varies across consecutive turns for the same run state at byte %d:\nfirst:  ...%q...\nsecond: ...%q...",
		i, first[lo:hiFirst], second[lo:hiSecond])
}
