package kernel

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

func TestProfileSynthesizerGatesAndSynthesizes(t *testing.T) {
	ctx := context.Background()
	store := &recordingMockStorage{}
	mem := memory.NewMemoryManager(store)
	prov := &mockProviderWithResponse{response: "User is a Go backend developer who prefers concise Chinese answers."}
	ps := NewProfileSynthesizer(prov, true)

	// Below the minimum fact count: must not synthesize (no model call effect).
	for i := 0; i < 3; i++ {
		_ = mem.AddFact(ctx, "t", "user", fmt.Sprintf("fact %d", i))
	}
	ps.MaybeSynthesize(ctx, "t", mem)
	if got, _ := mem.GetFacts(ctx, "t", "profile"); len(got) != 0 {
		t.Fatalf("should not synthesize below minFacts, got %d profile facts", len(got))
	}

	// Enough facts: synthesize once.
	for i := 3; i < 12; i++ {
		_ = mem.AddFact(ctx, "t", "user", fmt.Sprintf("fact %d", i))
	}
	ps.MaybeSynthesize(ctx, "t", mem)
	got, _ := mem.GetFacts(ctx, "t", "profile")
	if len(got) == 0 {
		t.Fatal("expected a profile to be synthesized after enough facts")
	}

	// latestProfileSummary should expose the synthesized prose.
	a := &Agent{memory: mem}
	if s := a.latestProfileSummary(ctx, "t"); !strings.Contains(s, "Go backend developer") {
		t.Fatalf("latestProfileSummary = %q", s)
	}
}

func TestProfileSynthesizerDisabledWithoutProvider(t *testing.T) {
	ctx := context.Background()
	store := &recordingMockStorage{}
	mem := memory.NewMemoryManager(store)
	for i := 0; i < 15; i++ {
		_ = mem.AddFact(ctx, "t", "user", fmt.Sprintf("fact %d", i))
	}
	ps := NewProfileSynthesizer(nil, true) // no provider -> disabled
	ps.MaybeSynthesize(ctx, "t", mem)
	if got, _ := mem.GetFacts(ctx, "t", "profile"); len(got) != 0 {
		t.Fatalf("disabled synthesizer must not write a profile, got %d", len(got))
	}
}

// capturingProvider records the last ChatCompletion prompt for assertions.
type capturingProvider struct {
	mockProvider
	lastUser string
	response string
}

func (c *capturingProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	for _, m := range messages {
		if m.Role == "user" {
			c.lastUser = m.Content
		}
	}
	return c.response, nil
}

// TestSynthesizeHonorsPinnedFacts proves pinned (authoritative) facts are sent
// to the synthesizer as ground truth it must not contradict — the human veto.
func TestSynthesizeHonorsPinnedFacts(t *testing.T) {
	ctx := context.Background()
	store := &recordingMockStorage{}
	mem := memory.NewMemoryManager(store)
	prov := &capturingProvider{response: "profile prose"}
	ps := NewProfileSynthesizer(prov, true)

	for i := 0; i < 12; i++ {
		_ = mem.AddFact(ctx, "t", "user", fmt.Sprintf("fact %d", i))
	}
	_ = mem.AddFact(ctx, "t", "pinned", "User primarily writes Go, not Rust.")

	ps.MaybeSynthesize(ctx, "t", mem)

	if !strings.Contains(prov.lastUser, "User primarily writes Go, not Rust.") {
		t.Fatalf("pinned fact missing from synthesis prompt:\n%s", prov.lastUser)
	}
	if !strings.Contains(prov.lastUser, "AUTHORITATIVE") || !strings.Contains(prov.lastUser, "never contradict") {
		t.Fatalf("pinned facts must be framed as authoritative/never-contradict:\n%s", prov.lastUser)
	}
}
