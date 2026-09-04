package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
)

func msgs(pairs ...string) []llm.Message {
	out := make([]llm.Message, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, llm.Message{Role: pairs[i], Content: pairs[i+1]})
	}
	return out
}

func messagesContain(m []llm.Message, substr string) bool {
	for _, msg := range m {
		if strings.Contains(msg.Content, substr) {
			return true
		}
	}
	return false
}

// fakeTrajStore records trajectories per key so tests can verify how
// working-context history is keyed and read back. It embeds mockStorage for all
// the StorageProvider methods it does not care about.
type fakeTrajStore struct {
	mockStorage
	traj map[string][][]byte // trajectory key -> blobs, latest first
}

func (f *fakeTrajStore) SaveTrajectory(ctx context.Context, tenantID, channel string, traj []byte) error {
	if f.traj == nil {
		f.traj = map[string][][]byte{}
	}
	cp := make([]byte, len(traj))
	copy(cp, traj)
	f.traj[channel] = append([][]byte{cp}, f.traj[channel]...)
	return nil
}

func (f *fakeTrajStore) GetLatestContext(ctx context.Context, tenantID, channel string) ([][]byte, error) {
	return f.traj[channel], nil
}

func TestTrajectoryKey_PersonSpine(t *testing.T) {
	a := &Agent{}
	// ALL agent-bound turns of the person converge on the ONE spine key: two
	// different tasks, two different raw channels (WeChat openid vs a CLI UUID),
	// and taskless casual chat all share it. The storage tenant is the person,
	// so the constant key is person-scoped.
	ctxA := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "T1"})
	ctxB := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "T2"})
	keys := []string{
		a.trajectoryKey(ctxA, "wx-openid-123"),
		a.trajectoryKey(ctxB, "3f2504e0-4f89-41d3-9a0c-0305e82c3301"),
		a.trajectoryKey(context.Background(), "cli"),
		a.trajectoryKey(context.Background(), ""),
	}
	for i, got := range keys {
		if got != SpineTrajectoryKey {
			t.Fatalf("key %d: expected spine key %q, got %q", i, SpineTrajectoryKey, got)
		}
	}
}

func TestTrajectoryKey_InternalSubsystemsStayOffSpine(t *testing.T) {
	a := &Agent{}
	// Delegation sub-agents and background review are internal turns; they must
	// never append to the person's work spine.
	if got := a.trajectoryKey(context.Background(), "delegation"); got != "delegation" {
		t.Fatalf("delegation must stay channel-keyed, got %q", got)
	}
	if got := a.trajectoryKey(context.Background(), "cli:background_review"); got != "cli:background_review" {
		t.Fatalf("background review must stay channel-keyed, got %q", got)
	}
}

func TestTrajectoryFallbackKeys_LegacyChain(t *testing.T) {
	a := &Agent{}
	// Task-bound turn: old task key first, then the task's prior run channel.
	ctxTask := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "T1", PriorChannel: "wx-openid-123"})
	got := a.trajectoryFallbackKeys(ctxTask, "cli")
	want := []string{"task:T1", "wx-openid-123"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("task-bound fallback chain = %v, want %v", got, want)
	}
	// Taskless turn: the old channel-derived key (bare UUID collapses to the
	// stable per-person key).
	if got := a.trajectoryFallbackKeys(context.Background(), "3f2504e0-4f89-41d3-9a0c-0305e82c3301"); len(got) != 1 || got[0] != "session" {
		t.Fatalf("taskless UUID fallback = %v, want [session]", got)
	}
	if got := a.trajectoryFallbackKeys(context.Background(), "wechat"); len(got) != 1 || got[0] != "wechat" {
		t.Fatalf("taskless stable-channel fallback = %v, want [wechat]", got)
	}
}

func TestLooksLikeSessionUUID(t *testing.T) {
	cases := map[string]bool{
		"3f2504e0-4f89-41d3-9a0c-0305e82c3301": true,
		"cli":                                  false,
		"wechat":                               false,
		"task:abc":                             false,
		"3f2504e04f8941d39a0c0305e82c3301":     false, // no dashes
		"":                                     false,
	}
	for in, want := range cases {
		if got := looksLikeSessionUUID(in); got != want {
			t.Errorf("looksLikeSessionUUID(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestSaveHistory_SpineEntryIsSlim: a turn with tool calls persists ONE
// turn-level entry — user text + assistant final answer + harvested paths —
// never tool payloads or the system prompt.
func TestSaveHistory_SpineEntryIsSlim(t *testing.T) {
	store := &fakeTrajStore{}
	mem := memory.NewMemoryManager(store)
	a := NewAgent(mem, &planningBackend{}, &recordingLLMProvider{}, "helpful", 1, 1, nil)

	turn := []llm.Message{
		{Role: "system", Content: "SECRET SYSTEM PROMPT"},
		{Role: "user", Content: "build the game"},
		{Role: "assistant", Content: "writing files", ToolCalls: []llm.ToolCall{{Function: "write_file", Args: `{"path":"game/index.html"}`}}},
		{Role: "tool", Content: "HUGE TOOL OUTPUT PAYLOAD"},
		{Role: "assistant", Content: "done, the game is at game/index.html"},
	}
	ctxTask := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "T-game"})
	a.saveHistory(ctxTask, "person1", SpineTrajectoryKey, "cli", "build the game", "done, the game is at game/index.html", turn)

	blobs := store.traj[SpineTrajectoryKey]
	if len(blobs) != 1 {
		t.Fatalf("expected exactly one spine entry, got %d", len(blobs))
	}
	entry, ok := parseSpineEntry(blobs[0])
	if !ok {
		t.Fatalf("spine blob is not a turn-level entry: %s", blobs[0])
	}
	if entry.User != "build the game" {
		t.Fatalf("entry user = %q", entry.User)
	}
	if entry.Assistant != "done, the game is at game/index.html" {
		t.Fatalf("entry assistant = %q", entry.Assistant)
	}
	if len(entry.Files) != 1 || entry.Files[0] != "game/index.html" {
		t.Fatalf("entry files = %v, want the harvested tool-arg path", entry.Files)
	}
	if entry.TaskID != "T-game" {
		t.Fatalf("entry task label = %q", entry.TaskID)
	}
	raw := string(blobs[0])
	for _, forbidden := range []string{"SECRET SYSTEM PROMPT", "HUGE TOOL OUTPUT PAYLOAD", "tool_calls"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("spine entry leaked %q:\n%s", forbidden, raw)
		}
	}
}

// TestSaveHistory_StripsGatewayDecoration: the daemon/resume context blocks the
// gateway prepends to the agent input must not become the spine's user text.
func TestSaveHistory_StripsGatewayDecoration(t *testing.T) {
	store := &fakeTrajStore{}
	mem := memory.NewMemoryManager(store)
	a := NewAgent(mem, &planningBackend{}, &recordingLLMProvider{}, "helpful", 1, 1, nil)

	decorated := "[SelfMind daemon context]\nworkspace_root: /tmp/ws\n[/SelfMind daemon context]\n\n" +
		"[SelfMind resume context]\ntask_id: t1\n[/SelfMind resume context]\n\n" +
		"continue the game"
	a.saveHistory(context.Background(), "person1", SpineTrajectoryKey, "cli", decorated, "ok", nil)

	entry, ok := parseSpineEntry(store.traj[SpineTrajectoryKey][0])
	if !ok {
		t.Fatal("expected spine entry")
	}
	if entry.User != "continue the game" {
		t.Fatalf("decoration not stripped, user = %q", entry.User)
	}
}

// TestSpine_CrossChannelCrossTaskTail: same person, two channels, two different
// tasks → ONE spine; the tail replays across them in completion order.
func TestSpine_CrossChannelCrossTaskTail(t *testing.T) {
	store := &fakeTrajStore{}
	mem := memory.NewMemoryManager(store)
	a := NewAgent(mem, &planningBackend{}, &recordingLLMProvider{}, "helpful", 1, 1, nil)

	// Turn 1: task A on WeChat.
	ctxA := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "game-97"})
	a.saveHistory(ctxA, "person1", a.trajectoryKey(ctxA, "wx-openid-123"), "wx-openid-123",
		"用JS写九七游戏", "game built at 97/index.html", nil)
	// Turn 2: task B on CLI (fresh UUID channel).
	ctxB := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "stock-summary"})
	a.saveHistory(ctxB, "person1", a.trajectoryKey(ctxB, "3f2504e0-4f89-41d3-9a0c-0305e82c3301"), "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"帮我总结今天股市", "market summary done", nil)

	// Turn 3 (either endpoint): the composed window sees BOTH prior turns, in order.
	got, err := a.contextEngine.BuildMessages(context.Background(), mem, "person1",
		SpineTrajectoryKey, a.trajectoryFallbackKeys(context.Background(), "cli"), "sys", "再多做几个角色")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	idxGame, idxStock := -1, -1
	for i, m := range got {
		if strings.Contains(m.Content, "九七游戏") && idxGame < 0 {
			idxGame = i
		}
		if strings.Contains(m.Content, "股市") && !strings.Contains(m.Content, "再多做") {
			idxStock = i
		}
	}
	if idxGame < 0 || idxStock < 0 {
		t.Fatalf("spine tail missing turns: game=%d stock=%d\n%#v", idxGame, idxStock, got)
	}
	if idxGame > idxStock {
		t.Fatalf("spine tail out of completion order: game@%d after stock@%d", idxGame, idxStock)
	}
	if got[len(got)-1].Content != "再多做几个角色" {
		t.Fatalf("latest user message must be last, got %q", got[len(got)-1].Content)
	}
}

// TestSpine_LegacyTaskKeyCompatRead: history stored only under the old
// `task:<id>` key is still loaded on the first spine turn, and after one save
// the spine key carries the turn.
func TestSpine_LegacyTaskKeyCompatRead(t *testing.T) {
	store := &fakeTrajStore{}
	mem := memory.NewMemoryManager(store)
	a := NewAgent(mem, &planningBackend{}, &recordingLLMProvider{}, "helpful", 1, 1, nil)

	// Pre-spine task history lives under task:<id> (the 2026-07-06 layout).
	_ = store.SaveTrajectory(context.Background(), "person1", "task:order-sys",
		[]byte(`{"messages":[{"role":"user","content":"legacy order work"},{"role":"assistant","content":"module scaffolded"}]}`))

	ctxTask := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "order-sys"})
	key := a.trajectoryKey(ctxTask, "cli")
	fallbacks := a.trajectoryFallbackKeys(ctxTask, "cli")
	got, err := a.contextEngine.BuildMessages(ctxTask, mem, "person1", key, fallbacks, "sys", "continue")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	if !messagesContain(got, "legacy order work") {
		t.Fatalf("first spine load lost the legacy task-keyed history: %#v", got)
	}

	// One save migrates forward: the spine key now carries the turn.
	a.saveHistory(ctxTask, "person1", key, "cli", "continue", "resumed the order module", got)
	if len(store.traj[SpineTrajectoryKey]) != 1 {
		t.Fatalf("expected the turn under the spine key after save, got %v", store.traj)
	}
	if entry, ok := parseSpineEntry(store.traj[SpineTrajectoryKey][0]); !ok || entry.TaskID != "order-sys" {
		t.Fatalf("spine entry missing after migration save: %v %v", entry, ok)
	}
}

// TestSpine_LegacyTaskReadWorksEvenWhenSpineNotEmpty: a legacy task resumed
// AFTER unrelated turns already populated the spine still gets its compat read
// (the tail carries no entry for that task).
func TestSpine_LegacyTaskReadWorksEvenWhenSpineNotEmpty(t *testing.T) {
	store := &fakeTrajStore{}
	mem := memory.NewMemoryManager(store)
	a := NewAgent(mem, &planningBackend{}, &recordingLLMProvider{}, "helpful", 1, 1, nil)

	// Casual turn already on the spine.
	a.saveHistory(context.Background(), "person1", SpineTrajectoryKey, "cli", "hi", "hello!", nil)
	// Legacy pre-spine task history.
	_ = store.SaveTrajectory(context.Background(), "person1", "task:order-sys",
		[]byte(`{"messages":[{"role":"user","content":"legacy order work"}]}`))

	ctxTask := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "order-sys"})
	got, err := a.contextEngine.BuildMessages(ctxTask, mem, "person1",
		SpineTrajectoryKey, a.trajectoryFallbackKeys(ctxTask, "cli"), "sys", "continue")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	if !messagesContain(got, "legacy order work") {
		t.Fatalf("legacy task went amnesiac because the spine was non-empty: %#v", got)
	}
	if !messagesContain(got, "hello!") {
		t.Fatalf("spine tail dropped by the compat read: %#v", got)
	}
}

// TestSpine_PriorChannelCompatRead: the oldest layout (channel-keyed task
// history) is still reachable through TaskRuntimeContext.PriorChannel.
func TestSpine_PriorChannelCompatRead(t *testing.T) {
	store := &fakeTrajStore{}
	mem := memory.NewMemoryManager(store)
	a := NewAgent(mem, &planningBackend{}, &recordingLLMProvider{}, "helpful", 1, 1, nil)

	_ = store.SaveTrajectory(context.Background(), "person1", "wx-openid-123",
		[]byte(`{"messages":[{"role":"user","content":"channel-era order work"}]}`))

	ctxTask := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "order-sys", PriorChannel: "wx-openid-123"})
	got, err := a.contextEngine.BuildMessages(ctxTask, mem, "person1",
		a.trajectoryKey(ctxTask, "cli"), a.trajectoryFallbackKeys(ctxTask, "cli"), "sys", "continue")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	if !messagesContain(got, "channel-era order work") {
		t.Fatalf("PriorChannel compat read did not recover channel-keyed history: %#v", got)
	}
}

// TestSpineTail_SourceTagAndFiles: non-interactive entries replay with their
// source tag, and touched files survive into the tail.
func TestSpineTail_SourceTagAndFiles(t *testing.T) {
	store := &fakeTrajStore{}
	mem := memory.NewMemoryManager(store)
	a := NewAgent(mem, &planningBackend{}, &recordingLLMProvider{}, "helpful", 1, 1, nil)

	cronCtx := WithTurnSource(context.Background(), "cron")
	turn := []llm.Message{
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{{Function: "write_file", Args: `{"path":"report/daily.md"}`}}},
	}
	a.saveHistory(cronCtx, "person1", SpineTrajectoryKey, "wechat", "daily market summary", "summary written", turn)

	got, err := a.contextEngine.BuildMessages(context.Background(), mem, "person1",
		SpineTrajectoryKey, nil, "sys", "next")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	if !messagesContain(got, "[cron] daily market summary") {
		t.Fatalf("cron source tag missing from spine tail: %#v", got)
	}
	if !messagesContain(got, "report/daily.md") {
		t.Fatalf("touched file path missing from spine tail: %#v", got)
	}
}

// TestSpineTail_BoundedEntries: the tail replays at most
// composerSpineTailEntries turns, newest kept.
func TestSpineTail_BoundedEntries(t *testing.T) {
	var blobs [][]byte
	total := composerSpineTailEntries + 5
	for i := 0; i < total; i++ {
		b, _ := json.Marshal(spineEntry{Kind: spineEntryKind, User: fmt.Sprintf("turn-%d", i), Assistant: "ok"})
		blobs = append([][]byte{b}, blobs...) // latest first
	}
	got := spineTailMessages(blobs)
	if want := composerSpineTailEntries * 2; len(got) != want {
		t.Fatalf("expected %d messages, got %d", want, len(got))
	}
	if got[0].Content != fmt.Sprintf("turn-%d", total-composerSpineTailEntries) {
		t.Fatalf("oldest kept entry = %q", got[0].Content)
	}
	if got[len(got)-2].Content != fmt.Sprintf("turn-%d", total-1) {
		t.Fatalf("newest entry = %q", got[len(got)-2].Content)
	}
}

// TestCompaction_OverSpineShapedHistory: engine A still protects head + tail
// and summarizes the middle when the window is spine-shaped (alternating slim
// user/assistant turns).
func TestCompaction_OverSpineShapedHistory(t *testing.T) {
	provider := &fakeSummarizer{reply: "## Active Task\ninterleaved work"}
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(provider)

	big := strings.Repeat("spine narrative ", 30)
	messages := []llm.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "original task"},
	}
	for i := 0; i < 6; i++ {
		messages = append(messages,
			llm.Message{Role: "user", Content: fmt.Sprintf("turn-%d %s", i, big)},
			llm.Message{Role: "assistant", Content: fmt.Sprintf("answer-%d %s", i, big)},
		)
	}
	for i := 0; i < compactionTailTurns; i++ {
		messages = append(messages, llm.Message{Role: "user", Content: fmt.Sprintf("recent-%d", i)})
	}

	got := engine.TruncateMessages(messages)
	if provider.calls.Load() != 1 {
		t.Fatalf("expected one compaction call, got %d", provider.calls.Load())
	}
	if got[0].Content != "sys" || got[1].Content != "original task" {
		t.Fatalf("head not protected: %+v", got[:2])
	}
	if !strings.Contains(got[2].Content, "[CONTEXT COMPACTION") {
		t.Fatalf("expected summary at index 2: %+v", got[2])
	}
	for i := 0; i < compactionTailTurns; i++ {
		if want := fmt.Sprintf("recent-%d", i); got[len(got)-compactionTailTurns+i].Content != want {
			t.Fatalf("tail not protected at %d", i)
		}
	}
}

// TestBoundaryNote_PresentOnlyWithSummary: the verbatim boundary note rides on
// every rendered compaction summary and appears nowhere else.
func TestBoundaryNote_PresentOnlyWithSummary(t *testing.T) {
	provider := &fakeSummarizer{reply: "## Active Task\nkeep going"}
	engine := NewContextEngine(200, 10)
	engine.SetSummaryProvider(provider)

	got := engine.TruncateMessages(compactionFixture())
	found := false
	for _, m := range got {
		if strings.Contains(m.Content, compactionBoundaryNote) {
			if !strings.Contains(m.Content, "[CONTEXT COMPACTION") {
				t.Fatalf("boundary note outside the summary message: %+v", m)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("compaction summary present but boundary note missing")
	}

	// Under budget: no summary, no note.
	small := NewContextEngine(100000, 512)
	small.SetSummaryProvider(provider)
	plain := small.TruncateMessages(msgs("user", "hi", "assistant", "hello"))
	for _, m := range plain {
		if strings.Contains(m.Content, compactionBoundaryNote) {
			t.Fatalf("boundary note must be absent without a summary: %+v", m)
		}
	}
}

// TestSpine_CasualAndTaskTurnsInterleave: casual chat and task turns share the
// spine without breaking budgets or ordering.
func TestSpine_CasualAndTaskTurnsInterleave(t *testing.T) {
	store := &fakeTrajStore{}
	mem := memory.NewMemoryManager(store)
	a := NewAgent(mem, &planningBackend{}, &recordingLLMProvider{}, "helpful", 1, 1, nil)

	ctxTask := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "T1"})
	a.saveHistory(context.Background(), "person1", SpineTrajectoryKey, "cli", "hello", "hi there", nil)
	a.saveHistory(ctxTask, "person1", SpineTrajectoryKey, "cli", "build feature X", "feature X done", nil)
	a.saveHistory(context.Background(), "person1", SpineTrajectoryKey, "wechat", "thanks", "welcome", nil)

	got, err := a.contextEngine.BuildMessages(context.Background(), mem, "person1",
		SpineTrajectoryKey, nil, "sys", "what did we just do?")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	for _, want := range []string{"hi there", "feature X done", "welcome"} {
		if !messagesContain(got, want) {
			t.Fatalf("interleaved spine tail missing %q: %#v", want, got)
		}
	}
}

// TestSessionKey_RunDerived pins the retrieval key. It is the run, a fact, not
// the Task, a judgment: keying on Task meant a mis-grouped thread put unrelated
// conversations into one searchable session. Coherence across a continued line
// of work is recovered at read time from the resume edge.
func TestSessionKey_RunDerived(t *testing.T) {
	a := &Agent{}
	ctxRun := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "T9", RunID: "R7"})
	if got := a.sessionKey(ctxRun, nil); got != "run:R7" {
		t.Fatalf("expected run-derived session id run:R7, got %q", got)
	}
	// A Task without a run must not resurrect the task key.
	ctxTaskOnly := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "T9"})
	if got := a.sessionKey(ctxTaskOnly, msgs("user", "hello")); strings.HasPrefix(got, "task:") {
		t.Fatalf("a task must never key a session: %q", got)
	}
	// Runless turns keep a per-content id (non-empty, not work-prefixed).
	got := a.sessionKey(context.Background(), msgs("user", "hello"))
	if got == "" || strings.HasPrefix(got, "run:") || strings.HasPrefix(got, "task:") {
		t.Fatalf("runless session id should be per-content, got %q", got)
	}
}

func TestSystemPromptNote_InspectBeforeBuild(t *testing.T) {
	const phrase = "search the workspace for an existing implementation"
	// A write-capable coding turn gets the inspect-before-build rule.
	write := DefaultTaskStrategy() // ToolMode defaults to full (write-capable)
	if !strings.Contains(write.SystemPromptNote(), phrase) {
		t.Fatalf("write-capable turn should include inspect-before-build rule")
	}
	// A pure direct-answer turn (no tools) must not get it.
	none := DefaultTaskStrategy()
	none.ToolMode = ToolModeNone
	if strings.Contains(none.SystemPromptNote(), phrase) {
		t.Fatalf("direct-answer (no-tools) turn should not include inspect-before-build rule")
	}
}

func TestSystemPromptNote_ExecSandboxNote(t *testing.T) {
	const note = "Shell/exec tools run inside an OS sandbox: NETWORK IS DISABLED test-note.\n"
	// A tool-capable turn renders the gateway-supplied sandbox note verbatim.
	withTools := DefaultTaskStrategy()
	withTools.ExecSandboxNote = note
	if !strings.Contains(withTools.SystemPromptNote(), "NETWORK IS DISABLED test-note") {
		t.Fatalf("tool-capable turn should include the exec sandbox note")
	}
	// A direct-answer turn never mentions the execution environment.
	none := DefaultTaskStrategy()
	none.ToolMode = ToolModeNone
	none.ExecSandboxNote = note
	if strings.Contains(none.SystemPromptNote(), "test-note") {
		t.Fatalf("no-tools turn should not include the exec sandbox note")
	}
	// Empty note adds nothing.
	empty := DefaultTaskStrategy()
	if strings.Contains(empty.SystemPromptNote(), "OS sandbox") {
		t.Fatalf("empty note must not inject sandbox text")
	}
}
