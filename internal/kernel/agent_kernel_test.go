package kernel

import (
	"context"
	"fmt"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type mockStorage struct{}
func (m *mockStorage) SaveTrajectory(ctx context.Context, tenantID, channel string, traj []byte) error { return nil }
func (m *mockStorage) GetLatestContext(ctx context.Context, tenantID, channel string) ([][]byte, error) { return nil, nil }
func (m *mockStorage) IndexMessagesFromTrajectory(ctx context.Context, tenantID, channel, sessionID string, messagesJSON []byte) error { return nil }
func (m *mockStorage) SearchSessions(tenantID, query string, limit int) ([]memory.FTS5Session, error) { return nil, nil }
func (m *mockStorage) SaveCheckpoint(ctx context.Context, tenantID, channel, name string, messages []byte) error { return nil }
func (m *mockStorage) ListCheckpoints(ctx context.Context, tenantID, channel string) ([]memory.Checkpoint, error) { return nil, nil }
func (m *mockStorage) LoadCheckpoint(ctx context.Context, tenantID, channel, name string) ([]byte, error) { return nil, nil }
func (m *mockStorage) DeleteCheckpoint(ctx context.Context, tenantID, channel, name string) error { return nil }
func (m *mockStorage) AddFact(ctx context.Context, tenantID string, target, content string) error { return nil }
func (m *mockStorage) GetFacts(ctx context.Context, tenantID string, target string) ([]memory.Fact, error) { return nil, nil }
func (m *mockStorage) RemoveFact(ctx context.Context, tenantID string, id string) error { return nil }
func (m *mockStorage) Close() error { return nil }

type mockLLMProvider struct{}
func (p *mockLLMProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "mock response", nil
}
func (p *mockLLMProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "mock response"}, nil
}
func (p *mockLLMProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Content: "mock response"}
	close(ch)
	return ch, nil
}

// mockBackend implements AgentBackend for test purposes (avoids importing tools package)
type mockBackend struct{}
func (b *mockBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	return "mock dispatch: " + name, nil
}

func TestAgentRun(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &mockBackend{}
	provider := &mockLLMProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 1, 1, nil)

	ctx := memory.WithTenantID(context.Background(), "user123")
	res, _, err := agent.RunConversation(ctx, "user123", "cli", "hello")
	if err != nil {
		t.Fatalf("Agent failed: %v", err)
	}
	fmt.Printf("Result: %s\n", res)
}
