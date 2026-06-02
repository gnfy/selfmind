package kernel

import (
	"context"
	"fmt"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type mockStorage struct{}

func (m *mockStorage) SaveTrajectory(ctx context.Context, tenantID, channel string, traj []byte) error {
	return nil
}
func (m *mockStorage) GetLatestContext(ctx context.Context, tenantID, channel string) ([][]byte, error) {
	return nil, nil
}
func (m *mockStorage) IndexMessagesFromTrajectory(ctx context.Context, tenantID, channel, sessionID string, messagesJSON []byte) error {
	return nil
}
func (m *mockStorage) SearchSessions(tenantID, query string, limit int) ([]memory.FTS5Session, error) {
	return nil, nil
}
func (m *mockStorage) ListRecentSessions(tenantID string, limit int) ([]memory.FTS5Session, error) {
	return nil, nil
}
func (m *mockStorage) GetSessionMessages(tenantID, sessionID string, aroundMessageID, window int) ([]memory.SessionMessage, error) {
	return nil, nil
}
func (m *mockStorage) SaveCheckpoint(ctx context.Context, tenantID, channel, name string, messages []byte) error {
	return nil
}
func (m *mockStorage) ListCheckpoints(ctx context.Context, tenantID, channel string) ([]memory.Checkpoint, error) {
	return nil, nil
}
func (m *mockStorage) LoadCheckpoint(ctx context.Context, tenantID, channel, name string) ([]byte, error) {
	return nil, nil
}
func (m *mockStorage) DeleteCheckpoint(ctx context.Context, tenantID, channel, name string) error {
	return nil
}
func (m *mockStorage) AddFact(ctx context.Context, tenantID string, target, content string) error {
	return nil
}
func (m *mockStorage) GetFacts(ctx context.Context, tenantID string, target string) ([]memory.Fact, error) {
	return nil, nil
}
func (m *mockStorage) RemoveFact(ctx context.Context, tenantID string, id string) error { return nil }
func (m *mockStorage) Close() error                                                     { return nil }

func (m *mockStorage) GetPermission(ctx context.Context, tenantID, toolName string) (bool, error) {
	return true, nil
}
func (m *mockStorage) SetPermission(ctx context.Context, tenantID, toolName string, allowed bool) error {
	return nil
}
func (m *mockStorage) SetSecret(ctx context.Context, tenantID, keyName, value string) error {
	return nil
}
func (m *mockStorage) GetSecret(ctx context.Context, tenantID, keyName string) (string, error) {
	return "", nil
}
func (m *mockStorage) SaveProcess(ctx context.Context, tenantID string, proc memory.ProcessRecord) error {
	return nil
}
func (m *mockStorage) UpdateProcessStatus(ctx context.Context, tenantID, id, status string, exitCode int) error {
	return nil
}
func (m *mockStorage) ListProcesses(ctx context.Context, tenantID string) ([]memory.ProcessRecord, error) {
	return nil, nil
}
func (m *mockStorage) GetProcess(ctx context.Context, tenantID, id string) (*memory.ProcessRecord, error) {
	return nil, nil
}
func (m *mockStorage) RecordSkillCall(ctx context.Context, tenantID, skillName string) error {
	return nil
}
func (m *mockStorage) RecordSkillFailure(ctx context.Context, tenantID, skillName string) error {
	return nil
}
func (m *mockStorage) ListSkillMetrics(ctx context.Context, tenantID string) ([]memory.SkillMetric, error) {
	return nil, nil
}
func (m *mockStorage) PruneSkills(ctx context.Context, tenantID string, thresholdDays int) (int, error) {
	return 0, nil
}
func (m *mockStorage) GetSkillMetric(ctx context.Context, tenantID, skillName string) (*memory.SkillMetric, error) {
	return nil, nil
}

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
func (b *mockBackend) GetToolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{}
}

type nativeToolLLMProvider struct {
	requests []llm.ChatRequest
}

func (p *nativeToolLLMProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "mock response", nil
}

func (p *nativeToolLLMProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}

func (p *nativeToolLLMProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.requests = append(p.requests, req)
	ch := make(chan llm.StreamEvent, 1)
	if len(p.requests) == 1 {
		ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{
			ID:       "call-1",
			Function: "read_file",
			Args:     `{"path":"README.md"}`,
		}}}
	} else {
		ch <- llm.StreamEvent{Content: "done"}
	}
	close(ch)
	return ch, nil
}

type recordingBackend struct {
	calledName string
	calledArgs map[string]interface{}
}

func (b *recordingBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	b.calledName = name
	b.calledArgs = args
	return "file content", nil
}

func (b *recordingBackend) GetToolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{{
		"type": "function",
		"function": map[string]interface{}{
			"name":        "read_file",
			"description": "Read a file",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"path"},
			},
		},
	}}
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

func TestAgentRunNativeToolCall(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &recordingBackend{}
	provider := &nativeToolLLMProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 3, 1, nil)

	ctx := memory.WithTenantID(context.Background(), "user123")
	res, _, err := agent.RunConversation(ctx, "user123", "cli", "read the readme")
	if err != nil {
		t.Fatalf("Agent failed: %v", err)
	}
	if res != "done" {
		t.Fatalf("response = %q, want done", res)
	}
	if len(provider.requests) < 2 {
		t.Fatalf("expected at least 2 llm requests, got %d", len(provider.requests))
	}
	if len(provider.requests[0].Tools) != 1 || provider.requests[0].Tools[0].Name != "read_file" {
		t.Fatalf("native tools were not passed to the provider: %+v", provider.requests[0].Tools)
	}
	if backend.calledName != "read_file" {
		t.Fatalf("calledName = %q, want read_file", backend.calledName)
	}
	if backend.calledArgs["path"] != "README.md" || backend.calledArgs["_tenant_id"] != "user123" {
		t.Fatalf("unexpected tool args: %+v", backend.calledArgs)
	}

	var toolMsg *llm.Message
	for i := range provider.requests[1].Messages {
		if provider.requests[1].Messages[i].Role == "tool" {
			toolMsg = &provider.requests[1].Messages[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatalf("second request did not include a structured tool result")
	}
	if toolMsg.ToolCallID != "call-1" || toolMsg.Name != "read_file" || toolMsg.Content != "file content" {
		t.Fatalf("unexpected tool message: %+v", *toolMsg)
	}
}
