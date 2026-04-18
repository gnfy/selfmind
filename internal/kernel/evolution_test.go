package kernel

import (
	"context"
	"os"
	"path/filepath"
	"selfmind/internal/kernel/llm"
	"testing"
)

// MockProvider 实现 llm.Provider 接口
type mockProvider struct{}

func (m *mockProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "---\nname: log-archiver\n---\n# Log Archiver\n\n1. Find logs\n2. Compress\n3. Cleanup", nil
}

func (m *mockProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "mock"}, nil
}

func (m *mockProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Content: "mock"}
	close(ch)
	return ch, nil
}

func TestEvolutionIntegration(t *testing.T) {
	cfg := EvolutionConfig{
		Enabled:                true,
		Mode:                   "silent",
		MinComplexityThreshold: 2,
	}
	reflector := &ReflectionEngine{
		Provider: &mockProvider{},
		Config:   cfg,
	}

	history := TaskHistory{
		Goal: "清理日志",
		Steps: []string{
			"find logs -name '*.log'",
			"tar -czf logs.tar.gz logs/",
			"rm -rf logs/",
		},
		Outcome: "成功清理",
	}

	should, content, err := reflector.Reflect(context.Background(), history)
	if err != nil {
		t.Fatalf("Reflect failed: %v", err)
	}

	if should {
		err = reflector.ArchiveSkill(content)
		if err != nil {
			t.Fatalf("ArchiveSkill failed: %v", err)
		}
	}

	home, _ := os.UserHomeDir()
	files, _ := filepath.Glob(filepath.Join(home, ".selfmind", "skills", "auto-skill-*.md"))
	if len(files) == 0 {
		t.Error("Expected skill file not found")
	}
}
