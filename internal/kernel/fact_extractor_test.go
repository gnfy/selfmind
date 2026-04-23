package kernel

import (
	"context"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// mockProviderWithResponse extends the package-level mockProvider to return custom responses.
type mockProviderWithResponse struct {
	mockProvider
	response string
	err      error
}

func (m *mockProviderWithResponse) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return m.response, nil
}

// recordingMockStorage records facts for test verification.
type recordingMockStorage struct {
	mockStorage
	facts []memory.Fact
}

func (m *recordingMockStorage) AddFact(ctx context.Context, tenantID, target, content string) error {
	m.facts = append(m.facts, memory.Fact{Target: target, Content: content})
	return nil
}
func (m *recordingMockStorage) GetFacts(ctx context.Context, tenantID, target string) ([]memory.Fact, error) {
	var result []memory.Fact
	for _, f := range m.facts {
		if f.Target == target {
			result = append(result, f)
		}
	}
	return result, nil
}

func TestFactExtractor_Extract(t *testing.T) {
	ctx := context.Background()

	provider := &mockProviderWithResponse{
		response: `{"user_facts":["User prefers pnpm over npm","User likes dark mode"],"memory_facts":["Project uses Go 1.26","Tests must be in _test.go files"]}`,
	}

	store := &recordingMockStorage{}
	mem := memory.NewMemoryManager(store)

	fe := NewFactExtractor(provider, true)

	messages := []llm.Message{
		{Role: "user", Content: "Help me set up this Go project. I prefer pnpm and dark mode."},
		{Role: "assistant", Content: "Sure! This project uses Go 1.26. I'll set it up for you."},
		{Role: "tool", Content: "go mod init selfmind"},
		{Role: "assistant", Content: "Done. Remember tests must be in _test.go files."},
	}

	err := fe.Extract(ctx, "test-tenant", mem, messages)
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	// Should have persisted user facts and memory facts
	if len(store.facts) != 4 {
		t.Fatalf("Expected 4 facts, got %d: %+v", len(store.facts), store.facts)
	}

	var userCount, memCount int
	for _, f := range store.facts {
		if f.Target == "user" {
			userCount++
		}
		if f.Target == "memory" {
			memCount++
		}
	}
	if userCount != 2 {
		t.Errorf("Expected 2 user facts, got %d", userCount)
	}
	if memCount != 2 {
		t.Errorf("Expected 2 memory facts, got %d", memCount)
	}
}

func TestFactExtractor_Deduplication(t *testing.T) {
	ctx := context.Background()

	provider := &mockProviderWithResponse{
		response: `{"user_facts":["User prefers pnpm"],"memory_facts":[]}`,
	}

	store := &recordingMockStorage{
		facts: []memory.Fact{
			{Target: "user", Content: "User prefers pnpm"},
		},
	}
	mem := memory.NewMemoryManager(store)

	fe := NewFactExtractor(provider, true)

	messages := []llm.Message{
		{Role: "user", Content: "I prefer pnpm."},
		{Role: "assistant", Content: "Noted."},
		{Role: "tool", Content: "ok"},
	}

	err := fe.Extract(ctx, "test-tenant", mem, messages)
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	// Should NOT add duplicate
	if len(store.facts) != 1 {
		t.Errorf("Expected 1 fact (no duplicate), got %d", len(store.facts))
	}
}

func TestFactExtractor_SkipCasualChat(t *testing.T) {
	ctx := context.Background()
	provider := &mockProviderWithResponse{response: `{"user_facts":[],"memory_facts":[]}`}
	store := &recordingMockStorage{}
	mem := memory.NewMemoryManager(store)
	fe := NewFactExtractor(provider, true)

	// No tool calls, only 2 turns — should skip
	messages := []llm.Message{
		{Role: "user", Content: "Hi"},
		{Role: "assistant", Content: "Hello!"},
	}

	err := fe.Extract(ctx, "test-tenant", mem, messages)
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}

	if len(store.facts) != 0 {
		t.Errorf("Expected 0 facts for casual chat, got %d", len(store.facts))
	}
}

func TestLevenshteinRatio(t *testing.T) {
	tests := []struct {
		a, b   string
		expect float64
	}{
		{"hello", "hello", 1.0},
		{"hello", "helo", 0.8},
		{"hello", "world", 0.2},
		{"", "", 1.0},
		{"", "x", 0.0},
	}

	for _, tc := range tests {
		ratio := levenshteinRatio(tc.a, tc.b)
		if ratio < tc.expect-0.01 || ratio > tc.expect+0.01 {
			t.Errorf("levenshteinRatio(%q, %q) = %.2f, want %.2f", tc.a, tc.b, ratio, tc.expect)
		}
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		input  string
		expect string
	}{
		{"Some text ```json{\"a\":1}```", "{\"a\":1}"},
		{"{\"a\":1}", "{\"a\":1}"},
		{"prefix{\"a\":1}suffix", "{\"a\":1}"},
	}

	for _, tc := range tests {
		got := extractJSON(tc.input)
		if !strings.Contains(got, tc.expect) && got != tc.expect {
			t.Errorf("extractJSON(%q) = %q, want %q", tc.input, got, tc.expect)
		}
	}
}
