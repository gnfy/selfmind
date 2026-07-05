package kernel

import (
	"context"
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

// fakeTrajStore records trajectories per channel key so tests can verify how
// working-context history is keyed and read back. It embeds mockStorage for all
// the StorageProvider methods it does not care about.
type fakeTrajStore struct {
	mockStorage
	traj map[string][][]byte // channel key -> blobs, latest first
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

func TestTrajectoryKey_TaskScoped(t *testing.T) {
	a := &Agent{}
	// Same task, two different raw channel values (WeChat openid vs a CLI UUID)
	// resolve to ONE shared key so the task's history follows it cross-endpoint.
	ctxA := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "T1"})
	ctxB := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "T1"})
	keyA := a.trajectoryKey(ctxA, "wx-openid-123")
	keyB := a.trajectoryKey(ctxB, "3f2504e0-4f89-41d3-9a0c-0305e82c3301")
	if keyA != "task:T1" || keyB != "task:T1" {
		t.Fatalf("expected both keys to be task:T1, got %q and %q", keyA, keyB)
	}
	if keyA != keyB {
		t.Fatalf("same task must yield the same trajectory key: %q != %q", keyA, keyB)
	}
}

func TestTrajectoryKey_TasklessFallback(t *testing.T) {
	a := &Agent{}
	// A bare per-session UUID collapses to a stable per-person key (not the UUID)
	// so casual history survives restarts.
	if got := a.trajectoryKey(context.Background(), "3f2504e0-4f89-41d3-9a0c-0305e82c3301"); got == "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Fatalf("a bare UUID channel must not be used as the taskless key, got %q", got)
	} else if got != "session" {
		t.Fatalf("expected stable fallback key 'session', got %q", got)
	}
	// A stable channel (openid, "cli", "wechat") is kept as-is and casual chat on
	// two platforms stays in separate buckets.
	cli := a.trajectoryKey(context.Background(), "cli")
	wx := a.trajectoryKey(context.Background(), "wechat")
	if cli != "cli" || wx != "wechat" {
		t.Fatalf("stable channels must be kept as-is, got cli=%q wx=%q", cli, wx)
	}
	if cli == wx {
		t.Fatalf("casual chat on two platforms must stay separate: %q == %q", cli, wx)
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

func TestBuildMessages_CrossEndpointRoundTrip(t *testing.T) {
	store := &fakeTrajStore{}
	mem := memory.NewMemoryManager(store)
	a := NewAgent(mem, &planningBackend{}, &recordingLLMProvider{}, "helpful", 1, 1, nil)

	// Endpoint A (say WeChat) saves the task's history under the task key.
	ctxTask := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "order-sys"})
	keyA := a.trajectoryKey(ctxTask, "wx-openid-123")
	a.saveHistory(ctxTask, "person1", keyA, msgs("user", "build the order module", "assistant", "starting on it"))

	// Endpoint B (CLI, fresh UUID channel) resumes the SAME task and must load
	// the shared history.
	keyB := a.trajectoryKey(ctxTask, "3f2504e0-4f89-41d3-9a0c-0305e82c3301")
	fallbackB := a.trajectoryFallbackKey(ctxTask, keyB)
	msgs, err := a.contextEngine.BuildMessages(ctxTask, mem, "person1", keyB, fallbackB, "sys", "continue")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	if !messagesContain(msgs, "build the order module") {
		t.Fatalf("CLI resume did not load the task history saved on the other endpoint: %#v", msgs)
	}
}

func TestBuildMessages_BackwardCompatFallbackRead(t *testing.T) {
	store := &fakeTrajStore{}
	mem := memory.NewMemoryManager(store)
	a := NewAgent(mem, &planningBackend{}, &recordingLLMProvider{}, "helpful", 1, 1, nil)

	// A pre-change task stored its transcript channel-keyed (under its prior run
	// channel), never under the task key.
	_ = store.SaveTrajectory(context.Background(), "person1", "wx-openid-123",
		[]byte(`{"messages":[{"role":"user","content":"legacy order work"}]}`))

	ctxTask := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "order-sys", PriorChannel: "wx-openid-123"})
	primary := a.trajectoryKey(ctxTask, "cli")
	fallback := a.trajectoryFallbackKey(ctxTask, primary)
	if fallback != "wx-openid-123" {
		t.Fatalf("expected fallback key wx-openid-123, got %q", fallback)
	}
	msgs, err := a.contextEngine.BuildMessages(ctxTask, mem, "person1", primary, fallback, "sys", "continue")
	if err != nil {
		t.Fatalf("BuildMessages: %v", err)
	}
	if !messagesContain(msgs, "legacy order work") {
		t.Fatalf("backward-compat read did not recover pre-change task history: %#v", msgs)
	}
}

func TestSessionKey_TaskDerived(t *testing.T) {
	a := &Agent{}
	ctxTask := WithTaskRuntimeContext(context.Background(), TaskRuntimeContext{TaskID: "T9"})
	if got := a.sessionKey(ctxTask, nil); got != "task:T9" {
		t.Fatalf("expected task-derived session id task:T9, got %q", got)
	}
	// Taskless turns keep a per-content id (non-empty, not task-prefixed).
	got := a.sessionKey(context.Background(), msgs("user", "hello"))
	if got == "" || strings.HasPrefix(got, "task:") {
		t.Fatalf("taskless session id should be per-content, got %q", got)
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
