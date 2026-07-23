package kernel

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

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
func (m *mockStorage) AddFactMeta(ctx context.Context, tenantID string, f memory.Fact) error {
	return nil
}
func (m *mockStorage) GetFacts(ctx context.Context, tenantID string, target string) ([]memory.Fact, error) {
	return nil, nil
}
func (m *mockStorage) RemoveFact(ctx context.Context, tenantID string, id string) error { return nil }
func (m *mockStorage) TouchFact(ctx context.Context, tenantID, id string, confidence float64, verifiedAt time.Time) error {
	return nil
}
func (m *mockStorage) Close() error { return nil }

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

type outputLimitProvider struct {
	requests []llm.ChatRequest
}

func (p *outputLimitProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "summary", nil
}

func (p *outputLimitProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "fallback"}, nil
}

func (p *outputLimitProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.requests = append(p.requests, req)
	ch := make(chan llm.StreamEvent, 3)
	if len(p.requests) == 1 {
		ch <- llm.StreamEvent{Content: "part ", FinishReason: "max_tokens"}
	} else {
		ch <- llm.StreamEvent{Content: "finish", FinishReason: "end_turn"}
	}
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

type xmlFallbackToolProvider struct {
	requests []llm.ChatRequest
}

func (p *xmlFallbackToolProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return "done", nil
}

func (p *xmlFallbackToolProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}

func (p *xmlFallbackToolProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.requests = append(p.requests, req)
	ch := make(chan llm.StreamEvent, 4)
	if len(p.requests) == 1 {
		ch <- llm.StreamEvent{Content: "I need to inspect it.\n"}
		ch <- llm.StreamEvent{Content: "<tool>list_dir</tool>"}
		ch <- llm.StreamEvent{Content: "\n<parameter>{\"path\":\"/repo\",\"recursive\":true}</parameter>"}
		ch <- llm.StreamEvent{Content: "\nThis trailing text should not be streamed."}
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

type planReconciliationProvider struct {
	requests []llm.ChatRequest
}

func (p *planReconciliationProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "done", nil
}

func (p *planReconciliationProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}

func (p *planReconciliationProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.requests = append(p.requests, req)
	ch := make(chan llm.StreamEvent, 1)
	switch len(p.requests) {
	case 1:
		ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{
			ID: "plan-initial", Function: "update_plan",
			Args: `{"plan":[{"step":"implement","status":"completed"},{"step":"verify","status":"in_progress"}]}`,
		}}}
	case 2:
		ch <- llm.StreamEvent{Content: "Everything is done."}
	case 3:
		ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{
			ID: "plan-final", Function: "update_plan",
			Args: `{"plan":[{"step":"implement","status":"completed"},{"step":"verify","status":"completed"}]}`,
		}}}
	default:
		ch <- llm.StreamEvent{Content: "Implemented and verified."}
	}
	close(ch)
	return ch, nil
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
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "web_search",
				"description": "Search the web",
				"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "web_extract",
				"description": "Extract a webpage",
				"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
		},
	}
}

type recordingBackend struct {
	calledName string
	calledArgs map[string]interface{}
}

type budgetBackend struct {
	calls []string
}

func (b *budgetBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	b.calls = append(b.calls, fmt.Sprint(args["path"]))
	return "read " + fmt.Sprint(args["path"]), nil
}

func (b *budgetBackend) GetToolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{{
		"type": "function",
		"function": map[string]interface{}{
			"name": "read_file", "description": "read",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}},
				"required":   []string{"path"},
			},
		},
	}}
}

type budgetProvider struct {
	requests int
}

func (p *budgetProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "done", nil
}

func (p *budgetProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}

func (p *budgetProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.requests++
	ch := make(chan llm.StreamEvent, 1)
	if requestHasTool(req, "read_file") {
		ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{
			ID: "budget-call-" + fmt.Sprint(p.requests), Function: "read_file",
			Args: fmt.Sprintf(`{"path":"file-%d.go"}`, p.requests),
		}}}
	} else {
		ch <- llm.StreamEvent{Content: "done"}
	}
	close(ch)
	return ch, nil
}

type budgetClosureBackend struct{}

func (b *budgetClosureBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	switch name {
	case "read_file":
		return "read evidence", nil
	case "update_plan":
		return `{"changed":true}`, nil
	case "finish_run":
		return `{"status":"done","summary":"implemented and verified"}`, nil
	default:
		return "", fmt.Errorf("unexpected tool %s", name)
	}
}

func (b *budgetClosureBackend) GetToolDefinitions() []map[string]interface{} {
	definition := func(name string) map[string]interface{} {
		return map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": name, "description": name,
				"parameters": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
		}
	}
	return []map[string]interface{}{definition("read_file"), definition("update_plan"), definition("finish_run")}
}

type budgetClosureProvider struct{ requests int }

func (p *budgetClosureProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "done", nil
}

func (p *budgetClosureProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}

func (p *budgetClosureProvider) StreamChat(_ context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.requests++
	ch := make(chan llm.StreamEvent, 1)
	switch p.requests {
	case 1:
		ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{
			{ID: "plan-open", Function: "update_plan", Args: `{"plan":[{"step":"inspect","status":"completed"},{"step":"verify","status":"in_progress"}]}`},
			{ID: "read", Function: "read_file", Args: `{"path":"target.go"}`},
		}}
	case 2:
		ch <- llm.StreamEvent{Content: "The implementation is ready."}
	case 3:
		ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{
			ID: "plan-close", Function: "update_plan",
			Args: `{"plan":[{"step":"inspect","status":"completed"},{"step":"verify","status":"completed"}]}`,
		}}}
	case 4:
		ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{
			ID: "finish", Function: "finish_run", Args: `{"status":"done","summary":"implemented and verified"}`,
		}}}
	default:
		ch <- llm.StreamEvent{Content: "Implemented and verified."}
	}
	close(ch)
	return ch, nil
}

func TestToolBudgetExtendsOnlyWithinHardLimitAndMarksIncomplete(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &budgetBackend{}
	provider := &budgetProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 10, 1, nil)
	events := make(chan string, 64)
	ctx := WithEventChannel(context.Background(), events)
	ctx = WithTaskStrategy(ctx, TaskStrategy{
		Class: TaskClassRepoTask, ToolMode: ToolModeFull, PlanPolicy: PlanPolicyOptional,
		MaxActionTools: 1, ActionToolBudgetStep: 1, ActionToolBudgetLimit: 3, MaxBudgetExtensions: 2,
	})

	if _, _, err := agent.RunConversation(ctx, "user123", "cli", "inspect three files"); err != nil {
		t.Fatal(err)
	}
	close(events)
	if len(backend.calls) != 3 {
		t.Fatalf("tool calls = %d (%v), want hard limit 3", len(backend.calls), backend.calls)
	}
	extensions := 0
	completion := AgentEvent{}
	for raw := range events {
		event, ok := DecodeAgentEvent(raw)
		if !ok {
			continue
		}
		if event.Type == "strategy.budget_extended" {
			extensions++
		}
		if event.Type == "turn.completed" {
			completion = event
		}
	}
	if extensions != 2 {
		t.Fatalf("extensions = %d, want 2", extensions)
	}
	if completion.Payload["completion_reason"] != "tool_budget_exhausted" || completion.Payload["resumable"] != true {
		t.Fatalf("completion = %+v", completion)
	}
}

func TestActionBudgetExhaustionStillAllowsLifecycleClosure(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	provider := &budgetClosureProvider{}
	agent := NewAgent(mem, &budgetClosureBackend{}, provider, "helpful", 8, 1, nil)
	events := make(chan string, 64)
	ctx := WithEventChannel(context.Background(), events)
	ctx = WithTaskStrategy(ctx, TaskStrategy{
		Class: TaskClassRepoTask, ToolMode: ToolModeFull, PlanPolicy: PlanPolicyRequired,
		MaxActionTools: 1, ActionToolBudgetStep: 1, ActionToolBudgetLimit: 1, MaxBudgetExtensions: 1,
	})

	if _, _, err := agent.RunConversation(ctx, "user123", "cli", "inspect, implement, and verify"); err != nil {
		t.Fatal(err)
	}
	close(events)
	completion := AgentEvent{}
	for raw := range events {
		event, ok := DecodeAgentEvent(raw)
		if ok && event.Type == "turn.completed" {
			completion = event
		}
	}
	if completion.Payload["completion_reason"] != "completed" || completion.Payload["resumable"] == true {
		t.Fatalf("completion = %+v", completion)
	}
	if provider.requests < 5 {
		t.Fatalf("provider requests = %d, lifecycle closure was cut short", provider.requests)
	}
}

func TestSimpleRequestKeepsOptionalPlanButHidesWebTools(t *testing.T) {
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
	if !requestHasTool(provider.requests[0], "update_plan") {
		t.Fatalf("update_plan should stay available as optional agent-first planning: %+v", provider.requests[0].Tools)
	}
	if requestHasTool(provider.requests[0], "web_search") {
		t.Fatalf("web_search should not be exposed for stable simple requests: %+v", provider.requests[0].Tools)
	}
}

func TestAgentReconcilesUnresolvedPlanBeforeCompleting(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	provider := &planReconciliationProvider{}
	agent := NewAgent(mem, &planningBackend{}, provider, "helpful", 6, 1, nil)
	events := make(chan string, 64)
	ctx := WithEventChannel(context.Background(), events)
	ctx = WithTaskStrategy(ctx, TaskStrategy{
		Class: TaskClassRepoTask, ToolMode: ToolModeFull, PlanPolicy: PlanPolicyRequired,
		MaxActionTools: 2, ActionToolBudgetStep: 1, ActionToolBudgetLimit: 4, MaxBudgetExtensions: 1,
	})

	answer, _, err := agent.RunConversation(ctx, "user123", "cli", "implement and verify the change")
	if err != nil {
		t.Fatal(err)
	}
	close(events)
	if answer != "Implemented and verified." {
		t.Fatalf("answer = %q", answer)
	}
	if len(provider.requests) != 4 {
		t.Fatalf("provider requests = %d, want 4", len(provider.requests))
	}
	repairPromptSeen := false
	for _, msg := range provider.requests[2].Messages {
		if msg.Role == "user" && strings.Contains(msg.Content, "reconcile the visible plan") {
			repairPromptSeen = true
		}
	}
	if !repairPromptSeen {
		t.Fatal("plan reconciliation prompt was not injected")
	}
	completion := AgentEvent{}
	for raw := range events {
		if event, ok := DecodeAgentEvent(raw); ok && event.Type == "turn.completed" {
			completion = event
		}
	}
	if completion.Payload["completion_reason"] != "completed" {
		t.Fatalf("completion = %+v", completion)
	}
}

func TestStableCodingAdviceDoesNotExposeWebTools(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &planningBackend{}
	provider := &recordingLLMProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 1, 1, nil)

	_, _, err := agent.RunConversation(context.Background(), "user123", "cli", "给一个Go学习的方案，要求结合当前AI编程的情况。")
	if err != nil {
		t.Fatalf("Agent failed: %v", err)
	}
	if len(provider.requests) == 0 {
		t.Fatal("provider received no requests")
	}
	if requestHasTool(provider.requests[0], "web_search") || requestHasTool(provider.requests[0], "web_extract") {
		t.Fatalf("web tools should not be exposed for stable coding advice: %+v", provider.requests[0].Tools)
	}
}

func TestExplicitLookupKeepsWebTools(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &planningBackend{}
	provider := &recordingLLMProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 1, 1, nil)

	_, _, err := agent.RunConversation(context.Background(), "user123", "cli", "查一下 Go 当前最新稳定版本。")
	if err != nil {
		t.Fatalf("Agent failed: %v", err)
	}
	if len(provider.requests) == 0 {
		t.Fatal("provider received no requests")
	}
	if !requestHasTool(provider.requests[0], "web_search") {
		t.Fatalf("web_search should be available for explicit lookup: %+v", provider.requests[0].Tools)
	}
}

func requestHasTool(req llm.ChatRequest, name string) bool {
	for _, tool := range req.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func TestSystemPromptGuidesToolFailureDiagnosis(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &planningBackend{}
	provider := &recordingLLMProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 1, 1, nil)

	prompt, err := agent.BuildSystemPrompt(context.Background(), "user123")
	if err != nil {
		t.Fatalf("BuildSystemPrompt failed: %v", err)
	}
	if !strings.Contains(prompt, "When a tool returns an error, treat the error as diagnostic evidence") {
		t.Fatalf("prompt missing tool failure recovery guidance: %q", prompt)
	}
	if !strings.Contains(prompt, "Do not hard-code environment overrides as a default tool behavior") {
		t.Fatalf("prompt missing generic env override guidance: %q", prompt)
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

func TestAgentContinuesAfterOutputLimit(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &mockBackend{}
	provider := &outputLimitProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 3, 1, nil)

	res, _, err := agent.RunConversation(context.Background(), "user123", "cli", "write a longer answer")
	if err != nil {
		t.Fatalf("Agent failed: %v", err)
	}
	if res != "part finish" {
		t.Fatalf("response = %q, want concatenated continuation", res)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(provider.requests))
	}
	lastReq := provider.requests[1]
	if len(lastReq.Messages) == 0 || !strings.Contains(lastReq.Messages[len(lastReq.Messages)-1].Content, "Continue from the exact point") {
		t.Fatalf("second request did not ask model to continue: %+v", lastReq.Messages)
	}
}

func TestAgentRunNativeToolCall(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &recordingBackend{}
	provider := &nativeToolLLMProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 3, 1, nil)

	eventCh := make(chan string, 20)
	ctx := WithEventChannel(memory.WithTenantID(context.Background(), "user123"), eventCh)
	ctx = WithWorkspaceContext(ctx, WorkspaceContext{ID: "workspace-123", Root: t.TempDir()})
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
	if backend.calledArgs["path"] != "README.md" || backend.calledArgs["_tenant_id"] != "user123" || backend.calledArgs["_workspace_id"] != "workspace-123" {
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

	events := drainAgentEvents(eventCh)
	if !agentEventsContain(events, "agent.thinking") {
		t.Fatalf("agent.thinking event missing: %+v", events)
	}
	if !agentEventsContain(events, "tool.started") {
		t.Fatalf("tool.started event missing: %+v", events)
	}
}

func TestSelectedCanonicalAccessIDsOnlyIncludesInjectedFacts(t *testing.T) {
	selected := selectedCanonicalAccessIDs(
		[]string{"pinned", "selected", "not-selected"},
		[]memory.Fact{{ID: "pinned"}},
		[]memory.Fact{{ID: "selected"}, {ID: "legacy"}, {ID: "selected"}},
	)
	if got, want := strings.Join(selected, ","), "pinned,selected"; got != want {
		t.Fatalf("selected ids = %q, want %q", got, want)
	}
}

func TestAgentRunXMLFallbackToolCallDoesNotStreamRawMarkup(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	backend := &recordingBackend{}
	provider := &xmlFallbackToolProvider{}
	agent := NewAgent(mem, backend, provider, "helpful", 3, 1, nil)

	eventCh := make(chan string, 20)
	ctx := WithEventChannel(memory.WithTenantID(context.Background(), "user123"), eventCh)
	res, _, err := agent.RunConversation(ctx, "user123", "cli", "inspect current project")
	if err != nil {
		t.Fatalf("Agent failed: %v", err)
	}
	if res != "done" {
		t.Fatalf("response = %q, want done", res)
	}
	if backend.calledName != "ls_r" {
		t.Fatalf("calledName = %q, want ls_r", backend.calledName)
	}
	if backend.calledArgs["path"] != "/repo" {
		t.Fatalf("unexpected tool args: %+v", backend.calledArgs)
	}
	if len(provider.requests) < 2 {
		t.Fatalf("expected second LLM request after tool call")
	}
	foundAssistantToolCall := false
	for _, msg := range provider.requests[1].Messages {
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}
		foundAssistantToolCall = true
		if strings.Contains(msg.Content, "<tool>") || strings.Contains(msg.Content, "<parameter>") {
			t.Fatalf("assistant tool markup was not stripped: %+v", msg)
		}
	}
	if !foundAssistantToolCall {
		t.Fatalf("second request did not include assistant tool call: %+v", provider.requests[1].Messages)
	}
	for _, event := range drainAgentEvents(eventCh) {
		if event.Type == "stream" && (strings.Contains(event.Content, "<tool>") || strings.Contains(event.Content, "<parameter>")) {
			t.Fatalf("raw tool markup leaked to stream: %+v", event)
		}
		if event.Type == "stream" && strings.Contains(event.Content, "trailing text") {
			t.Fatalf("post-tool text should not stream before tool execution: %+v", event)
		}
	}
}

func drainAgentEvents(ch <-chan string) []AgentEvent {
	var events []AgentEvent
	for {
		select {
		case raw := <-ch:
			if event, ok := DecodeAgentEvent(raw); ok {
				events = append(events, event)
			}
		default:
			return events
		}
	}
}

func agentEventsContain(events []AgentEvent, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

// stepOutcomeTrace collects the ordered outcome of each agent.step event —
// the mid-loop state machine's transition sequence (P0-B).
func stepOutcomeTrace(events []AgentEvent) []string {
	var trace []string
	for _, e := range events {
		if e.Type != "agent.step" {
			continue
		}
		if outcome, ok := e.Payload["outcome"].(string); ok {
			trace = append(trace, outcome)
		}
	}
	return trace
}

// TestLoopEmitsTypedStepTrace pins the mid-loop state machine: a
// tool-call-then-answer turn traces execute_tools → complete_turn, and the
// terminal step carries the resolved completion reason.
func TestLoopEmitsTypedStepTrace(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	agent := NewAgent(mem, &recordingBackend{}, &nativeToolLLMProvider{}, "helpful", 3, 1, nil)
	eventCh := make(chan string, 64)
	ctx := WithEventChannel(memory.WithTenantID(context.Background(), "user123"), eventCh)
	ctx = WithWorkspaceContext(ctx, WorkspaceContext{ID: "ws", Root: t.TempDir()})
	if _, _, err := agent.RunConversation(ctx, "user123", "cli", "read the readme"); err != nil {
		t.Fatal(err)
	}
	trace := stepOutcomeTrace(drainAgentEvents(eventCh))
	want := []string{string(StepExecuteTools), string(StepCompleteTurn)}
	if len(trace) != len(want) {
		t.Fatalf("step trace = %v, want %v", trace, want)
	}
	for i := range want {
		if trace[i] != want[i] {
			t.Fatalf("step trace[%d] = %q, want %q (full %v)", i, trace[i], want[i], trace)
		}
	}
}

// TestBudgetExhaustionStepTrace pins the transition sequence when the tool
// budget forces a finalize-from-evidence: the loop continues under
// budget_exhausted_finalize then completes, and the terminal step reason is
// tool_budget_exhausted.
func TestBudgetExhaustionStepTrace(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	agent := NewAgent(mem, &budgetBackend{}, &budgetProvider{}, "helpful", 10, 1, nil)
	eventCh := make(chan string, 128)
	ctx := WithEventChannel(context.Background(), eventCh)
	ctx = WithTaskStrategy(ctx, TaskStrategy{
		Class: TaskClassRepoTask, ToolMode: ToolModeFull, PlanPolicy: PlanPolicyOptional,
		MaxActionTools: 1, ActionToolBudgetStep: 1, ActionToolBudgetLimit: 3, MaxBudgetExtensions: 2,
	})
	if _, _, err := agent.RunConversation(ctx, "user123", "cli", "inspect three files"); err != nil {
		t.Fatal(err)
	}
	events := drainAgentEvents(eventCh)
	trace := stepOutcomeTrace(events)
	if len(trace) == 0 || trace[len(trace)-1] != string(StepCompleteTurn) {
		t.Fatalf("trace must end in complete_turn: %v", trace)
	}
	var completion AgentEvent
	for _, e := range events {
		if e.Type == "turn.completed" {
			completion = e
		}
	}
	if completion.Payload["completion_reason"] != "tool_budget_exhausted" {
		t.Fatalf("completion = %+v (trace %v)", completion.Payload, trace)
	}
}

// bigOutputBackend returns a large tool output to grow the window past a small
// budget within a run.
type bigOutputBackend struct{ calls int }

func (b *bigOutputBackend) Dispatch(name string, args map[string]interface{}) (string, error) {
	b.calls++
	return strings.Repeat("x", 4000), nil
}
func (b *bigOutputBackend) GetToolDefinitions() []map[string]interface{} { return nil }

// multiToolProvider asks for read_file on the first N stream requests, then
// answers — driving several loop iterations that each append a large tool
// result.
type multiToolProvider struct {
	requests  int
	toolTurns int
}

func (p *multiToolProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "done", nil
}
func (p *multiToolProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}
func (p *multiToolProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.requests++
	ch := make(chan llm.StreamEvent, 1)
	if p.requests <= p.toolTurns {
		ch <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{
			ID:       fmt.Sprintf("call-%d", p.requests),
			Function: "read_file",
			Args:     fmt.Sprintf(`{"path":"f%d.txt"}`, p.requests),
		}}}
	} else {
		ch <- llm.StreamEvent{Content: "final answer"}
	}
	close(ch)
	return ch, nil
}

// TestMidTurnCompactionFiresWithinRun pins P0-C: a growing window is compacted
// WITHIN the run (a compact_context step + a context.compacted event), and the
// run still completes — no provider context-window rejection needed.
func TestMidTurnCompactionFiresWithinRun(t *testing.T) {
	mem := memory.NewMemoryManager(&mockStorage{})
	provider := &multiToolProvider{toolTurns: 10}
	agent := NewAgent(mem, &bigOutputBackend{}, provider, "helpful", 18, 1, nil)
	// Leave enough room for several tool-call/result pairs before crossing the
	// threshold. A 4K window is now smaller than the stable system prefix and
	// exercises deterministic emergency trimming rather than middle compaction.
	agent.SetContextWindow(6000)
	agent.SetSummaryProvider(&fakeSummarizer{reply: "## Active Task\ncontinue the work\n## Relevant Files\n- f1.txt"})

	eventCh := make(chan string, 256)
	ctx := WithEventChannel(memory.WithTenantID(context.Background(), "user123"), eventCh)
	ctx = WithWorkspaceContext(ctx, WorkspaceContext{ID: "ws", Root: t.TempDir()})
	answer, _, err := agent.RunConversation(ctx, "user123", "cli", "inspect several files")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "final answer" {
		t.Fatalf("answer = %q, want final answer", answer)
	}

	events := drainAgentEvents(eventCh)
	sawCompactStep := false
	sawCompactEvent := false
	for _, e := range events {
		if e.Type == "agent.step" {
			if outcome, _ := e.Payload["outcome"].(string); outcome == string(StepCompactContext) {
				sawCompactStep = true
			}
		}
		if e.Type == "context.compacted" {
			sawCompactEvent = true
		}
	}
	if !sawCompactStep {
		t.Fatalf("expected a compact_context step within the run; events=%v", stepOutcomeTrace(events))
	}
	if !sawCompactEvent {
		t.Fatal("expected a context.compacted event from mid-turn compaction")
	}
}
