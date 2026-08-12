package llm

import (
	"context"
	"strings"
	"testing"
)

type recordingProvider struct {
	model       string
	content     string
	calls       int
	seenRole    ModelRole
	seenReqRole string
}

func (p *recordingProvider) ChatCompletion(ctx context.Context, messages []Message) (string, error) {
	resp, err := p.Chat(ctx, ChatRequest{Messages: messages})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

func (p *recordingProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	p.calls++
	p.seenRole = ModelContextFrom(ctx).Role
	if req.Options != nil {
		if role, ok := req.Options["model_role"].(string); ok {
			p.seenReqRole = role
		}
	}
	return &ChatResponse{
		Content: p.content,
		Usage: UsageStats{
			InputTokens:  10,
			OutputTokens: 5,
		},
	}, nil
}

func (p *recordingProvider) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	ch := make(chan StreamEvent, 1)
	ch <- StreamEvent{Content: p.content, Usage: &UsageStats{InputTokens: 1, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

func (p *recordingProvider) SetModel(model string) {
	p.model = model
}

func (p *recordingProvider) GetModel() string {
	return p.model
}

type recordingUsageRecorder struct {
	events []UsageEvent
}

func (r *recordingUsageRecorder) RecordModelUsage(ctx context.Context, event UsageEvent) {
	r.events = append(r.events, event)
}

func TestPolicyGatewayRoutesRoleProfile(t *testing.T) {
	fallback := &recordingProvider{model: "fallback-model", content: "fallback"}
	memory := &recordingProvider{model: "memory-model", content: "memory"}
	gw := NewPolicyGateway(ProviderProfile{
		Name:         "default",
		ProviderName: "openai",
		Model:        fallback.model,
		Provider:     fallback,
	})
	gw.RegisterRoleProfile(RoleMemoryExtract, ProviderProfile{
		Name:         "memory",
		ProviderName: "gemini",
		Model:        memory.model,
		Provider:     memory,
	})

	provider := gw.ProviderForRole(RoleMemoryExtract)
	resp, err := provider.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Content != "memory" {
		t.Fatalf("expected memory profile response, got %q", resp.Content)
	}
	if memory.calls != 1 || fallback.calls != 0 {
		t.Fatalf("expected memory calls=1 fallback calls=0, got %d/%d", memory.calls, fallback.calls)
	}
	if memory.seenRole != RoleMemoryExtract || memory.seenReqRole != string(RoleMemoryExtract) {
		t.Fatalf("role metadata not propagated: ctx=%q req=%q", memory.seenRole, memory.seenReqRole)
	}
}

func TestStablePromptCacheKeySurvivesRunChanges(t *testing.T) {
	base := ModelContext{TenantID: "tenant-a", PersonID: "person-a", WorkspaceID: "ws-a", TaskID: "task-a", RunID: "run-1", Role: RoleCodingAgent}
	first := StablePromptCacheKey(WithModelContext(context.Background(), base))
	base.RunID = "run-2"
	second := StablePromptCacheKey(WithModelContext(context.Background(), base))
	if first == "" || first != second {
		t.Fatalf("cache keys must be stable across runs: %q != %q", first, second)
	}
	base.TaskID = "task-b"
	if other := StablePromptCacheKey(WithModelContext(context.Background(), base)); other == first {
		t.Fatalf("different tasks reused cache key %q", other)
	}
	if got := StablePromptCacheKey(context.Background()); got != "" {
		t.Fatalf("empty model context cache key = %q", got)
	}
}

func TestStableProviderUserIDIsRunIndependentAndOpaque(t *testing.T) {
	base := ModelContext{TenantID: "tenant-a", PersonID: "weixin-user-123", RunID: "run-1"}
	first := StableProviderUserID(WithModelContext(context.Background(), base))
	base.RunID = "run-2"
	second := StableProviderUserID(WithModelContext(context.Background(), base))
	if first == "" || first != second {
		t.Fatalf("provider user id must be stable across runs: %q %q", first, second)
	}
	if strings.Contains(first, "tenant") || strings.Contains(first, "weixin") || strings.Contains(first, "123") {
		t.Fatalf("provider user id leaked source identity: %q", first)
	}
	base.PersonID = "other-person"
	if other := StableProviderUserID(WithModelContext(context.Background(), base)); other == first {
		t.Fatal("different people must not share a provider user id")
	}
}

func TestPolicyGatewayFallsBackForUnconfiguredRole(t *testing.T) {
	fallback := &recordingProvider{model: "fallback-model", content: "fallback"}
	gw := NewPolicyGateway(ProviderProfile{
		Name:         "default",
		ProviderName: "openai",
		Model:        fallback.model,
		Provider:     fallback,
	})

	provider := gw.ProviderForRole(RoleSkillCurator)
	resp, err := provider.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Content != "fallback" || fallback.calls != 1 {
		t.Fatalf("expected fallback response/call, got %q calls=%d", resp.Content, fallback.calls)
	}
	if GetModelName(provider) != "fallback-model" {
		t.Fatalf("expected fallback model, got %q", GetModelName(provider))
	}
}

func TestPolicyGatewayRecordsUsage(t *testing.T) {
	fallback := &recordingProvider{model: "fallback-model", content: "fallback"}
	recorder := &recordingUsageRecorder{}
	gw := NewPolicyGateway(ProviderProfile{
		Name:         "default",
		ProviderName: "openai",
		Model:        fallback.model,
		Provider:     fallback,
	})
	gw.SetUsageRecorder(recorder)

	ctx := WithModelContext(context.Background(), ModelContext{TenantID: "tenant-a"})
	if _, err := gw.ProviderForRole(RoleCodingAgent).Chat(ctx, ChatRequest{}); err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("expected 1 usage event, got %d", len(recorder.events))
	}
	event := recorder.events[0]
	if event.Context.TenantID != "tenant-a" || event.Context.Role != RoleCodingAgent {
		t.Fatalf("unexpected usage context: %+v", event.Context)
	}
	if event.ProviderName != "openai" || event.Model != "fallback-model" {
		t.Fatalf("unexpected usage model metadata: %+v", event)
	}
	if event.InputTokens != 10 || event.OutputTokens != 5 {
		t.Fatalf("unexpected usage tokens: %+v", event)
	}
}
