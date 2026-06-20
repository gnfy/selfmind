package kernel

import (
	"context"
	"fmt"
	"io"
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

type streamEOFThenChatProvider struct {
	streamRequests int
	chatRequests   int
}

func (p *streamEOFThenChatProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "fallback response", nil
}

func (p *streamEOFThenChatProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.chatRequests++
	return &llm.ChatResponse{
		Content: "fallback response",
		Usage: llm.UsageStats{
			InputTokens:  1,
			OutputTokens: 2,
		},
	}, nil
}

func (p *streamEOFThenChatProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.streamRequests++
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Err: io.ErrUnexpectedEOF}
	close(ch)
	return ch, nil
}

type streamStartEOFThenChatProvider struct {
	streamRequests int
	chatRequests   int
}

func (p *streamStartEOFThenChatProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "fallback response", nil
}

func (p *streamStartEOFThenChatProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.chatRequests++
	return &llm.ChatResponse{Content: "fallback response"}, nil
}

func (p *streamStartEOFThenChatProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.streamRequests++
	return nil, io.ErrUnexpectedEOF
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

type recordingLLMProvider struct {
	requests []llm.ChatRequest
}

func (p *recordingLLMProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "done", nil
}

func (p *recordingLLMProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.requests = append(p.requests, req)
	return &llm.ChatResponse{Content: "done"}, nil
}

func (p *recordingLLMProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.requests = append(p.requests, req)
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Content: "done"}
	close(ch)
	return ch, nil
}

type planningBackend struct{}

func (b *planningBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	return "ok", nil
}

func (b *planningBackend) GetToolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "update_plan",
				"description": "Update plan",
				"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "read_file",
				"description": "Read a file",
				"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
		},
	}
}

type recordingBackend struct {
	calledName string
	calledArgs map[string]interface{}
}

func TestSimpleRequestDoesNotExposeUpdatePlan(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &planningBackend{}
	provider := &recordingLLMProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 1, 1, nil)

	_, _, err := agent.RunConversation(context.Background(), "user123", "cli", "用Go写一个二分法")
	if err != nil {
		t.Fatalf("Agent failed: %v", err)
	}
	if len(provider.requests) == 0 {
		t.Fatal("provider received no requests")
	}
	for _, tool := range provider.requests[0].Tools {
		if tool.Name == "update_plan" {
			t.Fatalf("update_plan should not be exposed for simple requests: %+v", provider.requests[0].Tools)
		}
	}
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

func TestAgentFallsBackToNonStreamingWhenStreamFailsBeforeOutput(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &mockBackend{}
	provider := &streamEOFThenChatProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 1, 2, nil)

	ctx := memory.WithTenantID(context.Background(), "user123")
	res, usage, err := agent.RunConversation(ctx, "user123", "cli", "hello")
	if err != nil {
		t.Fatalf("Agent failed: %v", err)
	}
	if res != "fallback response" {
		t.Fatalf("expected fallback response, got %q", res)
	}
	if provider.streamRequests != 1 {
		t.Fatalf("expected 1 stream request, got %d", provider.streamRequests)
	}
	if provider.chatRequests != 1 {
		t.Fatalf("expected 1 non-stream fallback request, got %d", provider.chatRequests)
	}
	if usage.InputTokens != 1 || usage.OutputTokens != 2 {
		t.Fatalf("expected fallback usage 1/2, got %d/%d", usage.InputTokens, usage.OutputTokens)
	}
}

func TestAgentFallsBackToNonStreamingWhenStreamStartFails(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &mockBackend{}
	provider := &streamStartEOFThenChatProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 1, 2, nil)

	ctx := memory.WithTenantID(context.Background(), "user123")
	res, _, err := agent.RunConversation(ctx, "user123", "cli", "hello")
	if err != nil {
		t.Fatalf("Agent failed: %v", err)
	}
	if res != "fallback response" {
		t.Fatalf("expected fallback response, got %q", res)
	}
	if provider.streamRequests != 2 {
		t.Fatalf("expected 2 stream attempts before fallback, got %d", provider.streamRequests)
	}
	if provider.chatRequests != 1 {
		t.Fatalf("expected 1 non-stream fallback request, got %d", provider.chatRequests)
	}
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
