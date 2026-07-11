package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

type capturingProviderStub struct {
	content    string
	lastPrompt string
	chatCalls  int
}

func (p *capturingProviderStub) calls() int { return p.chatCalls }

func (p *capturingProviderStub) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return p.content, nil
}

func (p *capturingProviderStub) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.chatCalls++
	if len(req.Messages) > 0 {
		p.lastPrompt = req.Messages[len(req.Messages)-1].Content
	}
	return &llm.ChatResponse{Content: p.content}, nil
}

func (p *capturingProviderStub) StreamChat(context.Context, llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent)
	close(ch)
	return ch, nil
}

// TestIntakeDecisionPolicy pins the deterministic policy layer of intake
// (docs/memory-governance.zh-CN.md §3.4): the model proposes rulings against
// OFFERED neighbors only; SUPERSEDE retires a belief in place, protected
// (user-stated) facts degrade to CONFLICT, an invalid ref degrades to ADD,
// and REINFORCE bumps confidence without rewriting content.
func TestIntakeDecisionPolicy(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	ctx := context.Background()

	seed := []memory.Fact{
		{ID: "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Target: "memory", Content: "Default model is gpt-4o", Source: memory.SourceFactExtractor, Scope: "global", Confidence: 0.65, LastVerifiedAt: time.Now()},
		{ID: "22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Target: "memory", Content: "Repo uses Go modules", Source: memory.SourceFactExtractor, Scope: "global", Confidence: 0.65, LastVerifiedAt: time.Now()},
		{ID: "33333333-cccc-4ccc-8ccc-cccccccccccc", Target: "user", Content: "User indents with tabs", Source: memory.SourceUser, Scope: "global", Confidence: 0.9, LastVerifiedAt: time.Now()},
	}
	for _, f := range seed {
		if err := mem.AddFactMeta(ctx, "tenant", f); err != nil {
			t.Fatal(err)
		}
	}

	model := &capturingProviderStub{content: `{
		"task_decision":"KEEP",
		"memory_decisions":[
			{"target":"memory","decision":"SUPERSEDE","ref":"11111111","content":"Default model is kimi-k2","confidence":0.99},
			{"target":"memory","decision":"REINFORCE","ref":"22222222","confidence":0.95},
			{"target":"user","decision":"SUPERSEDE","ref":"33333333","content":"User indents with spaces","confidence":0.99},
			{"target":"memory","decision":"REINFORCE","ref":"deadbeef","content":"A fact with an unknown reference","confidence":0.95}
		]}`}
	analyzer := &llmPostRunAnalyzer{provider: model, memory: mem}
	req := httpapi.PostRunAnalysisRequest{
		Prompt: "analyze", TurnText: "model config changed",
		TenantID: "tenant", PersonID: "person", WorkspaceID: "ws", TaskID: "task", RunID: "run",
	}
	analysis, err := analyzer.Analyze(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := analyzer.Apply(ctx, req, analysis); err != nil {
		t.Fatal(err)
	}

	// The model was offered the neighbors with short refs, as data.
	if !strings.Contains(model.lastPrompt, "Existing nearby memories") || !strings.Contains(model.lastPrompt, "[11111111]") {
		t.Fatalf("neighbor block missing from prompt:\n%s", model.lastPrompt)
	}

	memFacts, _ := mem.GetFacts(ctx, "tenant", "memory")
	byID := map[string]memory.Fact{}
	byContent := map[string]memory.Fact{}
	for _, f := range memFacts {
		byID[f.ID] = f
		byContent[f.Content] = f
	}

	// SUPERSEDE: belief keeps its id, content moves forward, old text gone.
	if f, ok := byID["11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa"]; !ok || f.Content != "Default model is kimi-k2" {
		t.Fatalf("supersede must replace content in place: %+v", f)
	}
	if _, stale := byContent["Default model is gpt-4o"]; stale {
		t.Fatal("superseded content must not remain active")
	}

	// REINFORCE: content untouched, confidence up.
	if f := byID["22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb"]; f.Content != "Repo uses Go modules" || f.Confidence <= 0.65 {
		t.Fatalf("reinforce must bump confidence without rewriting: %+v", f)
	}

	// Invalid ref degrades to ADD.
	if _, ok := byContent["A fact with an unknown reference"]; !ok {
		t.Fatal("unknown ref must degrade to ADD")
	}

	// Protected user-stated fact: SUPERSEDE degrades to CONFLICT — both kept.
	userFacts, _ := mem.GetFacts(ctx, "tenant", "user")
	var oldKept, newKept bool
	for _, f := range userFacts {
		if f.Content == "User indents with tabs" {
			oldKept = true
		}
		if f.Content == "User indents with spaces" {
			newKept = true
		}
	}
	if !oldKept || !newKept {
		t.Fatalf("protected supersede must keep both statements: old=%v new=%v %+v", oldKept, newKept, userFacts)
	}
}

// TestIntakeSupersedeConfidenceGate: an under-confident SUPERSEDE never
// retires the old belief — it degrades to CONFLICT (both kept).
func TestIntakeSupersedeConfidenceGate(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	ctx := context.Background()
	if err := mem.AddFactMeta(ctx, "tenant", memory.Fact{
		ID: "44444444-dddd-4ddd-8ddd-dddddddddddd", Target: "memory",
		Content: "Service listens on port 8080", Source: memory.SourceFactExtractor, Scope: "global", Confidence: 0.65,
	}); err != nil {
		t.Fatal(err)
	}
	model := &capturingProviderStub{content: `{
		"task_decision":"KEEP",
		"memory_decisions":[
			{"target":"memory","decision":"SUPERSEDE","ref":"44444444","content":"Service listens on port 9090","confidence":0.8}
		]}`}
	analyzer := &llmPostRunAnalyzer{provider: model, memory: mem}
	req := httpapi.PostRunAnalysisRequest{
		Prompt: "analyze", TurnText: "port change", TenantID: "tenant", RunID: "run",
	}
	analysis, err := analyzer.Analyze(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := analyzer.Apply(ctx, req, analysis); err != nil {
		t.Fatal(err)
	}
	facts, _ := mem.GetFacts(ctx, "tenant", "memory")
	var old, updated bool
	for _, f := range facts {
		if f.Content == "Service listens on port 8080" {
			old = true
		}
		if f.Content == "Service listens on port 9090" {
			updated = true
		}
	}
	if !old || !updated {
		t.Fatalf("under-confident supersede must keep both: old=%v new=%v", old, updated)
	}
}
