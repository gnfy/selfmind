package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/kernel/task/cron"
)

func TestConcurrentIMInputNeedsExactChoiceBeforeSteering(t *testing.T) {
	provider := newSlowLLMProvider("unused")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, "weixin", "wx-local", "Local on IM"); err != nil {
		t.Fatal(err)
	}
	coord := daemon.coordinator()
	coord.activeLimit = 2
	var active []*activeRun
	for i, session := range []string{"session-a", "session-b"} {
		task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "Work " + session, Channel: session})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, session, "work")
		if err != nil {
			t.Fatal(err)
		}
		handle := &activeRun{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, Channel: session, Steer: make(chan kernel.SteeringInput, 1), StartedAt: run.StartedAt.Add(time.Duration(i) * time.Second)}
		if !coord.beginActive(owner.PersonID, handle) {
			t.Fatal("cannot register active run")
		}
		active = append(active, handle)
		defer coord.endActiveRun(owner.PersonID, handle)
	}
	if _, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, "telegram", "tg-local", "Local on Telegram"); err != nil {
		t.Fatal(err)
	}
	linked, err := store.EnqueueDelivery(ctx, control.Delivery{
		TenantID: owner.TenantID, PersonID: owner.PersonID, Platform: "telegram", PlatformUserID: "tg-local",
		Channel: "chat-one", TaskID: active[1].TaskID, RunID: active[1].RunID, Content: "Work B is running",
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimDelivery(ctx, linked.ID); err != nil || !claimed {
		t.Fatalf("claim outbound reply source: %v, %v", claimed, err)
	}
	if err := store.MarkDeliverySentWithNativeID(ctx, linked.ID, "501"); err != nil {
		t.Fatal(err)
	}
	linkedReply, code := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "telegram", PlatformUserID: "tg-local", Channel: "chat-one",
		Content: "add a test to B", NativeReplyMessageID: "501", Async: true,
	})
	if code != 200 || !linkedReply.Accepted || len(active[0].Steer) != 0 || len(active[1].Steer) != 1 {
		t.Fatalf("native reply did not target B alone: code=%d response=%+v", code, linkedReply)
	}
	<-active[1].Steer
	input := api.MessageRequest{Platform: "weixin", PlatformUserID: "wx-local", Channel: "chat-one", Content: "please add the missing check", Async: true}
	response, code := daemon.ProcessMessage(ctx, input)
	if code != 200 || response.Choice == nil || len(response.Choice.Options) != 3 || !strings.Contains(response.Content, "For status only, use /status <run_id>") {
		t.Fatalf("ambiguous input was not durably parked: code=%d response=%+v", code, response)
	}
	if len(active[0].Steer) != 0 || len(active[1].Steer) != 0 {
		t.Fatal("an unbound IM message reached a Run before the choice")
	}
	input.Content = "/choose " + response.Choice.ID + " 1"
	selected, code := daemon.ProcessMessage(ctx, input)
	if code != 200 || !selected.Accepted || selected.Turn == nil || selected.Turn.RunID != active[0].RunID {
		t.Fatalf("exact choice did not reach its run: code=%d response=%+v", code, selected)
	}
	if len(active[0].Steer) != 1 || len(active[1].Steer) != 0 {
		t.Fatal("choice sent guidance to the wrong run")
	}
	repeated, code := daemon.ProcessMessage(ctx, input)
	if code != 200 || !repeated.Accepted || len(active[0].Steer) != 1 {
		t.Fatalf("repeated answer duplicated or lost steering: code=%d response=%+v", code, repeated)
	}
	input.Content = "one more requirement for A"
	secondChoice, code := daemon.ProcessMessage(ctx, input)
	if code != 200 || secondChoice.Choice == nil {
		t.Fatalf("second ambiguous input was not saved: code=%d response=%+v", code, secondChoice)
	}
	if err := store.FinishRun(ctx, owner.TenantID, active[0].RunID, "done"); err != nil {
		t.Fatal(err)
	}
	coord.endActiveRun(owner.PersonID, active[0])
	input.Content = "/choose " + secondChoice.Choice.ID + " 1"
	late, code := daemon.ProcessMessage(ctx, input)
	if code != 200 || !late.Accepted || late.Turn == nil || late.Turn.QueueID == "" {
		t.Fatalf("late guidance was dropped: code=%d response=%+v", code, late)
	}
	saved, err := store.GetQueued(ctx, owner.TenantID, late.Turn.QueueID)
	if err != nil || saved == nil || saved.Content != "one more requirement for A" || saved.TaskID != active[0].TaskID || saved.ReplyToRunID != "" {
		t.Fatalf("late guidance lost its exact work grouping: queue=%+v err=%v", saved, err)
	}
	repeated, code = daemon.ProcessMessage(ctx, input)
	if code != 200 || !repeated.Accepted || repeated.Turn == nil || repeated.Turn.QueueID != late.Turn.QueueID {
		t.Fatalf("repeated late answer created different queue work: code=%d response=%+v", code, repeated)
	}
}

type coordinationProvider struct {
	*slowLLMProvider
	answer string
	err    error
	called int
	seen   llm.ChatRequest
	runID  string
}

func (p *coordinationProvider) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	p.called++
	p.seen = req
	p.runID = llm.ModelContextFrom(ctx).RunID
	if p.err != nil {
		return nil, p.err
	}
	return &llm.ChatResponse{Content: p.answer}, nil
}

func TestMainCoordinationRoutesOnlyValidatedChoice(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		wantRun      int
		wantQueue    bool
		wantChoice   bool
		wantObserve  bool
	}{
		{name: "supplement second run", answer: `{"choice":"2"}`, wantRun: 1},
		{name: "observe second run", answer: `{"choice":"2","action":"observe"}`, wantRun: 1, wantObserve: true},
		{name: "independent work", answer: `{"choice":"3"}`, wantQueue: true},
		{name: "ambiguous", answer: `{"choice":"ask"}`, wantChoice: true},
		{name: "cannot observe new work", answer: `{"choice":"3","action":"observe"}`, wantChoice: true},
		{name: "invented target", answer: `{"choice":"run_foreign"}`, wantChoice: true},
		{name: "malformed", answer: `{"choice":"1"} afterthought`, wantChoice: true},
		{name: "provider unavailable", wantChoice: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			daemon, store, _ := newDetachedRunServer(t, newSlowLLMProvider("unused"))
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, "weixin", "wx-local", "Local on IM"); err != nil {
				t.Fatal(err)
			}
			coord := daemon.coordinator()
			coord.activeLimit = 2
			active := make([]*activeRun, 0, 2)
			for i, title := range []string{"release A", "database B"} {
				task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: title, Channel: title})
				if err != nil {
					t.Fatal(err)
				}
				run, err := store.StartRun(ctx, task, title, title)
				if err != nil {
					t.Fatal(err)
				}
				handle := &activeRun{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, Channel: title, Summary: title, Steer: make(chan kernel.SteeringInput, 1), StartedAt: run.StartedAt.Add(time.Duration(i) * time.Second)}
				if !coord.beginActive(owner.PersonID, handle) {
					t.Fatal("cannot register active run")
				}
				active = append(active, handle)
				defer coord.endActiveRun(owner.PersonID, handle)
			}
			provider := &coordinationProvider{slowLLMProvider: newSlowLLMProvider("unused"), answer: tc.answer}
			if tc.name == "provider unavailable" {
				provider.err = context.DeadlineExceeded
			}
			daemon.MainRoutingProvider = provider
			response, code := daemon.ProcessMessage(ctx, api.MessageRequest{
				Platform: "weixin", PlatformUserID: "wx-local", Channel: "chat-one",
				Content: "please check database B again", Async: true,
			})
			if code != 200 || provider.called != 1 || provider.runID == "" || len(provider.seen.Tools) != 0 ||
				!strings.Contains(provider.seen.SystemPrompt, active[1].RunID) {
				t.Fatalf("Main routing boundary: code=%d response=%+v called=%d run=%q request=%+v", code, response, provider.called, provider.runID, provider.seen)
			}
			coordRun, err := store.GetRun(ctx, owner.TenantID, provider.runID)
			wantStatus := "done"
			if tc.name == "invented target" || tc.name == "malformed" || tc.name == "provider unavailable" || tc.name == "cannot observe new work" {
				wantStatus = "failed"
			}
			if err != nil || coordRun == nil || coordRun.ExecutionClass != "coordination" || coordRun.Status != wantStatus {
				t.Fatalf("coordination Run was not durably closed: %+v, %v", coordRun, err)
			}
			if tc.wantRun >= 0 && !tc.wantQueue && !tc.wantChoice {
				wantSteer := 1
				if tc.wantObserve {
					wantSteer = 0
				}
				if !response.Accepted || response.Turn == nil || response.Turn.RunID != active[tc.wantRun].RunID || len(active[tc.wantRun].Steer) != wantSteer {
					t.Fatalf("Main choice did not preserve observation/routing semantics: %+v", response)
				}
				if tc.wantObserve && (!strings.Contains(response.Content, "database B") || response.Turn.Status != "done") {
					t.Fatalf("observation did not render exact status: %+v", response)
				}
			} else if tc.wantQueue {
				if !response.Accepted || response.Turn == nil || response.Turn.QueueID == "" || len(active[0].Steer)+len(active[1].Steer) != 0 {
					t.Fatalf("new work did not queue independently: %+v", response)
				}
			} else if !tc.wantChoice || response.Choice == nil || len(active[0].Steer)+len(active[1].Steer) != 0 {
				t.Fatalf("uncertain choice was not preserved: %+v", response)
			} else if pending, err := store.PeekPendingTurnChoice(ctx, owner.TenantID, owner.PersonID,
				response.Choice.ID, time.Now(), turnChoiceBareWindow); err != nil || pending == nil || !strings.Contains(pending.RequestJSON, "please check database B again") {
				t.Fatalf("fallback lost the original input: %+v, %v", pending, err)
			}
		})
	}
}

func TestMainCoordinationCanQueueExactHistoricalResume(t *testing.T) {
	ctx := context.Background()
	daemon, store, _ := newDetachedRunServer(t, newSlowLLMProvider("unused"))
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, "weixin", "wx-local", "Local on IM"); err != nil {
		t.Fatal(err)
	}
	oldTask, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "Old release", Channel: "cli-old"})
	if err != nil {
		t.Fatal(err)
	}
	oldRun, err := store.StartRun(ctx, oldTask, "cli-old", "finish old release")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, oldRun.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	coord := daemon.coordinator()
	coord.activeLimit = 2
	for _, title := range []string{"current A", "current B"} {
		task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: title, Channel: title})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.StartRun(ctx, task, title, title)
		if err != nil {
			t.Fatal(err)
		}
		handle := &activeRun{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID,
			Channel: title, Summary: title, Steer: make(chan kernel.SteeringInput, 1)}
		if !coord.beginActive(owner.PersonID, handle) {
			t.Fatal("cannot register active run")
		}
		defer coord.endActiveRun(owner.PersonID, handle)
	}
	provider := &coordinationProvider{slowLLMProvider: newSlowLLMProvider("unused"), answer: `{"choice":"3","action":"route"}`}
	daemon.MainRoutingProvider = provider
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "weixin", PlatformUserID: "wx-local",
		Channel: "one-chat", Content: "continue the old release and finish its verification", Async: true})
	if code != 200 || !response.Accepted || response.Turn == nil || response.Turn.QueueID == "" ||
		!strings.Contains(provider.seen.SystemPrompt, oldRun.ID) {
		t.Fatalf("historical Main choice: code=%d response=%+v prompt=%q", code, response, provider.seen.SystemPrompt)
	}
	queued, err := store.GetQueued(ctx, owner.TenantID, response.Turn.QueueID)
	if err != nil || queued == nil || queued.ReplyToRunID != oldRun.ID || queued.TaskID != oldTask.ID {
		t.Fatalf("historical input lost exact parent: %+v, %v", queued, err)
	}
}

func TestExactStatusReadsParkedRunWithoutSelectingAnotherWork(t *testing.T) {
	ctx := context.Background()
	daemon, store, _ := newDetachedRunServer(t, newSlowLLMProvider("unused"))
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "Parked release", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "finish the parked release")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	card, err := daemon.statusRunReply(ctx, owner, run.ID)
	if err != nil || !strings.Contains(card, "Parked release · waiting_user") || !strings.Contains(card, shortRunID(run.ID)) {
		t.Fatalf("parked exact status: %q, %v", card, err)
	}
	shortCard, err := daemon.statusRunReply(ctx, owner, shortRunID(run.ID))
	if err != nil || shortCard != card {
		t.Fatalf("short parked Run ID did not resolve uniquely: %q, %v", shortCard, err)
	}
	stranger, err := store.ResolveOrCreateAccount(ctx, "default", "weixin", "stranger", "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := daemon.statusRunReply(ctx, stranger, run.ID)
	if err != nil || strings.Contains(foreign, "Parked release") {
		t.Fatalf("foreign exact status leaked: %q, %v", foreign, err)
	}
}

type countedSlowProvider struct {
	*slowLLMProvider
	startedCalls chan struct{}
}

func (p *countedSlowProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	out, err := p.slowLLMProvider.StreamChat(ctx, req)
	p.startedCalls <- struct{}{}
	return out, err
}

func TestConcurrentIndependentSessionsExecuteWithoutCrossRunSelection(t *testing.T) {
	provider := &countedSlowProvider{slowLLMProvider: newSlowLLMProvider("done"), startedCalls: make(chan struct{}, 4)}
	daemon, store, _ := newDetachedRunServer(t, provider.slowLLMProvider)
	first := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test", 1, 1, nil)
	second := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test", 1, 1, nil)
	daemon.Gateway = router.NewGateway(first, nil)
	daemon.Gateway.EnableWorkerPool([]*kernel.Agent{second})
	daemon.coordinator().activeLimit = 2
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"session-a", "session-b"} {
		root := filepath.Join(t.TempDir(), session)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		workspace, err := store.EnsureWorkspace(ctx, control.Workspace{TenantID: identity.TenantID,
			OwnerPersonID: identity.PersonID, Name: session, LocalPath: root})
		if err != nil {
			t.Fatal(err)
		}
		resp, code := daemon.ProcessMessage(ctx, api.MessageRequest{
			Platform: "cli", PlatformUserID: "local", Channel: session,
			Content: "answer from " + session, Async: true, WorkspaceID: workspace.ID,
		})
		if code != 200 || !resp.Accepted {
			t.Fatalf("%s admission: code=%d response=%+v", session, code, resp)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-provider.startedCalls:
		case <-time.After(5 * time.Second):
			t.Fatal("two independent sessions did not overlap at the provider")
		}
	}
	active := daemon.coordinator().activeRunsForPerson(identity.PersonID)
	if len(active) != 2 || active[0].RunID == active[1].RunID || daemon.coordinator().currentActive(identity.PersonID) != nil {
		t.Fatalf("multi-run registry selected an ambiguous run: %+v", active)
	}
	status, err := daemon.statusReply(ctx, identity)
	if err != nil || !strings.Contains(status, "2 runs active") {
		t.Fatalf("multi-run status=%q err=%v", status, err)
	}
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool { return daemon.coordinator().activeCount(identity.PersonID) == 0 }, "both runs did not finish")
}

func TestTwoCLISessionsAndOneIMWorkRunStayDistinct(t *testing.T) {
	provider := &countedSlowProvider{slowLLMProvider: newSlowLLMProvider("done"), startedCalls: make(chan struct{}, 4)}
	defer provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider.slowLLMProvider)
	agents := make([]*kernel.Agent, 3)
	for i := range agents {
		agents[i] = kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test", 1, 1, nil)
	}
	daemon.Gateway = router.NewGateway(agents[0], nil)
	daemon.Gateway.EnableWorkerPool(agents[1:])
	daemon.coordinator().activeLimit = 3
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wx-local", "Local on IM"); err != nil {
		t.Fatal(err)
	}
	workspaceIDs := make(map[string]string)
	for _, name := range []string{"A", "B", "C"} {
		root := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		workspace, err := store.EnsureWorkspace(ctx, control.Workspace{
			TenantID: identity.TenantID, OwnerPersonID: identity.PersonID, Name: name, LocalPath: root,
		})
		if err != nil {
			t.Fatal(err)
		}
		workspaceIDs[name] = workspace.ID
	}
	if err := store.SetCurrentWorkspace(ctx, identity.TenantID, identity.PersonID, workspaceIDs["C"]); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"A", "B"} {
		response, code := daemon.ProcessMessage(ctx, api.MessageRequest{
			Platform: "cli", PlatformUserID: "local", Channel: "session-" + name,
			Content: "work " + name, WorkspaceID: workspaceIDs[name], Async: true,
		})
		if code != 200 || !response.Accepted {
			t.Fatalf("CLI %s admission: code=%d response=%+v", name, code, response)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-provider.startedCalls:
		case <-time.After(5 * time.Second):
			t.Fatal("both CLI sessions did not enter the model")
		}
	}
	before := daemon.coordinator().activeRunsForPerson(identity.PersonID)
	if len(before) != 2 || before[0].RunID == before[1].RunID {
		t.Fatalf("CLI work was not independent: %+v", before)
	}
	coordination := &coordinationProvider{slowLLMProvider: provider.slowLLMProvider, answer: `{"choice":"3"}`}
	daemon.MainRoutingProvider = coordination
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx-local", Channel: "one-chat",
		Content: "Start independent work C", Async: true,
	})
	if code != 200 || !response.Accepted || response.Turn == nil || response.Turn.QueueID == "" {
		t.Fatalf("IM work C was not durably routed: code=%d response=%+v", code, response)
	}
	select {
	case <-provider.startedCalls:
	case <-time.After(5 * time.Second):
		t.Fatal("independent IM work did not overlap both CLI runs")
	}
	active := daemon.coordinator().activeRunsForPerson(identity.PersonID)
	if len(active) != 3 || active[2].RunID == before[0].RunID || active[2].RunID == before[1].RunID {
		t.Fatalf("three work runs were not distinct: %+v", active)
	}
	status, statusCode := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx-local", Channel: "one-chat", Content: "/status",
	})
	if statusCode != 200 || !strings.Contains(status.Content, "3 runs active") {
		t.Fatalf("IM did not see all three works: code=%d response=%+v", statusCode, status)
	}
	coordination.answer = `{"choice":"1","action":"route"}`
	update, updateCode := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx-local", Channel: "one-chat",
		Content: "Add this requirement to work A", Async: true,
	})
	if updateCode != 200 || !update.Accepted || update.Turn == nil || update.Turn.RunID != before[0].RunID {
		t.Fatalf("IM update did not target the original CLI run: code=%d response=%+v", updateCode, update)
	}
	if len(daemon.coordinator().activeForRun(identity.PersonID, before[0].RunID).Steer) != 1 ||
		len(daemon.coordinator().activeForRun(identity.PersonID, before[1].RunID).Steer) != 0 ||
		len(daemon.coordinator().activeForRun(identity.PersonID, active[2].RunID).Steer) != 0 {
		t.Fatal("IM update crossed a run boundary")
	}
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool { return daemon.coordinator().activeCount(identity.PersonID) == 0 }, "three runs did not finalize")
}

func TestCLIIMAndCronRunsUseThreeIndependentSlots(t *testing.T) {
	provider := &countedSlowProvider{slowLLMProvider: newSlowLLMProvider("done"), startedCalls: make(chan struct{}, 4)}
	defer provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider.slowLLMProvider)
	agents := make([]*kernel.Agent, 3)
	for i := range agents {
		agents[i] = kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test", 1, 1, nil)
	}
	daemon.Gateway = router.NewGateway(agents[0], nil)
	daemon.Gateway.EnableWorkerPool(agents[1:])
	daemon.coordinator().activeLimit = 3
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wx-local", "IM"); err != nil {
		t.Fatal(err)
	}
	workspaceIDs := map[string]string{}
	workspaceRoots := map[string]string{}
	for _, name := range []string{"cli", "im", "cron", "watch"} {
		root := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		workspace, err := store.EnsureWorkspace(ctx, control.Workspace{TenantID: identity.TenantID,
			OwnerPersonID: identity.PersonID, Name: name, LocalPath: root})
		if err != nil {
			t.Fatal(err)
		}
		workspaceIDs[name] = workspace.ID
		workspaceRoots[name] = root
	}
	if err := store.SetCurrentWorkspace(ctx, identity.TenantID, identity.PersonID, workspaceIDs["cron"]); err != nil {
		t.Fatal(err)
	}
	watchTask, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID,
		PersonID: identity.PersonID, WorkspaceID: workspaceIDs["watch"], Title: "watch closure", Channel: "watch-channel"})
	if err != nil {
		t.Fatal(err)
	}
	watchRun, err := store.StartRunWithOptions(ctx, watchTask, "watch-channel", "wait for a result", control.StartRunOptions{
		MaxActiveRuns: 3, ExecutionRoots: []executionenv.RootBinding{{Path: workspaceRoots["watch"],
			Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite,
			Source: executionenv.RootSourceCLIAddDir}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, watchRun.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	watch, err := store.CreateExternalWatch(ctx, control.ExternalWatch{TenantID: identity.TenantID,
		PersonID: identity.PersonID, WorkspaceID: workspaceIDs["watch"], TaskID: watchTask.ID,
		RunID: watchRun.ID, Channel: "watch-channel", CWD: workspaceRoots["watch"], Command: "printf READY",
		SuccessPattern: "READY", IntervalSeconds: 5, CommandTimeoutSeconds: 10,
		TimeoutAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	im, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "weixin", PlatformUserID: "wx-local",
		Channel: "one-chat", WorkspaceID: workspaceIDs["im"], Content: "Independent IM work", Async: true})
	if code != 200 || !im.Accepted {
		t.Fatalf("IM admission: %d %+v", code, im)
	}
	select {
	case <-provider.startedCalls:
	case <-time.After(5 * time.Second):
		t.Fatal("IM did not enter model")
	}
	cli, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "cli-window", WorkspaceID: workspaceIDs["cli"], Content: "CLI work", Async: true})
	if code != 200 || !cli.Accepted {
		t.Fatalf("CLI admission: %d %+v", code, cli)
	}
	select {
	case <-provider.startedCalls:
	case <-time.After(5 * time.Second):
		t.Fatal("CLI did not overlap IM")
	}
	cronDone := make(chan error, 1)
	go func() {
		cronDone <- NewCronExecutor(daemon, nil).RunCronJob(ctx, cron.CronJob{
			TenantID: identity.TenantID, Platform: "weixin", DeliverTo: "wx-local", Channel: "cron-notice",
			Prompt: "Independent scheduled work"})
	}()
	select {
	case <-provider.startedCalls:
	case <-time.After(5 * time.Second):
		t.Fatal("cron did not overlap foreground work")
	}
	active := daemon.coordinator().activeRunsForPerson(identity.PersonID)
	if len(active) != 3 {
		t.Fatalf("mixed sources did not occupy three Run slots: %+v", active)
	}
	ids := map[string]bool{}
	for _, run := range active {
		ids[run.RunID] = true
	}
	if len(ids) != 3 {
		t.Fatalf("mixed sources shared a Run: %+v", active)
	}
	wantWorkspace := map[string]string{"cli-window": workspaceIDs["cli"], "one-chat": workspaceIDs["im"],
		"cron-notice": workspaceIDs["cron"]}
	for _, item := range active {
		run, err := store.GetRun(ctx, identity.TenantID, item.RunID)
		if err != nil || run == nil {
			t.Fatalf("active Run missing: %+v %v", item, err)
		}
		if run.WorkspaceID != wantWorkspace[run.Channel] {
			t.Fatalf("Run %s channel %s used workspace %s, want %s", run.ID, run.Channel, run.WorkspaceID, wantWorkspace[run.Channel])
		}
	}
	// The watcher can finish while CLI, IM, and cron occupy all three work
	// slots. Its exact-parent continuation must wait without stealing or
	// replacing any of their Run identities.
	daemon.runExternalWatchPass(ctx)
	storedWatch, err := store.GetExternalWatch(ctx, identity.TenantID, watch.ID)
	if err != nil || storedWatch == nil || storedWatch.Status != control.ExternalWatchSucceeded {
		t.Fatalf("watcher while slots are full = %+v, %v", storedWatch, err)
	}
	queued, err := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, externalWatchFinalizationKey(*storedWatch))
	if err != nil || queued == nil || queued.Status != control.QueueStatusQueued || queued.ReplyToRunID != watchRun.ID {
		t.Fatalf("exact watcher finalization while slots are full = %+v, %v", queued, err)
	}
	if daemon.coordinator().activeCount(identity.PersonID) != 3 {
		t.Fatal("watcher finalization displaced an active source")
	}
	provider.releaseNow()
	select {
	case err := <-cronDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cron did not complete")
	}
	waitUntil(t, 5*time.Second, func() bool { return daemon.coordinator().activeCount(identity.PersonID) == 0 }, "mixed runs did not finalize")
	// The test server has no periodic daemon worker. Run its due-queue pass
	// explicitly after a slot opens, as the real daemon does on its next tick.
	daemon.drainQueuedWhenReady(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current, err := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, externalWatchFinalizationKey(*storedWatch))
		if err == nil && current != nil && current.Status == control.QueueStatusDone && current.RunID != "" {
			child, childErr := store.GetRun(ctx, identity.TenantID, current.RunID)
			physicalRoot, rootErr := filepath.EvalSymlinks(workspaceRoots["watch"])
			if childErr != nil || child == nil || child.ResumesRunID != watchRun.ID || child.WorkspaceID != workspaceIDs["watch"] ||
				rootErr != nil || len(child.ExecutionRoots) != 1 || child.ExecutionRoots[0].Path != physicalRoot {
				t.Fatalf("watcher child lost exact parent or workspace: %+v, childErr=%v rootErr=%v wantRoot=%s", child, childErr, rootErr, physicalRoot)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	current, queueErr := store.GetQueuedByIdempotencyKey(ctx, identity.TenantID, externalWatchFinalizationKey(*storedWatch))
	t.Fatalf("watcher finalization did not resume its exact parent: queue=%+v err=%v active=%d", current, queueErr, daemon.coordinator().activeCount(identity.PersonID))
}

func TestQueueDrainFillsAvailableConcurrentSlots(t *testing.T) {
	provider := &countedSlowProvider{slowLLMProvider: newSlowLLMProvider("done"), startedCalls: make(chan struct{}, 4)}
	defer provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider.slowLLMProvider)
	first := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test", 1, 1, nil)
	second := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test", 1, 1, nil)
	daemon.Gateway = router.NewGateway(first, nil)
	daemon.Gateway.EnableWorkerPool([]*kernel.Agent{second})
	daemon.coordinator().activeLimit = 2
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"session-a", "session-b"} {
		root := filepath.Join(t.TempDir(), channel)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		workspace, err := store.EnsureWorkspace(ctx, control.Workspace{TenantID: identity.TenantID,
			OwnerPersonID: identity.PersonID, Name: channel, LocalPath: root})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID,
			Channel: channel, Platform: "cli", PlatformUserID: "local", Content: "work from " + channel, WorkspaceID: workspace.ID,
			ExecutionRoots: []executionenv.RootBinding{{Path: root, Role: executionenv.RootRolePrimary,
				AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceCLIAddDir}}}); err != nil {
			t.Fatal(err)
		}
	}
	daemon.coordinator().drainQueue(identity)
	for i := 0; i < 2; i++ {
		select {
		case <-provider.startedCalls:
		case <-time.After(5 * time.Second):
			queued, _ := store.ListQueued(ctx, identity.TenantID, identity.PersonID, "queued")
			t.Fatalf("queue did not fill two independent slots: active=%+v queued=%+v", daemon.coordinator().activeRunsForPerson(identity.PersonID), queued)
		}
	}
	if got := daemon.coordinator().activeCount(identity.PersonID); got != 2 {
		t.Fatalf("active runs=%d, want 2", got)
	}
	if count, err := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); err != nil || count != 0 {
		t.Fatalf("still queued=%d err=%v", count, err)
	}
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool { return daemon.coordinator().activeCount(identity.PersonID) == 0 }, "drained runs did not finalize")
}

func TestQueuedCapacityRaceDoesNotDuplicateInput(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	coord := daemon.coordinator()
	coord.activeLimit = 1
	handle := &activeRun{PersonID: identity.PersonID, Channel: "session-a", RunID: "occupied"}
	if !coord.beginActive(identity.PersonID, handle) {
		t.Fatal("cannot occupy admission slot")
	}
	defer coord.endActiveRun(identity.PersonID, handle)
	response := coord.startAsyncRun(identity, api.MessageRequest{Channel: "session-b", Platform: "cli", PlatformUserID: "local",
		Content: "accepted once", QueueID: "queue-original", QueueClaimToken: "claim-original"}, router.IntentResult{})
	if response.Accepted || response.Turn == nil || response.Turn.Status != "capacity_wait" {
		t.Fatalf("capacity race response=%+v", response)
	}
	if count, err := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); err != nil || count != 0 {
		t.Fatalf("capacity race created duplicate queue row: count=%d err=%v", count, err)
	}
}

func TestQueueSelectSkipsBlockedRootWithoutPassingSourceHead(t *testing.T) {
	provider := newSlowLLMProvider("unused")
	defer provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	rootA := filepath.Join(t.TempDir(), "repo-a")
	rootB := filepath.Join(t.TempDir(), "repo-b")
	for _, root := range []string{rootA, rootB} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	binding := func(path string) []executionenv.RootBinding {
		return []executionenv.RootBinding{{Path: path, Role: executionenv.RootRolePrimary,
			AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceCLIAddDir}}
	}
	for _, entry := range []struct{ channel, content, root string }{
		{"session-b", "blocked head", rootA},
		{"session-b", "later same-source work", rootB},
		{"session-c", "independent work", rootB},
	} {
		if _, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID,
			Channel: entry.channel, Platform: "cli", Content: entry.content, ExecutionRoots: binding(entry.root)}); err != nil {
			t.Fatal(err)
		}
	}
	coord := daemon.coordinator()
	coord.activeLimit = 2
	handle := &activeRun{PersonID: identity.PersonID, TaskID: "task-a", RunID: "run-a", Channel: "session-a", ExecutionRoots: binding(rootA)}
	if !coord.beginActive(identity.PersonID, handle) {
		t.Fatal("cannot occupy repo A")
	}
	defer coord.endActiveRun(identity.PersonID, handle)
	coord.drainQueue(identity)
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("independent queued work did not start")
	}
	queued, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
	if err != nil || len(queued) != 2 || queued[0].Content != "blocked head" || queued[1].Content != "later same-source work" {
		t.Fatalf("source queue order changed: queued=%+v err=%v", queued, err)
	}
	// The same physical repo through a symlink must stay blocked as well.
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Dir(rootA), alias); err != nil {
		t.Fatal(err)
	}
	if queuedResourcesReady(control.QueuedTask{ExecutionRoots: binding(filepath.Join(alias, filepath.Base(rootA)))}, []*activeRun{handle}) {
		t.Fatal("symlink alias bypassed queue resource readiness")
	}
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool { return coord.activeCount(identity.PersonID) == 1 }, "independent run did not finalize")
}

func TestWaitingExactReplyDoesNotStarveIndependentQueue(t *testing.T) {
	provider := newSlowLLMProvider("done")
	defer provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "A", Channel: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.StartRun(ctx, task, "session-a", "A")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "session-a", "recover A", control.StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: "chat", Platform: "weixin", Content: "add requirement to A", TaskID: task.ID, ReplyToRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "repo-b")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID,
		// The same displayed channel string on another endpoint is not the
		// same FIFO source as the waiting IM reply.
		Channel: "chat", Platform: "cli", PlatformUserID: "local", Content: "independent B",
		ExecutionRoots: []executionenv.RootBinding{{Path: root, Role: executionenv.RootRolePrimary,
			AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceCLIAddDir}}})
	if err != nil {
		t.Fatal(err)
	}
	coord := daemon.coordinator()
	coord.activeLimit = 2
	rootA := filepath.Join(t.TempDir(), "repo-a")
	if err := os.MkdirAll(rootA, 0o755); err != nil {
		t.Fatal(err)
	}
	handle := &activeRun{PersonID: identity.PersonID, TaskID: task.ID, RunID: child.ID, Channel: "session-a",
		ExecutionRoots: []executionenv.RootBinding{{Path: rootA, Role: executionenv.RootRolePrimary,
			AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceCLIAddDir}}}
	if !coord.beginActive(identity.PersonID, handle) {
		t.Fatal("cannot occupy A slot")
	}
	defer coord.endActiveRun(identity.PersonID, handle)
	coord.drainQueue(identity)
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("waiting exact reply starved independent B")
	}
	row, err := store.GetQueued(ctx, identity.TenantID, waiting.ID)
	if err != nil || row == nil || row.Status != control.QueueStatusQueued || row.RunID != "" || row.Content != "add requirement to A" {
		t.Fatalf("exact A reply was consumed: row=%+v err=%v", row, err)
	}
}

func TestQueuedReplyClaimRaceRequeuesBeforeAnyModelCall(t *testing.T) {
	provider := newSlowLLMProvider("unexpected model call")
	defer provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "A", Channel: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.StartRun(ctx, task, "session-a", "A")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, parent.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	queued, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: "chat", Platform: "weixin", Content: "new requirement", TaskID: task.ID, ReplyToRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	token, claimed, err := store.ClaimQueued(ctx, identity.TenantID, queued.ID, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("queue claim: claimed=%v err=%v", claimed, err)
	}
	// Another continuation wins after the queue claim but before its Run is
	// created. The old queue claimant must not acknowledge and discard input.
	if _, err := store.StartRunWithOptions(ctx, task, "session-a", "automatic recovery", control.StartRunOptions{ResumesRunID: parent.ID}); err != nil {
		t.Fatal(err)
	}
	response, _ := daemon.coordinator().runMessage(ctx, identity, api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx", Channel: "chat", Content: "new requirement",
		TaskID: task.ID, ReplyToRunID: parent.ID, QueueID: queued.ID, QueueClaimToken: token,
	}, router.IntentResult{})
	if response.Turn == nil || response.Turn.Status != "capacity_wait" {
		t.Fatalf("claimed parent consumed input: %+v", response)
	}
	row, err := store.GetQueued(ctx, identity.TenantID, queued.ID)
	if err != nil || row == nil || row.Status != control.QueueStatusQueued || row.Content != "new requirement" || row.RunID != "" {
		t.Fatalf("accepted input lost in race: row=%+v err=%v", row, err)
	}
	select {
	case <-provider.started:
		t.Fatal("race started a model run before it knew the parent")
	default:
	}
}
