package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
)

// countingReviewProvider proves that a legacy skill-only maintenance job is
// drained before it can spend a model call or reach skill_manage.
type countingReviewProvider struct{ calls int }

func (p *countingReviewProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	p.calls++
	return "", nil
}

func (p *countingReviewProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls++
	return &llm.ChatResponse{}, nil
}

func (p *countingReviewProvider) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.calls++
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

func TestLegacySkillReviewIsDrainedWithoutMutation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	registry := NewRegistry()
	dispatcher := NewDispatcherWithRegistry(registry)
	dispatcher.RegisterTool(NewSkillManageTool())
	dispatcher.RegisterTool(NewSkillViewTool())
	provider := &countingReviewProvider{}
	engine := kernel.NewBackgroundReviewEngine(nil, dispatcher, provider, kernel.EvolutionConfig{Enabled: true}, 4, 1)

	payload, err := json.Marshal(kernel.ReviewJobPayload{
		Channel:      "cli",
		ReviewSkills: true,
		Messages: []kernel.ReviewMessage{
			{Role: "user", Content: "turn this workflow into a reusable skill"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := engine.RunReviewFromPayload(context.Background(), "person_review", string(payload))
	if err != nil {
		t.Fatalf("run legacy review: %v", err)
	}
	if !strings.Contains(summary, "legacy skill review is disabled") {
		t.Fatalf("unexpected summary: %q", summary)
	}
	if provider.calls != 0 {
		t.Fatalf("legacy skill review used %d model calls, want 0", provider.calls)
	}

	dir, err := getSkillsDir("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "review-flow")); !os.IsNotExist(err) {
		t.Fatalf("legacy review unexpectedly wrote a skill: %v", err)
	}
}
