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

	prompt, err := agent.buildSystemPrompt(ctx, "tenant", DefaultTaskStrategy(), "do some work")
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
		p, err := agent.buildSystemPrompt(context.Background(), "tenant", DefaultTaskStrategy(), "do some work")
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
