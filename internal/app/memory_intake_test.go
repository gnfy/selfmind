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
	content          string
	lastPrompt       string
	lastModelContext llm.ModelContext
	chatCalls        int
	err              error
}

func (p *capturingProviderStub) calls() int { return p.chatCalls }

func (p *capturingProviderStub) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return p.content, p.err
}

func (p *capturingProviderStub) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.chatCalls++
	p.lastModelContext = llm.ModelContextFrom(ctx)
	if p.err != nil {
		return nil, p.err
	}
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
// after simplification P3 (preference-only person memory): the model proposes
// rulings against OFFERED user-preference neighbors only; every
// target="memory" decision is skipped deterministically regardless of its
// verb; SUPERSEDE of a user-stated fact degrades to CONFLICT; an invalid ref
// is ignored; REINFORCE bumps confidence without rewriting.
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
		{ID: "33333333-cccc-4ccc-8ccc-cccccccccccc", Target: "user", Content: "User indents with tabs", Source: memory.SourceUser, Scope: "global", Confidence: 0.9, LastVerifiedAt: time.Now()},
		{ID: "44444444-dddd-4ddd-8ddd-dddddddddddd", Target: "user", Content: "User prefers concise answers", Source: memory.SourceFactExtractor, Scope: "global", Confidence: 0.65, LastVerifiedAt: time.Now()},
	}
	for _, f := range seed {
		if err := mem.AddFactMeta(ctx, "person", f); err != nil {
			t.Fatal(err)
		}
	}

	model := &capturingProviderStub{content: `{
		"memory_decisions":[
			{"target":"memory","decision":"SUPERSEDE","ref":"11111111","content":"Default model is kimi-k2","confidence":0.99},
			{"target":"memory","decision":"ADD","content":"Repo uses pnpm for builds","confidence":0.95},
			{"target":"user","decision":"SUPERSEDE","ref":"33333333","content":"User indents with spaces","confidence":0.99},
			{"target":"user","decision":"REINFORCE","ref":"44444444","confidence":0.95},
			{"target":"user","decision":"REINFORCE","ref":"deadbeef","content":"A fact with an unknown reference","confidence":0.95}
		]}`}
	analyzer := &llmPostRunAnalyzer{provider: model, memory: mem}
	req := httpapi.PostRunAnalysisRequest{
		Prompt: "analyze", TurnText: "preferences changed",
		TenantID: "tenant", PersonID: "person", WorkspaceID: "ws", TaskID: "task", RunID: "run",
	}
	analysis, err := analyzer.Analyze(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := analyzer.Apply(ctx, req, analysis); err != nil {
		t.Fatal(err)
	}

	// Only user-preference neighbors are offered; environment rows never are.
	if !strings.Contains(model.lastPrompt, "Existing nearby memories") || !strings.Contains(model.lastPrompt, "[33333333]") {
		t.Fatalf("user neighbor block missing from prompt:\n%s", model.lastPrompt)
	}
	if strings.Contains(model.lastPrompt, "[11111111]") {
		t.Fatalf("environment rows must not be offered as neighbors:\n%s", model.lastPrompt)
	}

	// target="memory" decisions are skipped deterministically: the historical
	// environment row is untouched and the proposed project fact never lands.
	memFacts, _ := mem.GetFacts(ctx, "person", "memory")
	for _, f := range memFacts {
		if f.ID == "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa" && f.Content != "Default model is gpt-4o" {
			t.Fatalf("environment supersede must be skipped: %+v", f)
		}
		if f.Content == "Repo uses pnpm for builds" {
			t.Fatal("environment ADD must be skipped")
		}
	}

	// REINFORCE on a user preference: content untouched, confidence up.
	userFacts, _ := mem.GetFacts(ctx, "person", "user")
	var oldKept, newKept bool
	for _, f := range userFacts {
		if f.ID == "44444444-dddd-4ddd-8ddd-dddddddddddd" && (f.Content != "User prefers concise answers" || f.Confidence <= 0.65) {
			t.Fatalf("reinforce must bump confidence without rewriting: %+v", f)
		}
		if f.Content == "User indents with tabs" {
			oldKept = true
		}
		if f.Content == "User indents with spaces" {
			newKept = true
		}
		if f.Content == "A fact with an unknown reference" {
			t.Fatal("unknown ref must not create memory")
		}
	}
	// Protected user-stated fact: SUPERSEDE degrades to CONFLICT — both kept.
	if !oldKept || !newKept {
		t.Fatalf("protected supersede must keep both statements: old=%v new=%v %+v", oldKept, newKept, userFacts)
	}
}

func TestResolveOfferedRefUsesIdentityAcrossTargetBuckets(t *testing.T) {
	fact := memory.Fact{ID: "abcdef12-3456", Target: "user", Scope: "global", Content: "User prefers concise replies"}
	got := resolveOfferedRef(map[string][]memory.Fact{"user": {fact}}, "abcdef12")
	if got == nil || got.ID != fact.ID || got.Target != "user" || got.Scope != "global" {
		t.Fatalf("resolved = %+v", got)
	}
}

// TestIntakeSupersedeConfidenceGate: an under-confident SUPERSEDE never
// retires the old belief — it degrades to CONFLICT (both kept).
// The canonical row still reaches the analyzer prompt as a neighbor, but a
// legacy user_facts array naming the same statement no longer reinforces or
// recreates anything — legacy arrays are audited and ignored.
func TestPostRunAnalyzerLegacyFactArraysDoNotTouchCanonical(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	ctx := context.Background()
	store, ok := mem.Canonical()
	if !ok {
		t.Fatal("canonical store missing")
	}
	const statement = "User prefers concise daily reports with one item per line"
	if err := store.ApplyIntakeWrite(ctx, "person", memory.IntakeWrite{
		Decision:        "ADD",
		Target:          "user",
		Scope:           "global",
		Source:          memory.SourceFactExtractor,
		Content:         statement,
		RunID:           "seed-run",
		Confidence:      0.8,
		AnalyzerVersion: 1,
		DecisionKey:     "seed",
	}); err != nil {
		t.Fatal(err)
	}

	model := &capturingProviderStub{content: `{
		"task_decision":"KEEP",
		"user_facts":["User prefers concise daily reports with one item per line"]
	}`}
	analyzer := &llmPostRunAnalyzer{provider: model, memory: mem}
	req := httpapi.PostRunAnalysisRequest{
		Prompt: "analyze", TurnText: "Please keep the report concise, with one item per line.",
		TenantID: "tenant", PersonID: "person", WorkspaceID: "ws", TaskID: "task", RunID: "new-run",
	}
	analysis, err := analyzer.Analyze(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.lastPrompt, statement) {
		t.Fatalf("canonical neighbor missing from prompt:\n%s", model.lastPrompt)
	}
	if err := analyzer.Apply(ctx, req, analysis); err != nil {
		t.Fatal(err)
	}

	legacy, err := mem.GetFacts(ctx, "person", "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 0 {
		t.Fatalf("legacy fact arrays must not create legacy rows: %+v", legacy)
	}
	canonicals, err := store.ListCanonicalMemories(ctx, "person", memory.CanonicalFilter{Target: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if len(canonicals) != 1 || canonicals[0].EvidenceCount != 1 {
		t.Fatalf("ignored legacy arrays must leave canonical evidence untouched: %+v", canonicals)
	}
}

func TestIntakeSupersedeConfidenceGate(t *testing.T) {
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	ctx := context.Background()
	if err := mem.AddFactMeta(ctx, "tenant", memory.Fact{
		ID: "44444444-dddd-4ddd-8ddd-dddddddddddd", Target: "user",
		Content: "User prefers replies in English", Source: memory.SourceFactExtractor, Scope: "global", Confidence: 0.65,
	}); err != nil {
		t.Fatal(err)
	}
	model := &capturingProviderStub{content: `{
		"memory_decisions":[
			{"target":"user","decision":"SUPERSEDE","ref":"44444444","content":"User prefers replies in Chinese","confidence":0.8}
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
	facts, _ := mem.GetFacts(ctx, "tenant", "user")
	var old, updated bool
	for _, f := range facts {
		if f.Content == "User prefers replies in English" {
			old = true
		}
		if f.Content == "User prefers replies in Chinese" {
			updated = true
		}
	}
	if !old || !updated {
		t.Fatalf("under-confident supersede must keep both: old=%v new=%v", old, updated)
	}
}

// TestIntakeDurabilityEnforcement pins the deterministic time-validity gate:
// episodic decisions and transient run-state content never become memory even
// when the model pairs them with ADD (the 2026-07-17 pollution: 10/29 stored
// facts were IN_PROGRESS-style run state), and time_bounded facts carry a
// valid_until.
func TestIntakeDurabilityEnforcement(t *testing.T) {
	ctx := context.Background()
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	mem := memory.NewMemoryManager(provider)
	model := &capturingProviderStub{content: `{
		"memory_decisions":[
			{"target":"user","decision":"ADD","content":"RUQX-222 已执行并审批，当前状态 IN_PROGRESS / QUEUED","confidence":0.9,"durability":"durable"},
			{"target":"user","decision":"ADD","content":"RUQX-500 本次构建状态标记为 PREPARED_NOT_EXECUTED，尚未执行","confidence":0.9},
			{"target":"user","decision":"ADD","content":"release freeze active for launch week","confidence":0.9,"durability":"episodic"},
			{"target":"user","decision":"ADD","content":"用户偏好在发布说明里保留英文技术标识符","confidence":0.9,"durability":"durable","category":"preference"},
			{"target":"user","decision":"ADD","content":"本周内先用英文回复（临时出差偏好）","confidence":0.9,"durability":"time_bounded","valid_until":"2999-01-02T15:04:05Z"}
		]}`}
	analyzer := &llmPostRunAnalyzer{provider: model, memory: mem}
	req := httpapi.PostRunAnalysisRequest{
		Prompt: "analyze", TurnText: "release work",
		TenantID: "tenant", PersonID: "person", WorkspaceID: "ws", TaskID: "task", RunID: "run",
	}
	analysis, err := analyzer.Analyze(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := analyzer.Apply(ctx, req, analysis); err != nil {
		t.Fatal(err)
	}

	facts, _ := mem.GetFacts(ctx, "person", "user")
	if len(facts) != 2 {
		t.Fatalf("stored facts = %d (%+v), want durable + time_bounded", len(facts), facts)
	}
	for _, f := range facts {
		if strings.Contains(f.Content, "IN_PROGRESS") || strings.Contains(f.Content, "PREPARED_NOT_EXECUTED") || strings.Contains(f.Content, "release freeze") {
			t.Fatalf("episodic/unlabeled-transient content stored: %q", f.Content)
		}
	}

	store, ok := mem.Canonical()
	if !ok {
		t.Fatal("canonical store missing")
	}
	canonicals, err := store.ListCanonicalMemories(ctx, "person", memory.CanonicalFilter{})
	if err != nil || len(canonicals) != 2 {
		t.Fatalf("canonicals=%+v err=%v", canonicals, err)
	}
	var sawBounded, sawDurable bool
	for _, c := range canonicals {
		switch {
		case strings.Contains(c.Content, "本周内先用英文回复"):
			sawBounded = true
			if c.ValidUntil.IsZero() {
				t.Fatalf("time_bounded canonical lost valid_until: %+v", c)
			}
		case strings.Contains(c.Content, "保留英文技术标识符"):
			sawDurable = true
			if !c.ValidUntil.IsZero() {
				t.Fatalf("durable canonical must not expire: %+v", c)
			}
			if c.Category != "preference" {
				t.Fatalf("category lost: %+v", c)
			}
		case strings.Contains(c.Content, "IN_PROGRESS"):
			t.Fatalf("confirmed run-state canonical must be dropped: %+v", c)
		}
	}
	if !sawBounded || !sawDurable {
		t.Fatalf("expected bounded+durable facts in canonical store: %+v", canonicals)
	}
}

// TestIntakeDurabilityFailClosed pins the fail-closed default: a decision
// with NO durability field and no transient markers is stored, but bounded —
// an omitted field never mints permanent memory.
func TestIntakeDurabilityFailClosed(t *testing.T) {
	meta, episodic := decisionMeta(httpapi.MemoryDecision{Content: "team prefers squash merges"})
	if episodic {
		t.Fatal("unlabeled non-transient content must still be stored")
	}
	if meta.ValidUntil.IsZero() {
		t.Fatal("unlabeled content must be time-bounded, never permanent")
	}
	meta, episodic = decisionMeta(httpapi.MemoryDecision{Content: "team prefers squash merges", Durability: "durable"})
	if episodic || !meta.ValidUntil.IsZero() {
		t.Fatalf("explicit durable without markers must be permanent: %+v %v", meta, episodic)
	}
	// Confirmed transient (instance + current-state) without a label: dropped.
	if _, episodic := decisionMeta(httpapi.MemoryDecision{Content: "RUQX-9 当前状态 QUEUED"}); !episodic {
		t.Fatal("unlabeled confirmed-transient content must be dropped")
	}
	// Candidate (status vocabulary only) without a label: stored bounded,
	// never dropped — the acceptance bar protects ambiguous rules.
	if meta, episodic := decisionMeta(httpapi.MemoryDecision{Content: "构建已入队 QUEUED"}); episodic || meta.ValidUntil.IsZero() {
		t.Fatalf("unlabeled candidate must survive bounded: %+v %v", meta, episodic)
	}
	// Explanatory rule semantics veto the confirmed tier: an explicit durable
	// rule that mentions status tokens stays PERMANENT.
	if meta, episodic := decisionMeta(httpapi.MemoryDecision{Content: "发布记录从 PREPARED_NOT_EXECUTED 转为 EXECUTED 表示执行完成", Durability: "durable"}); episodic || !meta.ValidUntil.IsZero() {
		t.Fatalf("durable rule mentioning status tokens must stay permanent: %+v %v", meta, episodic)
	}
	if meta, episodic := decisionMeta(httpapi.MemoryDecision{Content: "QUEUED means the task is waiting for dispatch", Durability: "durable"}); episodic || !meta.ValidUntil.IsZero() {
		t.Fatalf("english rule must stay permanent: %+v %v", meta, episodic)
	}
}
