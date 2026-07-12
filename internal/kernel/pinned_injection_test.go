package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/memory"
)

// TestPinnedFactsAlwaysInjected pins the pinned-visibility contract: pinned
// memory is user-confirmed ground truth, so it must reach every prompt, ahead
// of extracted facts, and outside the bounded selection slots. (Regression:
// buildSystemPrompt once read only the user/memory targets, so /memory pin
// stored facts the model never saw.)
func TestPinnedFactsAlwaysInjected(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	ctx := context.Background()
	if err := mem.AddFact(ctx, "tenant", "pinned", "The user's canonical build command is GOWORK=off go test"); err != nil {
		t.Fatal(err)
	}
	if err := mem.AddFact(ctx, "tenant", "user", "prefers concise answers"); err != nil {
		t.Fatal(err)
	}

	agent := NewAgent(mem, promptToolBackend{}, &textOnlyProvider{}, "You are SelfMind.", 1, 1, nil)
	prompt, _, err := agent.buildSystemPrompt(ctx, "tenant", DefaultTaskStrategy(), "hello")
	if err != nil {
		t.Fatal(err)
	}

	pinnedIdx := strings.Index(prompt, "The user's canonical build command is GOWORK=off go test")
	if pinnedIdx < 0 {
		t.Fatal("pinned fact missing from system prompt")
	}
	if userIdx := strings.Index(prompt, "prefers concise answers"); userIdx >= 0 && pinnedIdx > userIdx {
		t.Fatalf("pinned fact (%d) must precede extracted facts (%d)", pinnedIdx, userIdx)
	}
}

// TestPinnedFactsNotTruncatedBySelection: pinned facts do not compete with the
// bounded per-target selection; every pinned fact is injected even when the
// extracted-fact slots are saturated.
func TestPinnedFactsNotTruncatedBySelection(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	ctx := context.Background()
	for i := 0; i < 30; i++ { // saturate the user-target selection budget
		if err := mem.AddFact(ctx, "tenant", "user", "extracted preference variant number "+strings.Repeat("x", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mem.AddFact(ctx, "tenant", "pinned", "PINNED-MARKER-FACT survives saturation"); err != nil {
		t.Fatal(err)
	}

	agent := NewAgent(mem, promptToolBackend{}, &textOnlyProvider{}, "You are SelfMind.", 1, 1, nil)
	prompt, _, err := agent.buildSystemPrompt(ctx, "tenant", DefaultTaskStrategy(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "PINNED-MARKER-FACT survives saturation") {
		t.Fatal("pinned fact was dropped when extracted-fact selection saturated")
	}
}
