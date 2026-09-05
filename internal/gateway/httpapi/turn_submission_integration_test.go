package httpapi

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
)

type steeringBoundaryTool struct{}

func (steeringBoundaryTool) Name() string        { return "steering_boundary" }
func (steeringBoundaryTool) Description() string { return "Advance the scripted integration turn." }
func (steeringBoundaryTool) Schema() tools.ToolSchema {
	return tools.ToolSchema{Type: "object"}
}
func (steeringBoundaryTool) Metadata() tools.ToolMetadata {
	return tools.ToolMetadata{Exposure: tools.ToolExposureDirect, ReadOnly: true, RiskLevel: tools.ToolRiskLow}
}
func (steeringBoundaryTool) Execute(map[string]interface{}) (string, error) {
	return `{"ok":true}`, nil
}

type independentSteeringProvider struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	once    sync.Once
	seenID  string
}

type relatedSteeringProvider struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newRelatedSteeringProvider() *relatedSteeringProvider {
	return &relatedSteeringProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *relatedSteeringProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "done", nil
}

func (p *relatedSteeringProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}

func (p *relatedSteeringProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	out := make(chan llm.StreamEvent, 1)
	go func() {
		defer close(out)
		switch call {
		case 1:
			out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{
				ID: "plan-initial", Function: "update_plan",
				Args: `{"explanation":"initial release plan","plan":[{"step":"release the service","status":"in_progress"}]}`,
			}}}
		case 2:
			p.once.Do(func() { close(p.started) })
			select {
			case <-p.release:
				out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "boundary-related", Function: "steering_boundary", Args: `{}`}}}
			case <-ctx.Done():
				out <- llm.StreamEvent{Err: ctx.Err()}
			}
		case 3:
			if liveInputID(req.Messages) == "" || !messagesContain(req.Messages, "rollback verification") {
				out <- llm.StreamEvent{Err: fmt.Errorf("related steering was not visible to Main")}
				return
			}
			out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{
				ID: "plan-revised", Function: "update_plan",
				Args: `{"explanation":"revised from live user guidance","plan":[{"step":"release the service","status":"completed"},{"step":"include rollback verification","status":"completed"}]}`,
			}}}
		default:
			out <- llm.StreamEvent{Content: "Release work completed with the requested rollback verification."}
		}
	}()
	return out, nil
}

func messagesContain(messages []llm.Message, needle string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, needle) {
			return true
		}
	}
	return false
}

func newIndependentSteeringProvider() *independentSteeringProvider {
	return &independentSteeringProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *independentSteeringProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "done", nil
}

func (p *independentSteeringProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "done"}, nil
}

func (p *independentSteeringProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	out := make(chan llm.StreamEvent, 1)
	go func() {
		defer close(out)
		switch call {
		case 1:
			p.once.Do(func() { close(p.started) })
			select {
			case <-p.release:
				out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "boundary-1", Function: "steering_boundary", Args: `{}`}}}
			case <-ctx.Done():
				out <- llm.StreamEvent{Err: ctx.Err()}
			}
		case 2:
			inputID := liveInputID(req.Messages)
			p.mu.Lock()
			p.seenID = inputID
			p.mu.Unlock()
			out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{
				ID: "queue-1", Function: "queue_user_input", Args: fmt.Sprintf(`{"input_id":%q}`, inputID),
			}}}
		default:
			out <- llm.StreamEvent{Content: "Current work remains focused; the independent request is queued."}
		}
	}()
	return out, nil
}

func liveInputID(messages []llm.Message) string {
	for _, message := range messages {
		if message.Role != "user" || !strings.Contains(message.Content, "[SelfMind live user input]") {
			continue
		}
		for _, line := range strings.Split(message.Content, "\n") {
			if strings.HasPrefix(line, "input_id: ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "input_id: "))
			}
		}
	}
	return ""
}

func TestActiveMainCanQueueIndependentSteeringWithoutChangingCurrentTask(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	provider := newIndependentSteeringProvider()
	registry := tools.NewRegistry()
	dispatcher := tools.NewDispatcherWithRegistry(registry)
	dispatcher.RegisterTool(steeringBoundaryTool{})
	dispatcher.RegisterTool(tools.NewQueueUserInputTool(store))
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test agent", 5, 1, nil)
	daemon := &Server{
		Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default",
	}
	daemon.Delivery = delivery.NewService(store, &syncedSender{}, delivery.Options{})
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wx-user", "Weixin"); err != nil {
		t.Fatal(err)
	}

	first, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "finish the release", Async: true,
	})
	if status != 200 || !first.Accepted {
		t.Fatalf("first run was not accepted: status=%d response=%+v", status, first)
	}
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("active Main did not start")
	}
	second, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx-user", Channel: "weixin",
		Content: "prepare an unrelated weekly report",
	})
	if status != 200 || !second.Accepted || second.Turn == nil || second.Turn.Status != "accepted" {
		t.Fatalf("live input was not durably accepted: status=%d response=%+v", status, second)
	}
	close(provider.release)

	completed := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusDone)
		if len(rows) == 1 && rows[0].Content == "prepare an unrelated weekly report" && rows[0].TaskID == "" {
			completed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !completed {
		queued, _ := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
		started, _ := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusStarted)
		failed, _ := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusFailed)
		tasks, _ := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 10)
		provider.mu.Lock()
		calls, seen := provider.calls, provider.seenID
		provider.mu.Unlock()
		t.Fatalf("independent input did not complete through queue: queued=%+v started=%+v failed=%+v tasks=%+v provider_calls=%d seen_id=%q", queued, started, failed, tasks, calls, seen)
	}
	provider.mu.Lock()
	seenID := provider.seenID
	provider.mu.Unlock()
	if !strings.HasPrefix(seenID, "steer_") {
		t.Fatalf("Main did not receive a server-issued live input id: %q", seenID)
	}
	tasks, err := store.ListTasks(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("independent input did not own a fresh root task: %+v", tasks)
	}
}

func TestActiveMainCanReviseTheCurrentPlanFromRelatedSteering(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	provider := newRelatedSteeringProvider()
	registry := tools.NewRegistry()
	dispatcher := tools.NewDispatcherWithRegistry(registry)
	dispatcher.RegisterTool(steeringBoundaryTool{})
	dispatcher.RegisterTool(tools.NewUpdatePlanTool())
	dispatcher.RegisterTool(tools.NewQueueUserInputTool(store))
	agent := kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test agent", 8, 1, nil)
	daemon := &Server{Control: store, Gateway: router.NewGateway(agent, nil), DefaultTenantID: "default"}
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local-related", "Local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wx-related", "Weixin"); err != nil {
		t.Fatal(err)
	}

	first, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local-related", Channel: "cli",
		Content: "release the service", Async: true,
	})
	if status != 200 || !first.Accepted {
		t.Fatalf("first run was not accepted: status=%d response=%+v", status, first)
	}
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("active Main did not reach the steering boundary")
	}
	active := daemon.coordinator().currentActive(identity.PersonID)
	if active == nil || active.RunID == "" {
		t.Fatal("accepted background turn has no active run")
	}
	runID := active.RunID
	steered, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx-related", Channel: "weixin",
		Content: "also include rollback verification",
	})
	if status != 200 || !steered.Accepted {
		t.Fatalf("related input was not accepted: status=%d response=%+v", status, steered)
	}
	close(provider.release)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, _ := store.GetRun(ctx, identity.TenantID, runID)
		if run != nil && run.Status != "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	plan, err := store.LatestRunPlan(ctx, identity.TenantID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || len(plan.Steps) != 2 || plan.Steps[1].Step != "include rollback verification" || plan.Steps[1].Status != "completed" {
		t.Fatalf("latest plan did not incorporate related guidance: %+v", plan)
	}
	for _, status := range []string{control.QueueStatusQueued, control.QueueStatusStarted, control.QueueStatusDone} {
		rows, _ := store.ListQueued(ctx, identity.TenantID, identity.PersonID, status)
		if len(rows) != 0 {
			t.Fatalf("related guidance was incorrectly queued as separate work (%s): %+v", status, rows)
		}
	}
}
