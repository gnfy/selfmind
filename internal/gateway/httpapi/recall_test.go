package httpapi

// Automatic semantic recall (Work Timeline P2) tests: selector attachment,
// relevance, budget/dedupe, control-command/short-message skips, query
// expansion (with degrade-on-timeout), the context.recall event, and the
// ephemeral-injection guarantee (recall never enters persisted user/assistant
// history). All deterministic and offline: temp control.db + temp FTS store.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

// newRecallMemory builds a real SQLite-backed memory manager in a temp dir.
func newRecallMemory(t *testing.T) *memory.MemoryManager {
	t.Helper()
	provider, err := memory.NewSQLiteProvider(t.TempDir())
	if err != nil {
		t.Fatalf("NewSQLiteProvider: %v", err)
	}
	mem := memory.NewMemoryManager(provider)
	t.Cleanup(func() { _ = mem.Close() })
	return mem
}

func seedIndexedSession(t *testing.T, mem *memory.MemoryManager, tenantID, sessionID string, texts ...string) {
	t.Helper()
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var msgs []msg
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, msg{Role: role, Content: text})
	}
	data, _ := json.Marshal(map[string]interface{}{"messages": msgs})
	if err := mem.IndexSession(context.Background(), tenantID, "cli", sessionID, data); err != nil {
		t.Fatalf("IndexSession: %v", err)
	}
}

func newRecallControl(t *testing.T) (*control.Store, *control.IdentityContext) {
	t.Helper()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Me")
	if err != nil {
		t.Fatal(err)
	}
	return store, identity
}

func seedTaskCard(t *testing.T, store *control.Store, identity *control.IdentityContext, title, summary string, files []string) *control.Task {
	t.Helper()
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title, Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "in_progress", summary, nil); err != nil {
		t.Fatal(err)
	}
	if summary != "" || len(files) > 0 {
		if _, err := store.SaveHandoff(ctx, control.Handoff{TaskID: task.ID, Summary: summary, ChangedFiles: files}); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

// selectWithRecall runs the real selector path for a fresh current task.
func selectWithRecall(t *testing.T, store *control.Store, identity *control.IdentityContext, engine *RecallEngine, message string) (kernel.TaskRuntimeContext, *control.Task) {
	t.Helper()
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "current turn", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", message)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store, Recall: engine}
	return server.coordinator().selectedTaskRuntimeContext(ctx, task, run, nil, "cli", message), task
}

func TestRecallSelectorAttachesRelatedSlices(t *testing.T) {
	store, identity := newRecallControl(t)
	mem := newRecallMemory(t)
	seedIndexedSession(t, mem, identity.PersonID, "sess-tetris",
		"build a tetris game with javascript canvas", "created tetris.html with rotation logic")
	card := seedTaskCard(t, store, identity, "KOF fighting game", "generated fighter sprites and index.html", []string{"kof/index.html"})

	engine := NewRecallEngine(store, mem, nil)
	selected, task := selectWithRecall(t, store, identity, engine, "improve the tetris javascript rendering")
	if len(selected.RecallSlices) == 0 {
		t.Fatalf("expected recall slices for a related message")
	}
	var refs []string
	for _, slice := range selected.RecallSlices {
		refs = append(refs, slice.Source+":"+slice.Ref)
		if slice.Ref == card.ID {
			t.Fatalf("unrelated task card must not be recalled: %+v", slice)
		}
	}
	if !strings.Contains(strings.Join(refs, " "), "session:sess-tetris") {
		t.Fatalf("expected the tetris session hit, got %v", refs)
	}
	prompt := selected.Prompt(10000)
	if !strings.Contains(prompt, "[Recall — possibly related prior work; reference only]") {
		t.Fatalf("recall header missing from rendered prompt:\n%s", prompt)
	}

	// Observability: a redacted context.recall event with refs, no excerpts.
	events, err := store.ListTaskEvents(context.Background(), task.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var recallEvent *control.Event
	for i := range events {
		if events[i].Type == "context.recall" {
			recallEvent = &events[i]
		}
	}
	if recallEvent == nil {
		t.Fatalf("expected a context.recall event, got %+v", events)
	}
	payload := string(recallEvent.Payload)
	if !strings.Contains(payload, "sess-tetris") {
		t.Fatalf("event payload must carry refs: %s", payload)
	}
	if strings.Contains(payload, "rotation logic") {
		t.Fatalf("event payload must not carry excerpts: %s", payload)
	}
}

func TestRecallUnrelatedMessageNoSlices(t *testing.T) {
	store, identity := newRecallControl(t)
	mem := newRecallMemory(t)
	seedIndexedSession(t, mem, identity.PersonID, "sess-tetris", "build a tetris game with javascript canvas")
	seedTaskCard(t, store, identity, "KOF fighting game", "generated fighter sprites", nil)

	engine := NewRecallEngine(store, mem, nil)
	selected, task := selectWithRecall(t, store, identity, engine, "summarize yesterday weather report numbers")
	if len(selected.RecallSlices) != 0 {
		t.Fatalf("unrelated message must produce no recall slices, got %+v", selected.RecallSlices)
	}
	events, _ := store.ListTaskEvents(context.Background(), task.ID, 20)
	for _, event := range events {
		if event.Type == "context.recall" {
			t.Fatalf("no context.recall event expected when nothing is recalled")
		}
	}
}

func TestRecallBudgetExcerptCapAndDedupe(t *testing.T) {
	store, identity := newRecallControl(t)
	mem := newRecallMemory(t)
	longSummary := strings.Repeat("gearbox refactor details ", 40) // ~1000 chars
	var cardTasks []*control.Task
	for i := 0; i < 6; i++ {
		cardTasks = append(cardTasks, seedTaskCard(t, store, identity,
			fmt.Sprintf("gearbox refactor part %d", i), longSummary, []string{"pkg/gearbox.go"}))
	}
	// A raw session fragment on the SAME work line as card 0: dedupe must keep
	// one slice for that line and prefer the label card.
	seedIndexedSession(t, mem, identity.PersonID, "task:"+cardTasks[0].ID, "gearbox refactor session transcript")

	engine := NewRecallEngine(store, mem, nil)
	selected, _ := selectWithRecall(t, store, identity, engine, "continue the gearbox refactor work")
	if len(selected.RecallSlices) == 0 || len(selected.RecallSlices) > 3 {
		t.Fatalf("recall must return 1..3 slices, got %d", len(selected.RecallSlices))
	}
	seen := map[string]bool{}
	for _, slice := range selected.RecallSlices {
		// textutil.Truncate is byte-based and appends "..." past the cap.
		if len(slice.Excerpt) > recallExcerptChars+3 {
			t.Fatalf("excerpt exceeds cap: %d bytes", len(slice.Excerpt))
		}
		if seen[slice.Ref] {
			t.Fatalf("duplicate work line in slices: %s", slice.Ref)
		}
		seen[slice.Ref] = true
		if slice.Ref == cardTasks[0].ID && slice.Source != "taskcard" {
			t.Fatalf("label card must win over raw session fragment for the same work line, got %+v", slice)
		}
	}
}

// countingSessionSearcher records FTS probes so skip paths can prove they did
// no work.
type countingSessionSearcher struct {
	mu    sync.Mutex
	calls int
}

func (c *countingSessionSearcher) SearchSessions(tenantID, query string, limit int) ([]memory.FTS5Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return nil, nil
}

func (c *countingSessionSearcher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestRecallSkipsControlCommandsAndShortMessages(t *testing.T) {
	searcher := &countingSessionSearcher{}
	engine := NewRecallEngine(nil, searcher, nil)
	for _, message := range []string{"/status", "  /approve 1", "hi", "好", "ok?"} {
		slices, stats := engine.Select(context.Background(), "default", "person", "", message)
		if len(slices) != 0 {
			t.Fatalf("message %q must not recall, got %+v", message, slices)
		}
		if stats.Skipped == "" {
			t.Fatalf("message %q must report a skip reason", message)
		}
	}
	if searcher.count() != 0 {
		t.Fatalf("skipped messages must not run searches, got %d", searcher.count())
	}
}

// scriptedExpander is a fake semantic_recall expander.
type scriptedExpander struct {
	result string
	block  chan struct{} // when non-nil, Expand waits for release or ctx
}

func (e *scriptedExpander) Expand(ctx context.Context, query string) string {
	if e.block != nil {
		select {
		case <-e.block:
		case <-ctx.Done():
			return query
		}
	}
	if e.result == "" {
		return query
	}
	return query + " " + e.result
}

func TestRecallExpansionAddsVariants(t *testing.T) {
	store, identity := newRecallControl(t)
	mem := newRecallMemory(t)
	// Only the expansion variant "tetris" matches the indexed session.
	seedIndexedSession(t, mem, identity.PersonID, "sess-tetris", "tetris implementation notes and scoring")

	without := NewRecallEngine(store, mem, nil)
	slices, stats := without.Select(context.Background(), identity.TenantID, identity.PersonID, "", "improve the falling blocks arcade thing")
	if len(slices) != 0 || stats.Expanded {
		t.Fatalf("raw terms must not match, got %+v (expanded=%v)", slices, stats.Expanded)
	}

	with := NewRecallEngine(store, mem, &scriptedExpander{result: "tetris puzzle"})
	slices, stats = with.Select(context.Background(), identity.TenantID, identity.PersonID, "", "improve the falling blocks arcade thing")
	if !stats.Expanded {
		t.Fatalf("expansion must be reported in stats")
	}
	if len(slices) == 0 || slices[0].Ref != "sess-tetris" {
		t.Fatalf("expansion variant must recall the session, got %+v", slices)
	}
}

func TestRecallExpansionTimeoutFallsBackToRawTerms(t *testing.T) {
	store, identity := newRecallControl(t)
	mem := newRecallMemory(t)
	seedIndexedSession(t, mem, identity.PersonID, "sess-raw", "gearbox refactor transcript")

	blocked := &scriptedExpander{result: "irrelevant", block: make(chan struct{})}
	engine := NewRecallEngine(store, mem, blocked)
	engine.expandTimeout = 30 * time.Millisecond

	start := time.Now()
	slices, stats := engine.Select(context.Background(), identity.TenantID, identity.PersonID, "", "continue the gearbox refactor")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("recall must not block on a stuck expander (took %s)", elapsed)
	}
	if stats.Expanded {
		t.Fatalf("timed-out expansion must not be reported as expanded")
	}
	if len(slices) == 0 || slices[0].Ref != "sess-raw" {
		t.Fatalf("raw terms must still recall after expansion timeout, got %+v", slices)
	}
	close(blocked.block)
}

// capturingProvider records every model request so tests can inspect exactly
// what the agent saw, and answers with a fixed completion.
type capturingProvider struct {
	mu       sync.Mutex
	requests [][]llm.Message
	answer   string
}

func (p *capturingProvider) record(messages []llm.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	copied := make([]llm.Message, len(messages))
	copy(copied, messages)
	p.requests = append(p.requests, copied)
}

func (p *capturingProvider) snapshot() [][]llm.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]llm.Message(nil), p.requests...)
}

func (p *capturingProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	p.record(messages)
	return p.answer, nil
}

func (p *capturingProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.record(req.Messages)
	return &llm.ChatResponse{Content: p.answer}, nil
}

func (p *capturingProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.record(req.Messages)
	out := make(chan llm.StreamEvent, 1)
	out <- llm.StreamEvent{Content: p.answer}
	close(out)
	return out, nil
}

// TestRecallEphemeralNotPersistedInHistory is the hermes-pattern guarantee:
// recall slices reach the model only through this turn's system-prompt context
// block. They must never appear in persisted user/assistant working history,
// and a later turn on the same task must not replay them.
func TestRecallEphemeralNotPersistedInHistory(t *testing.T) {
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, identity := newRecallControl(t)
	mem := newRecallMemory(t)
	const marker = "MARKER-TETRIS-ROTATION-42"
	seedIndexedSession(t, mem, identity.PersonID, "sess-old-tetris",
		"tetris rendering work "+marker)

	provider := &capturingProvider{answer: "Done."}
	agent := kernel.NewAgent(mem, stubToolBackend{}, provider, "test agent", 2, 1, nil)
	gw := router.NewGateway(nil, nil, agent, nil)
	daemon := &Server{
		Control:         store,
		Gateway:         gw,
		DefaultTenantID: identity.TenantID,
		Recall:          NewRecallEngine(store, mem, nil),
	}

	ctx := context.Background()
	req := api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "improve the tetris rendering pipeline",
	}
	intent := daemon.classifyIntent(ctx, identity, req.Content, req.Channel)
	resp, status := daemon.coordinator().runMessage(ctx, identity, req, intent)
	if status != 200 || resp.Error != "" {
		t.Fatalf("turn 1 failed: %d %s", status, resp.Error)
	}
	if resp.Task == nil {
		t.Fatal("turn 1 returned no task")
	}
	taskID := resp.Task.ID

	// Injection worked end to end: the model saw the recall block this turn.
	turn1 := provider.snapshot()
	if len(turn1) == 0 {
		t.Fatal("provider captured no requests")
	}
	foundInSystem := false
	for _, msg := range turn1[len(turn1)-1] {
		if msg.Role == "system" && strings.Contains(msg.Content, marker) {
			foundInSystem = true
		}
	}
	if !foundInSystem {
		t.Fatalf("recall content must reach the model via the system context block")
	}

	// Persisted working history: the marker must not exist in any
	// user/assistant message (the replayable spine text).
	blobs, err := mem.GetLatestContext(ctx, identity.PersonID, "task:"+taskID)
	if err != nil || len(blobs) == 0 {
		t.Fatalf("expected persisted task history, err=%v blobs=%d", err, len(blobs))
	}
	for _, blob := range blobs {
		var record struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.Unmarshal(blob, &record); err != nil {
			t.Fatal(err)
		}
		for _, msg := range record.Messages {
			if (msg.Role == "user" || msg.Role == "assistant") && strings.Contains(msg.Content, marker) {
				t.Fatalf("recall leaked into persisted %s history: %q", msg.Role, msg.Content)
			}
		}
	}

	// Turn 2 continues the SAME task with an unrelated message: nothing the
	// model sees may still carry the turn-1 recall content.
	req2 := api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "now summarize the weather station findings", TaskID: taskID,
	}
	intent2 := daemon.classifyIntent(ctx, identity, req2.Content, req2.Channel)
	resp2, status2 := daemon.coordinator().runMessage(ctx, identity, req2, intent2)
	if status2 != 200 || resp2.Error != "" {
		t.Fatalf("turn 2 failed: %d %s", status2, resp2.Error)
	}
	turn2 := provider.snapshot()
	last := turn2[len(turn2)-1]
	for _, msg := range last {
		if strings.Contains(msg.Content, marker) {
			t.Fatalf("turn 2 %s message still carries turn-1 recall content: %q", msg.Role, msg.Content)
		}
	}
}
