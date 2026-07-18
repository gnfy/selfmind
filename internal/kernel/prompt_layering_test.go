package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/memory"
)

// TestPromptStablePrefixOrdering pins P1-3: the stable blocks (tool contract,
// tool defs) must come BEFORE the volatile blocks (runtime context, memory), so
// the cacheable prefix is maximized. The pre-P1-3 bug injected the volatile
// runtime context between soul and tools, splitting the prefix.
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
	runtimeIdx := strings.Index(prompt, "VOLATILE-RUNTIME-MARKER")
	if toolIdx < 0 {
		t.Fatal("tool block missing")
	}
	if runtimeIdx < 0 {
		t.Fatal("runtime context missing")
	}
	if toolIdx > runtimeIdx {
		t.Fatalf("stable tool block (%d) must precede volatile runtime context (%d)", toolIdx, runtimeIdx)
	}
}

// TestPromptPrefixStableAcrossMemoryChange verifies the whole point of the
// split: adding memory (volatile) must not change the stable prefix (everything
// up to the first volatile block). If it did, every memory write would bust the
// provider prompt cache.
func TestPromptPrefixStableAcrossMemoryChange(t *testing.T) {
	build := func(withFact bool) string {
		mem := memory.NewMemoryManager(nil)
		if withFact {
			_ = mem.AddFact(context.Background(), "tenant", "user", "prefers concise answers")
		}
		agent := NewAgent(mem, promptToolBackend{}, &textOnlyProvider{}, "You are SelfMind.", 1, 1, nil)
		p, _, err := agent.buildSystemPrompt(context.Background(), "tenant", DefaultTaskStrategy(), "do some work")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	without := build(false)
	with := build(true)

	// The stable prefix is everything before the first volatile marker. With no
	// runtime context, memory is the first volatile block; without any memory,
	// the whole prompt is stable. Compare the shared stable head.
	stableHead := without
	if idx := strings.Index(with, "<memory-context>"); idx >= 0 {
		stableHead = with[:idx]
		// The version without the fact must start with the identical stable head.
		if !strings.HasPrefix(without, stableHead) {
			t.Fatal("adding a memory fact changed the stable prefix — prompt cache would bust every turn")
		}
		return
	}
	// Memory fence disabled or no fact rendered: at minimum the tool block must
	// be byte-identical between the two builds.
	toolBlock := func(s string) string {
		i := strings.Index(s, "# TOOL USE INSTRUCTIONS")
		if i < 0 {
			return ""
		}
		return s[i:]
	}
	_ = stableHead
	// Memory fence disabled / no fact rendered: at minimum the stable tool
	// block must be byte-identical between the two builds.
	if a, b := toolBlock(without), toolBlock(with); a != "" && a != b {
		t.Fatal("stable tool block diverged across a memory change")
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
