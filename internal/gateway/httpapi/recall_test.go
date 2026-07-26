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
	"reflect"
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

func seedCanonicalMemory(t *testing.T, mem *memory.MemoryManager, personID string, write memory.IntakeWrite) memory.CanonicalMemory {
	t.Helper()
	store, ok := mem.Canonical()
	if !ok {
		t.Fatal("canonical store unavailable")
	}
	if write.Decision == "" {
		write.Decision = "ADD"
	}
	if write.Target == "" {
		write.Target = "memory"
	}
	if write.Scope == "" {
		write.Scope = "global"
	}
	if err := store.ApplyIntakeWrite(context.Background(), personID, write); err != nil {
		t.Fatalf("ApplyIntakeWrite: %v", err)
	}
	rows, err := store.ListCanonicalMemories(context.Background(), personID, memory.CanonicalFilter{
		Statuses: []string{memory.CanonicalActive, memory.CanonicalConflicted},
	})
	if err != nil {
		t.Fatalf("ListCanonicalMemories: %v", err)
	}
	for _, row := range rows {
		if row.Content == write.Content && row.Scope == write.Scope {
			return row
		}
	}
	t.Fatalf("seeded canonical memory not found: %q", write.Content)
	return memory.CanonicalMemory{}
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
	return server.coordinator().selectedTaskRuntimeContext(ctx, task, run, nil, "cli", "cli", message, false), task
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
	// Zero-hit turns still emit the observability event (W2): "recall found
	// nothing" must be distinguishable from "recall never ran" in /diag.
	events, _ := store.ListTaskEvents(context.Background(), task.ID, 20)
	var recallEvent *control.Event
	for i := range events {
		if events[i].Type == "context.recall" {
			recallEvent = &events[i]
			break
		}
	}
	if recallEvent == nil {
		t.Fatal("zero-hit recall must still emit a context.recall event")
	}
	if !strings.Contains(string(recallEvent.Payload), `"slices":0`) {
		t.Fatalf("zero-hit event must record slices=0: %s", recallEvent.Payload)
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

func TestCanonicalRecallUsesExpansionAndReportsSource(t *testing.T) {
	mem := newRecallMemory(t)
	const personID = "person-canonical-expansion"
	row := seedCanonicalMemory(t, mem, personID, memory.IntakeWrite{
		Target:  "user",
		Scope:   "global",
		Content: "Daily reports use one work item per line.",
	})

	engine := NewRecallEngine(nil, mem, &scriptedExpander{result: "daily report one item per line"})
	slices, stats := engine.Select(context.Background(), "default", personID, "", "summarize what happened at work today")
	if !stats.Expanded {
		t.Fatal("expected semantic expansion")
	}
	if len(slices) == 0 || slices[0].Source != "canonical" || slices[0].Ref != row.ID {
		t.Fatalf("expected canonical recall hit, got %+v", slices)
	}
	if stats.Sources["canonical"] != 1 {
		t.Fatalf("canonical source missing from stats: %+v", stats.Sources)
	}
}

func TestLongRawRecallTermsPreserveExpansionVocabulary(t *testing.T) {
	raw := make([]string, recallMaxTerms+8)
	for i := range raw {
		raw[i] = fmt.Sprintf("raw-%d", i)
	}
	terms, rawCount := boundedRecallTerms(raw, []string{"reporting", "format"}, recallMaxTerms)
	if len(terms) != recallMaxTerms {
		t.Fatalf("terms=%d want %d", len(terms), recallMaxTerms)
	}
	if rawCount != recallMaxTerms-2 {
		t.Fatalf("rawCount=%d want %d", rawCount, recallMaxTerms-2)
	}
	if got := terms[rawCount:]; !reflect.DeepEqual(got, []string{"reporting", "format"}) {
		t.Fatalf("expansion terms were starved: %v", got)
	}
}

func TestCanonicalRecallRespectsWorkspaceValidityAndPinnedBoundary(t *testing.T) {
	mem := newRecallMemory(t)
	const personID = "person-canonical-scope"
	global := seedCanonicalMemory(t, mem, personID, memory.IntakeWrite{
		Target: "user", Scope: "global", Content: "Use the aurora release checklist.",
	})
	workspaceA := seedCanonicalMemory(t, mem, personID, memory.IntakeWrite{
		Scope: "workspace:ws-a", WorkspaceID: "ws-a", Content: "Aurora alpha deploys require the blue gate.",
	})
	workspaceB := seedCanonicalMemory(t, mem, personID, memory.IntakeWrite{
		Scope: "workspace:ws-b", WorkspaceID: "ws-b", Content: "Aurora beta deploys require the red gate.",
	})
	expired := seedCanonicalMemory(t, mem, personID, memory.IntakeWrite{
		Scope: "global", Content: "Aurora expired deploys require the grey gate.",
		ValidUntil: time.Now().Add(-time.Hour),
	})
	pinned := seedCanonicalMemory(t, mem, personID, memory.IntakeWrite{
		Target: "user", Scope: "global", Content: "Aurora pinned deploys require the gold gate.",
	})
	store, _ := mem.Canonical()
	if err := store.SetCanonicalPinned(context.Background(), personID, pinned.ID, true, "test"); err != nil {
		t.Fatal(err)
	}

	engine := NewRecallEngine(nil, mem, nil)
	slices, _ := engine.SelectForWorkspace(
		context.Background(),
		"default",
		personID,
		"ws-a",
		"",
		"check the aurora alpha blue gate release checklist",
	)
	refs := map[string]bool{}
	for _, slice := range slices {
		refs[slice.Ref] = true
	}
	if !refs[global.ID] || !refs[workspaceA.ID] {
		t.Fatalf("expected global and current-workspace memories, got %+v", slices)
	}
	for _, excluded := range []string{workspaceB.ID, expired.ID, pinned.ID} {
		if refs[excluded] {
			t.Fatalf("memory %s crossed a recall boundary: %+v", excluded, slices)
		}
	}

	other, _ := engine.SelectForWorkspace(
		context.Background(),
		"default",
		personID,
		"ws-a",
		"",
		"check the aurora beta red gate deployment",
	)
	for _, slice := range other {
		if slice.Ref == workspaceB.ID {
			t.Fatalf("workspace B memory leaked into workspace A: %+v", other)
		}
	}
}

func TestCanonicalRecallTouchesOnlyBudgetedSelections(t *testing.T) {
	mem := newRecallMemory(t)
	const personID = "person-canonical-touch"
	for i := 0; i < 5; i++ {
		seedCanonicalMemory(t, mem, personID, memory.IntakeWrite{
			Scope:   "global",
			Content: fmt.Sprintf("Gearbox release checklist variant %d.", i),
		})
	}
	engine := NewRecallEngine(nil, mem, nil)
	slices, _ := engine.Select(
		context.Background(),
		"default",
		personID,
		"",
		"review the gearbox release checklist variants",
	)
	if len(slices) != recallMaxSlices {
		t.Fatalf("expected %d budgeted memories, got %d", recallMaxSlices, len(slices))
	}
	selected := map[string]bool{}
	for _, slice := range slices {
		selected[slice.Ref] = true
	}

	store, _ := mem.Canonical()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := store.ListCanonicalMemories(context.Background(), personID, memory.CanonicalFilter{})
		if err != nil {
			t.Fatal(err)
		}
		touched := 0
		wrong := false
		for _, row := range rows {
			if !row.LastAccessedAt.IsZero() {
				touched++
				if !selected[row.ID] {
					wrong = true
				}
			}
		}
		if wrong {
			t.Fatalf("a non-selected canonical memory was marked accessed: %+v", rows)
		}
		if touched == len(selected) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("selected canonical memories were not marked accessed: selected=%v rows=%+v", selected, rows)
		}
		time.Sleep(10 * time.Millisecond)
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
	intent := daemon.classifyIntent(ctx, req.Content, req.Channel)
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

	// Persisted working history (P1 spine contract): the marker must not
	// exist in any persisted spine entry's user/assistant text.
	blobs, err := mem.GetLatestContext(ctx, identity.PersonID, kernel.SpineTrajectoryKey)
	if err != nil || len(blobs) == 0 {
		t.Fatalf("expected persisted spine history, err=%v blobs=%d", err, len(blobs))
	}
	for _, blob := range blobs {
		var entry struct {
			User      string `json:"user"`
			Assistant string `json:"assistant"`
		}
		if err := json.Unmarshal(blob, &entry); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(entry.User, marker) || strings.Contains(entry.Assistant, marker) {
			t.Fatalf("recall leaked into persisted spine entry: user=%q assistant=%q", entry.User, entry.Assistant)
		}
	}

	// Turn 2 continues the SAME task with an unrelated message: nothing the
	// model sees may still carry the turn-1 recall content.
	req2 := api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "now summarize the weather station findings", TaskID: taskID,
	}
	intent2 := daemon.classifyIntent(ctx, req2.Content, req2.Channel)
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
