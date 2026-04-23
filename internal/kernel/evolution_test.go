package kernel

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/kernel/llm"
)

// MockProvider 实现 llm.Provider 接口
type mockProvider struct{}

func (m *mockProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "SKIP", nil
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
		Enabled:               true,
		Mode:                  "silent",
		MinComplexityThreshold: 2,
		NudgeInterval:         5,
	}
	reflector := NewReflectionEngine(&mockProvider{}, cfg)

	history := TaskHistory{
		Goal: "清理日志",
		Steps: []string{
			"Executed tool: terminal, result: find logs -name '*.log'",
			"Executed tool: terminal, result: tar -czf logs.tar.gz logs/",
			"Executed tool: terminal, result: rm -rf logs/",
			"Executed tool: terminal, result: done",
		},
		Outcome: "成功清理",
	}

	result, err := reflector.Reflect(context.Background(), history)
	if err != nil {
		t.Fatalf("Reflect failed: %v", err)
	}

	if result.Action != "skip" && result.Action != "create" && result.Action != "update" {
		t.Fatalf("Unexpected action: %s", result.Action)
	}

	if result.Action == "create" || result.Action == "update" {
		err = reflector.ArchiveSkill(context.Background(), result)
		if err != nil {
			t.Fatalf("ArchiveSkill failed: %v", err)
		}

		home, _ := os.UserHomeDir()
		files, _ := filepath.Glob(filepath.Join(home, ".selfmind", "skills", result.SkillName+".md"))
		if len(files) == 0 {
			t.Errorf("Expected skill file not found for %s", result.SkillName)
		}
	}
}

func TestComplexityAssessment(t *testing.T) {
	cfg := EvolutionConfig{Enabled: true, MinComplexityThreshold: 2}
	reflector := NewReflectionEngine(&mockProvider{}, cfg)

	tests := []struct {
		name     string
		steps    []string
		expected complexityLevel
	}{
		{
			name:     "trivial - single step",
			steps:    []string{"Executed tool: terminal, result: ls"},
			expected: complexityTrivial,
		},
		{
			name:     "low - two steps",
			steps:    []string{"step1", "step2"},
			expected: complexityLow,
		},
		{
			name:     "medium - 4 steps",
			steps:    []string{"step1", "step2", "step3", "step4"},
			expected: complexityMedium,
		},
		{
			name:     "medium - 3 tool types in 3 steps (only 2 unique)",
			steps:    []string{"Executed tool: terminal, result: x", "Executed tool: read_file, result: y", "Executed tool: terminal, result: z"},
			expected: complexityMedium,
		},
		{
			name:     "high - 3 unique tool types",
			steps:    []string{"Executed tool: terminal, result: x", "Executed tool: read_file, result: y", "Executed tool: grep, result: z"},
			expected: complexityHigh,
		},
		{
			name:     "high - 6+ steps",
			steps:    []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7"},
			expected: complexityHigh,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reflector.assessComplexity(tt.steps)
			if got != tt.expected {
				t.Errorf("assessComplexity() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestSkillNameSanitization(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"my-skill", "my-skill"},
		{"My Skill 123", "my-skill-123"},
		{"你好世界", "unnamed-skill"},
		{"", "unnamed-skill"},
		{"abc!@#def", "abc-def"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := SanitizeSkillName(tt.input)
			if got != tt.expected {
				t.Errorf("SanitizeSkillName(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestExtractSkillName(t *testing.T) {
	content := `---
name: my-test-skill
description: A test skill
---
# Content`

	got := extractSkillName(content)
	if got != "my-test-skill" {
		t.Errorf("extractSkillName() = %q, want %q", got, "my-test-skill")
	}
}
