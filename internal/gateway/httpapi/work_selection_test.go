package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
)

func TestCommitWorkSelectionQueuesExactContinuation(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "weixin", "wx-user", "WX")
	targetTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace-parent", Title: "old release", Channel: "cli"})
	targetRun, _ := store.StartRun(ctx, targetTask, "cli", "old release")
	_ = store.FinishRun(ctx, identity.TenantID, targetRun.ID, "interrupted")
	interactionTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "continue old release", Channel: "weixin"})
	interactionRun, _ := store.StartRun(ctx, interactionTask, "weixin", "continue old release")
	_, _ = store.AppendEvent(ctx, control.Event{
		TaskID: interactionTask.ID, RunID: interactionRun.ID, Type: "work.selection", Visibility: "task",
		Payload: json.RawMessage(`{"action":"resume","run_id":"` + targetRun.ID + `","task_id":"` + targetTask.ID + `"}`),
	})
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	commit, err := daemon.coordinator().commitWorkSelection(ctx, identity, api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx-user", Channel: "weixin", Content: "continue old release and verify it",
	}, interactionTask, interactionRun)
	if err != nil {
		t.Fatal(err)
	}
	if commit == nil || commit.Action != "resume" || commit.QueueID == "" || commit.Rejected {
		t.Fatalf("commit = %+v", commit)
	}
	queued, _ := store.GetQueued(ctx, identity.TenantID, commit.QueueID)
	if queued == nil || queued.ReplyToRunID != targetRun.ID || queued.Content != "continue old release and verify it" {
		t.Fatalf("queued continuation = %+v", queued)
	}
}

func TestCommitWorkSelectionClaimsSameDomainInteractionDirectly(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	targetTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace", Title: "old release", Channel: "cli"})
	targetRun, _ := store.StartRun(ctx, targetTask, "cli", "old release")
	_ = store.FinishRun(ctx, identity.TenantID, targetRun.ID, "interrupted")
	interactionTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: "workspace", Title: "continue old release", Channel: "cli"})
	interactionRun, _ := store.StartRun(ctx, interactionTask, "cli", "continue old release")
	_, _ = store.AppendEvent(ctx, control.Event{
		TaskID: interactionTask.ID, RunID: interactionRun.ID, Type: "work.selection", Visibility: "task",
		Payload: json.RawMessage(`{"action":"resume","run_id":"` + targetRun.ID + `","task_id":"` + targetTask.ID + `"}`),
	})
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	commit, err := daemon.coordinator().commitWorkSelection(ctx, identity, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "continue old release",
	}, interactionTask, interactionRun)
	if err != nil {
		t.Fatal(err)
	}
	if commit == nil || !commit.Direct || commit.QueueID != "" || commit.Task == nil || commit.Task.ID != targetTask.ID {
		t.Fatalf("commit = %+v", commit)
	}
	if interactionRun.TaskID != targetTask.ID || interactionRun.ParentRunID != targetRun.ID {
		t.Fatalf("interaction run was not updated in place: %+v", interactionRun)
	}
	if placeholder, _ := store.GetTask(ctx, identity.TenantID, interactionTask.ID); placeholder != nil {
		t.Fatalf("placeholder survived direct commit: %+v", placeholder)
	}
}

func TestCommitWorkSelectionStopsAfterMaterialEffect(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	targetTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "target", Channel: "cli"})
	targetRun, _ := store.StartRun(ctx, targetTask, "cli", "target")
	_ = store.FinishRun(ctx, identity.TenantID, targetRun.ID, "interrupted")
	interactionTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "interaction", Channel: "cli"})
	interactionRun, _ := store.StartRun(ctx, interactionTask, "cli", "interaction")
	_, _ = store.AppendEvent(ctx, control.Event{TaskID: interactionTask.ID, RunID: interactionRun.ID, Type: "work.selection", Visibility: "task", Payload: json.RawMessage(`{"action":"resume","run_id":"` + targetRun.ID + `"}`)})
	claim, _ := store.ClaimToolDispatch(ctx, identity.TenantID, control.ToolLedgerEntry{RunID: interactionRun.ID, ToolCallID: "write", ToolName: "patch", ArgsHash: "x", RetryClass: "idempotent"})
	if !claim.Execute {
		t.Fatal("failed to seed material effect")
	}
	_ = store.RecordToolOutcome(ctx, identity.TenantID, interactionRun.ID, "write", true)
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	commit, err := daemon.coordinator().commitWorkSelection(ctx, identity, api.MessageRequest{Platform: "cli", Channel: "cli", Content: "continue target"}, interactionTask, interactionRun)
	if err != nil {
		t.Fatal(err)
	}
	if commit == nil || !commit.Rejected || !strings.Contains(commit.Notice, "already produced") {
		t.Fatalf("commit = %+v", commit)
	}
	queued, _ := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if len(queued) != 0 {
		t.Fatalf("post-effect selection queued work: %+v", queued)
	}
}

type workSelectionProvider struct {
	mu           sync.Mutex
	calls        int
	targetRunID  string
	action       string
	childStarted chan struct{}
	releaseChild chan struct{}
	once         sync.Once
	releaseOnce  sync.Once
}

type correctingWorkSelectionProvider struct {
	wrongRunID, correctRunID string
	mu                       sync.Mutex
	calls                    int
	firstSelection           chan struct{}
	release                  chan struct{}
	once                     sync.Once
}

func (p *correctingWorkSelectionProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "done", nil
}
func (p *correctingWorkSelectionProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}
func (p *correctingWorkSelectionProvider) StreamChat(ctx context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	out := make(chan llm.StreamEvent, 1)
	go func() {
		defer close(out)
		switch call {
		case 1:
			out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "select-wrong", Function: "work_select", Args: `{"action":"resume","run_id":"` + p.wrongRunID + `"}`}}}
		case 2:
			p.once.Do(func() { close(p.firstSelection) })
			select {
			case <-p.release:
				out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "inspect-after-steer", Function: "work_inspect", Args: `{"run_id":"` + p.wrongRunID + `"}`}}}
			case <-ctx.Done():
				out <- llm.StreamEvent{Err: ctx.Err()}
			}
		case 3:
			out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "select-correct", Function: "work_select", Args: `{"action":"resume","run_id":"` + p.correctRunID + `"}`}}}
		default:
			out <- llm.StreamEvent{Content: "I corrected the historical selection before continuing."}
		}
	}()
	return out, nil
}

func (p *workSelectionProvider) release() {
	p.releaseOnce.Do(func() { close(p.releaseChild) })
}

func (p *workSelectionProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "done", nil
}
func (p *workSelectionProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}
func (p *workSelectionProvider) StreamChat(ctx context.Context, _ llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	out := make(chan llm.StreamEvent, 1)
	go func() {
		defer close(out)
		switch call {
		case 1:
			action := p.action
			if action == "" {
				action = "resume"
			}
			out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "select", Function: "work_select", Args: `{"action":"` + action + `","run_id":"` + p.targetRunID + `"}`}}}
		case 2:
			out <- llm.StreamEvent{Content: "I found the exact historical run and queued its continuation."}
		default:
			p.once.Do(func() { close(p.childStarted) })
			select {
			case <-p.releaseChild:
				out <- llm.StreamEvent{Content: "Historical work continued."}
			case <-ctx.Done():
				out <- llm.StreamEvent{Err: ctx.Err()}
			}
		}
	}()
	return out, nil
}

func TestNaturalProgressQuestionUsesReferenceInteractionWithoutClaimingTheRun(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wx-local", "Local on Weixin"); err != nil {
		t.Fatal(err)
	}
	targetTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "RUQX-767 production release", Channel: "cli"})
	targetRun, _ := store.StartRun(ctx, targetTask, "cli", "release RUQX-767")
	_ = store.FinishRun(ctx, identity.TenantID, targetRun.ID, "interrupted")
	provider := &workSelectionProvider{targetRunID: targetRun.ID, action: "observe", childStarted: make(chan struct{}), releaseChild: make(chan struct{})}
	registry := tools.NewRegistry()
	dispatcher := tools.NewDispatcherWithRegistry(registry)
	dispatcher.RegisterTool(tools.NewWorkSelectTool(store))
	dispatcher.RegisterTool(tools.NewWorkInspectTool(store))
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test agent", 5, 1, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default"}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx-local", Channel: "weixin", Content: "刚才 RUQX-767 的发布任务进展怎么样？",
	})
	if status != 200 || resp.Run == nil || resp.Task == nil {
		t.Fatalf("observe response: status=%d resp=%+v", status, resp)
	}
	interaction, _ := store.GetTask(ctx, identity.TenantID, resp.Task.ID)
	if interaction == nil || interaction.Kind != "interaction" || interaction.Visibility != "hidden" {
		t.Fatalf("observe interaction = %+v", interaction)
	}
	if interaction.ID == targetTask.ID || resp.Run.ParentRunID != "" {
		t.Fatalf("natural progress question claimed the observed run: task=%s parent=%s", interaction.ID, resp.Run.ParentRunID)
	}
	queued, _ := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if len(queued) != 0 {
		t.Fatalf("observe unexpectedly queued work: %+v", queued)
	}
}

func TestIdleMainSelectionCommitsThroughNormalRunAndHidesInteractionLabel(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	targetTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "old release", Channel: "cli"})
	targetRun, _ := store.StartRunWithOptions(ctx, targetTask, "cli", "old release", control.StartRunOptions{ExecutionRoots: []executionenv.RootBinding{{
		Path: "/parent-workspace", Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceWorkspace,
	}}})
	_ = store.FinishRun(ctx, identity.TenantID, targetRun.ID, "interrupted")
	provider := &workSelectionProvider{targetRunID: targetRun.ID, childStarted: make(chan struct{}), releaseChild: make(chan struct{})}
	defer provider.release()
	registry := tools.NewRegistry()
	dispatcher := tools.NewDispatcherWithRegistry(registry)
	dispatcher.RegisterTool(tools.NewWorkSelectTool(store))
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test agent", 5, 1, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default"}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "Please revisit the old release and take it forward",
	})
	if status != 200 || resp.Run == nil || resp.Task == nil {
		t.Fatalf("interaction response: status=%d resp=%+v", status, resp)
	}
	select {
	case <-provider.childStarted:
	case <-time.After(5 * time.Second):
		queued, _ := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
		started, _ := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusStarted)
		events, _ := store.ListRunEvents(ctx, identity.TenantID, identity.PersonID, resp.Task.ID, resp.Run.ID, 20)
		provider.mu.Lock()
		calls := provider.calls
		provider.mu.Unlock()
		t.Fatalf("selected continuation did not start: resp=%+v queued=%+v started=%+v events=%+v calls=%d", resp, queued, started, events, calls)
	}
	key := "work-selection:" + resp.Run.ID + ":" + targetRun.ID
	queued, err := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, key)
	if err != nil || queued == nil || queued.ReplyToRunID != targetRun.ID {
		t.Fatalf("selected queue=%+v err=%v", queued, err)
	}
	interactionTask, _ := store.GetTask(ctx, identity.TenantID, resp.Task.ID)
	if interactionTask == nil || interactionTask.Kind != "interaction" || interactionTask.Visibility != "hidden" {
		t.Fatalf("interaction projection = %+v", interactionTask)
	}
	provider.release()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if daemon.coordinator().currentActive(identity.PersonID) == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("selected continuation stayed active after release")
}

func TestIdleMainSelectionDirectlyContinuesSameExecutionDomain(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	targetTask, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "old release", Channel: "cli"})
	targetRun, _ := store.StartRun(ctx, targetTask, "cli", "old release")
	_ = store.FinishRun(ctx, identity.TenantID, targetRun.ID, "interrupted")
	provider := &workSelectionProvider{targetRunID: targetRun.ID, childStarted: make(chan struct{}), releaseChild: make(chan struct{})}
	defer provider.release()
	registry := tools.NewRegistry()
	dispatcher := tools.NewDispatcherWithRegistry(registry)
	dispatcher.RegisterTool(tools.NewWorkSelectTool(store))
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test agent", 5, 1, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default"}

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "Please revisit the old release and take it forward",
	})
	if status != 200 || resp.Run == nil || resp.Task == nil {
		t.Fatalf("interaction response: status=%d resp=%+v", status, resp)
	}
	if resp.Task.ID != targetTask.ID || resp.Run.TaskID != targetTask.ID || resp.Run.ParentRunID != targetRun.ID {
		t.Fatalf("direct continuation response = task:%+v run:%+v", resp.Task, resp.Run)
	}
	queued, _ := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if len(queued) != 0 {
		t.Fatalf("same-domain continuation unexpectedly queued: %+v", queued)
	}
	provider.mu.Lock()
	calls := provider.calls
	provider.mu.Unlock()
	if calls != 2 {
		t.Fatalf("same-domain continuation used %d model calls, want one normal tool round-trip", calls)
	}
}

func TestLiveCorrectionReplacesImplicitSelectionBeforeEffects(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	makeTarget := func(title string) (*control.Task, *control.Run) {
		task, _ := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title, Channel: "cli"})
		run, _ := store.StartRun(ctx, task, "cli", title)
		_ = store.FinishRun(ctx, identity.TenantID, run.ID, "interrupted")
		return task, run
	}
	_, wrong := makeTarget("wrong release")
	correctTask, correct := makeTarget("correct release")
	provider := &correctingWorkSelectionProvider{
		wrongRunID: wrong.ID, correctRunID: correct.ID,
		firstSelection: make(chan struct{}), release: make(chan struct{}),
	}
	registry := tools.NewRegistry()
	dispatcher := tools.NewDispatcherWithRegistry(registry)
	dispatcher.RegisterTool(tools.NewWorkSelectTool(store))
	dispatcher.RegisterTool(tools.NewWorkInspectTool(store))
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test agent", 8, 1, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default"}
	type result struct {
		resp   api.MessageResponse
		status int
	}
	finished := make(chan result, 1)
	go func() {
		resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
			Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "continue the release",
		})
		finished <- result{resp: resp, status: status}
	}()
	select {
	case <-provider.firstSelection:
	case <-time.After(5 * time.Second):
		t.Fatal("first implicit selection was not made")
	}
	steered, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "Not that one; continue the correct release instead.",
	})
	if status != 200 || !steered.Accepted {
		t.Fatalf("correction steer: status=%d resp=%+v", status, steered)
	}
	close(provider.release)
	select {
	case got := <-finished:
		if got.status != 200 || got.resp.Run == nil || got.resp.Task == nil {
			t.Fatalf("corrected response: status=%d resp=%+v", got.status, got.resp)
		}
		if got.resp.Task.ID != correctTask.ID || got.resp.Run.ParentRunID != correct.ID {
			provider.mu.Lock()
			calls := provider.calls
			provider.mu.Unlock()
			events, _ := store.ListRunEvents(ctx, identity.TenantID, identity.PersonID, got.resp.Task.ID, got.resp.Run.ID, 20)
			t.Fatalf("corrected continuation = task:%+v run:%+v calls=%d events=%+v content=%q", got.resp.Task, got.resp.Run, calls, events, got.resp.Content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("corrected interaction did not finish")
	}
	wrongCandidates, _ := store.ListUnresolvedRuns(ctx, identity.TenantID, identity.PersonID, wrong.TaskID, 10)
	if len(wrongCandidates) != 1 || wrongCandidates[0].ID != wrong.ID {
		t.Fatalf("wrong parent was consumed by corrected selection: %+v", wrongCandidates)
	}
}
