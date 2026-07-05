package httpapi

// Fix 1: implicit continuation. When the rules classifier lands on IntentTask
// (no explicit cue) but the person has a recently active resumable task, the
// hybrid LLM may upgrade the turn to IntentContinue so the follow-up attaches to
// that task instead of spawning a fresh, context-less one.

import (
	"context"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel/llm"
)

// scriptedIntentProvider returns a fixed body and counts Chat calls so a test
// can assert whether the intent LLM was consulted.
type scriptedIntentProvider struct {
	content string
	calls   int
}

func (p *scriptedIntentProvider) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	return p.content, nil
}

func (p *scriptedIntentProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.calls++
	return &llm.ChatResponse{Content: p.content}, nil
}

func (p *scriptedIntentProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Content: p.content}
	close(ch)
	return ch, nil
}

func newContinuationServer(t *testing.T, provider *scriptedIntentProvider, mode string, window time.Duration) (*Server, *control.Store, *control.IdentityContext) {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "weixin", "wxid_1", "Me")
	if err != nil {
		t.Fatal(err)
	}
	gw := router.NewGateway(nil, nil, nil, provider)
	gw.SetIntentClassifier(router.NewIntentClassifierWithRules(router.IntentRuleConfig{Mode: mode}))
	daemon := &Server{Control: store, Gateway: gw, DefaultTenantID: "default", ContinueWindow: window}
	return daemon, store, identity
}

func createRecentTask(t *testing.T, store *control.Store, identity *control.IdentityContext) *control.Task {
	t.Helper()
	task, err := store.CreateTask(context.Background(), control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "build a KOF fighting game", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Move it off 'new' so it is a resumable non-terminal task with a summary.
	if err := store.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, "in_progress", "generated index.html", nil); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetTask(context.Background(), identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestImplicitContinuationUpgradeAttachesRecentTask: rules→IntentTask + recent
// resumable task + LLM says "continue" ⇒ IntentContinue that resolveTask
// attaches to the recent task.
func TestImplicitContinuationUpgradeAttachesRecentTask(t *testing.T) {
	provider := &scriptedIntentProvider{content: `{"decision":"continue","confidence":0.9,"reason":"reacting to the game"}`}
	daemon, store, identity := newContinuationServer(t, provider, "hybrid", 30*time.Minute)
	recent := createRecentTask(t, store, identity)
	ctx := context.Background()

	intent := daemon.classifyIntent(ctx, identity, "质量太差了,人物也不真实。", "weixin")
	if intent.Intent != router.IntentContinue || intent.Source != "llm" {
		t.Fatalf("expected llm continuation upgrade, got %+v", intent)
	}
	if provider.calls != 1 {
		t.Fatalf("expected exactly one intent llm call, got %d", provider.calls)
	}

	task, err := daemon.coordinator().resolveTask(ctx, identity,
		api.MessageRequest{Content: "质量太差了,人物也不真实。", Channel: "weixin"}, intent)
	if err != nil {
		t.Fatalf("resolveTask: %v", err)
	}
	if task == nil || task.ID != recent.ID {
		t.Fatalf("continuation must attach to the recent task %s, got %+v", recent.ID, task)
	}
}

// TestImplicitContinuationNewWorkKeepsTask: LLM says "new" ⇒ the turn stays
// IntentTask and resolveTask creates its OWN task.
func TestImplicitContinuationNewWorkKeepsTask(t *testing.T) {
	provider := &scriptedIntentProvider{content: `{"decision":"new","confidence":0.8}`}
	daemon, store, identity := newContinuationServer(t, provider, "hybrid", 30*time.Minute)
	recent := createRecentTask(t, store, identity)
	ctx := context.Background()

	intent := daemon.classifyIntent(ctx, identity, "write a python script instead", "weixin")
	if intent.Intent != router.IntentTask {
		t.Fatalf("a 'new' verdict must keep IntentTask, got %+v", intent)
	}
	task, err := daemon.coordinator().resolveTask(ctx, identity,
		api.MessageRequest{Content: "write a python script instead", Channel: "weixin"}, intent)
	if err != nil {
		t.Fatalf("resolveTask: %v", err)
	}
	if task == nil || task.ID == recent.ID {
		t.Fatalf("new work must get its own task, got %+v (recent %s)", task, recent.ID)
	}
}

// TestImplicitContinuationGarbageKeepsTask: unparsable LLM output never upgrades.
func TestImplicitContinuationGarbageKeepsTask(t *testing.T) {
	provider := &scriptedIntentProvider{content: "definitely not json"}
	daemon, store, identity := newContinuationServer(t, provider, "hybrid", 30*time.Minute)
	createRecentTask(t, store, identity)

	intent := daemon.classifyIntent(context.Background(), identity, "质量太差了", "weixin")
	if intent.Intent != router.IntentTask {
		t.Fatalf("garbage llm output must keep IntentTask, got %+v", intent)
	}
	if provider.calls != 1 {
		t.Fatalf("garbage path still consults once, got %d calls", provider.calls)
	}
}

// TestImplicitContinuationWindowZeroDisables: window=0 means the exception is
// off — the LLM is never consulted and the turn stays IntentTask.
func TestImplicitContinuationWindowZeroDisables(t *testing.T) {
	provider := &scriptedIntentProvider{content: `{"decision":"continue","confidence":0.9}`}
	daemon, store, identity := newContinuationServer(t, provider, "hybrid", 0)
	createRecentTask(t, store, identity)

	intent := daemon.classifyIntent(context.Background(), identity, "质量太差了", "weixin")
	if intent.Intent != router.IntentTask {
		t.Fatalf("window=0 must keep IntentTask, got %+v", intent)
	}
	if provider.calls != 0 {
		t.Fatalf("window=0 must not consult the llm, got %d calls", provider.calls)
	}
}

// TestImplicitContinuationRulesModeNeverConsults: rules mode never calls the LLM
// even with a recent task in the window.
func TestImplicitContinuationRulesModeNeverConsults(t *testing.T) {
	provider := &scriptedIntentProvider{content: `{"decision":"continue","confidence":0.9}`}
	daemon, store, identity := newContinuationServer(t, provider, "rules", 30*time.Minute)
	createRecentTask(t, store, identity)

	intent := daemon.classifyIntent(context.Background(), identity, "质量太差了", "weixin")
	if intent.Intent != router.IntentTask {
		t.Fatalf("rules mode must keep IntentTask, got %+v", intent)
	}
	if provider.calls != 0 {
		t.Fatalf("rules mode must not consult the llm, got %d calls", provider.calls)
	}
}

// TestImplicitContinuationNoRecentTaskNoConsult: with no resumable task in the
// window, the upgrade path is skipped entirely (no LLM call).
func TestImplicitContinuationNoRecentTaskNoConsult(t *testing.T) {
	provider := &scriptedIntentProvider{content: `{"decision":"continue","confidence":0.9}`}
	daemon, _, identity := newContinuationServer(t, provider, "hybrid", 30*time.Minute)

	intent := daemon.classifyIntent(context.Background(), identity, "质量太差了", "weixin")
	if intent.Intent != router.IntentTask {
		t.Fatalf("no recent task must keep IntentTask, got %+v", intent)
	}
	if provider.calls != 0 {
		t.Fatalf("no recent task must not consult the llm, got %d calls", provider.calls)
	}
}
