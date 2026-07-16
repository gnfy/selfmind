package app

import (
	"context"
	"errors"
	"testing"

	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type postRunProviderStub struct {
	content string
	calls   int
	err     error
}

func (p *postRunProviderStub) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return p.content, nil
}

func (p *postRunProviderStub) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return &llm.ChatResponse{Content: p.content}, nil
}

func (p *postRunProviderStub) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

func TestMaintenanceProviderChainUsesExplicitFallback(t *testing.T) {
	primary := &postRunProviderStub{err: errors.New("403 quota exhausted")}
	fallback := &postRunProviderStub{content: `{"task_decision":"KEEP"}`}
	chain := &maintenanceProviderChain{providers: []namedMaintenanceProvider{
		{role: llm.RoleMemoryExtract, provider: primary},
		{role: llm.RoleBackgroundReview, provider: fallback},
	}}
	resp, err := chain.Chat(context.Background(), llm.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Content == "" || primary.calls != 1 || fallback.calls != 1 {
		t.Fatalf("resp=%+v primary_calls=%d fallback_calls=%d", resp, primary.calls, fallback.calls)
	}
}

func TestPostRunAnalyzerCombinesDecisionAndFactPersistence(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	model := &postRunProviderStub{content: `{
		"task_decision":"TITLE:Post-run maintenance",
		"user_facts":["User prefers concise engineering summaries"],
		"memory_facts":["The active repository uses Go"]
	}`}
	analyzer := &llmPostRunAnalyzer{provider: model, memory: mem}
	req := httpapi.PostRunAnalysisRequest{
		Prompt: "analyze", TenantID: "tenant", PersonID: "person",
		WorkspaceID: "workspace", TaskID: "task", RunID: "run",
	}

	got, err := analyzer.Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskDecision != "TITLE:Post-run maintenance" || model.calls != 1 {
		t.Fatalf("analysis=%+v calls=%d", got, model.calls)
	}
	if err := analyzer.Apply(context.Background(), req, got); err != nil {
		t.Fatal(err)
	}
	userFacts, err := mem.GetFacts(context.Background(), "tenant", "user")
	if err != nil || len(userFacts) != 1 {
		t.Fatalf("user facts=%+v err=%v", userFacts, err)
	}
	if userFacts[0].CreatedFromRun != "run" || userFacts[0].Scope != "global" {
		t.Fatalf("user fact metadata=%+v", userFacts[0])
	}
	workspaceFacts, err := mem.GetFacts(context.Background(), "tenant", "memory")
	if err != nil || len(workspaceFacts) != 1 {
		t.Fatalf("memory facts=%+v err=%v", workspaceFacts, err)
	}
	if workspaceFacts[0].Scope != "workspace:workspace" {
		t.Fatalf("workspace fact metadata=%+v", workspaceFacts[0])
	}

	// Re-analyzing the same durable facts must not duplicate memory rows —
	// and the duplicate observation is corroborating evidence, so the stored
	// fact must be REINFORCED (confidence up, verification time refreshed),
	// never silently dropped.
	firstConfidence := userFacts[0].Confidence
	firstVerified := userFacts[0].LastVerifiedAt
	replayed, err := analyzer.Analyze(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := analyzer.Apply(context.Background(), req, replayed); err != nil {
		t.Fatal(err)
	}
	userFacts, _ = mem.GetFacts(context.Background(), "tenant", "user")
	workspaceFacts, _ = mem.GetFacts(context.Background(), "tenant", "memory")
	if len(userFacts) != 1 || len(workspaceFacts) != 1 {
		t.Fatalf("duplicates stored: user=%d memory=%d", len(userFacts), len(workspaceFacts))
	}
	if userFacts[0].Confidence != firstConfidence {
		t.Fatalf("same-run replay must not reinforce twice: %v -> %v", firstConfidence, userFacts[0].Confidence)
	}
	if userFacts[0].LastVerifiedAt.Before(firstVerified) {
		t.Fatalf("reinforcement must not move last_verified_at backwards: %v -> %v", firstVerified, userFacts[0].LastVerifiedAt)
	}
}

func TestDecodePostRunAnalysisRejectsNonJSON(t *testing.T) {
	if _, err := decodePostRunAnalysis("KEEP"); err == nil {
		t.Fatal("non-JSON analyzer output must be rejected")
	}
}
