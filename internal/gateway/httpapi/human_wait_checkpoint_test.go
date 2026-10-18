package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

type humanWaitProvider struct {
	independent   atomic.Int32
	clarification bool
	// clarifyThenRead asks a question and, in the same batch, reads a file: the
	// read runs once the answer is in, with no approval to wait on.
	clarifyThenRead bool
	readFirst       bool
	doubleWrite     bool
	retainedRead    atomic.Bool
	missingIDs      bool
	followupWrite   bool
}

func (*humanWaitProvider) ChatCompletion(context.Context, []llm.Message) (string, error) {
	return "completed", nil
}
func (*humanWaitProvider) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	return &llm.ChatResponse{Content: "completed"}, nil
}
func (p *humanWaitProvider) StreamChat(_ context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	if p.followupWrite {
		return p.streamFollowupWrite(req), nil
	}
	out := make(chan llm.StreamEvent, 1)
	var user string
	for _, m := range req.Messages {
		if m.Role == "tool" {
			if p.readFirst {
				for _, prior := range req.Messages {
					if prior.ToolCallID == "completed-read" && strings.Contains(prior.Content, "before parking") && !strings.Contains(prior.Content, "after parking") {
						p.retainedRead.Store(true)
					}
				}
			}
			out <- llm.StreamEvent{Content: "Completed the exact approved operation."}
			close(out)
			return out, nil
		}
		if m.Role == "user" {
			user = m.Content
		}
	}
	if strings.Contains(user, "independent work") {
		p.independent.Add(1)
		out <- llm.StreamEvent{Content: "Independent work completed."}
	} else if p.clarifyThenRead {
		out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{
			{ID: "pending-question", Function: "clarify", Args: `{"question":"Choose the receipt format","choices":["plain","json"]}`},
			{ID: "follow-read", Function: "read_file", Args: `{"path":"input.txt"}`},
		}}
	} else if p.clarification {
		out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "pending-question", Function: "clarify", Args: `{"question":"Choose the receipt format","choices":["plain","json"]}`}}}
	} else {
		args, _ := json.Marshal(map[string]string{"path": "receipt.txt", "content": "approved effect"})
		calls := []llm.ToolCall{{ID: "pending-write", Function: "write_file", Args: string(args)}}
		if p.readFirst {
			calls = append([]llm.ToolCall{{ID: "completed-read", Function: "read_file", Args: `{"path":"input.txt"}`}}, calls...)
		}
		if p.doubleWrite {
			calls = append(calls, llm.ToolCall{ID: "pending-second", Function: "write_file", Args: `{"path":"second.txt","content":"second approved effect"}`})
		}
		out <- llm.StreamEvent{ToolCalls: calls}
	}
	close(out)
	return out, nil
}

func newHumanWaitServer(t *testing.T) (*Server, *control.Store, *humanWaitProvider, *control.IdentityContext) {
	t.Helper()
	return newHumanWaitServerWithStore(t, controltest.NewStore(t))
}

func newHumanWaitServerWithStore(t *testing.T, store *control.Store, customize ...func(*tools.Dispatcher)) (*Server, *control.Store, *humanWaitProvider, *control.IdentityContext) {
	t.Helper()
	provider := &humanWaitProvider{}
	agents := make([]*kernel.Agent, 2)
	for i := range agents {
		dispatcher := tools.NewDispatcherWithRegistry(tools.NewRegistry())
		dispatcher.RegisterTool(tools.NewWriteFileTool())
		dispatcher.RegisterTool(tools.NewReadFileTool())
		dispatcher.RegisterTool(tools.NewClarifyTool())
		dispatcher.InjectMiddleware(tools.SmartApprovalMiddleware(t.TempDir()))
		dispatcher.InjectMiddleware(tools.WorkspaceScopeMiddleware())
		// Added last, a middleware runs closest to the tool: after the approval.
		for _, add := range customize {
			add(dispatcher)
		}
		agents[i] = kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test", 4, 1, nil)
	}
	// These tests exercise the park path, so the in-place hold is off; the hold
	// has its own tests (human_wait_hold_test.go) that turn it back on.
	daemon := &Server{Control: store, Gateway: router.NewGateway(agents[0], nil), ExecutionRuntime: newTestExecutionRuntime(t), DefaultTenantID: "default", ApprovalWait: 10 * time.Second, ApprovalWaitUnattended: 10 * time.Second, HumanWaitHold: -1}
	daemon.Gateway.EnableWorkerPool(agents[1:])
	daemon.coordinator().activeLimit = 2
	owner, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		daemon.coordinator().stopAllActive("test cleanup")
		waitUntil(t, 2*time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "human waiter ignored cancellation")
	})
	return daemon, store, provider, owner
}

func humanWaitWorkspace(t *testing.T, store *control.Store, owner *control.IdentityContext, name string) *control.Workspace {
	t.Helper()
	root := t.TempDir()
	ws, err := store.EnsureWorkspace(context.Background(), control.Workspace{TenantID: owner.TenantID, OwnerPersonID: owner.PersonID, Name: name, LocalPath: root})
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestTwoHumanWaitsReleaseWorkersForIndependentWork(t *testing.T) {
	daemon, store, provider, owner := newHumanWaitServer(t)
	ctx := context.Background()
	for _, name := range []string{"first", "second"} {
		ws := humanWaitWorkspace(t, store, owner, name)
		response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: name, WorkspaceID: ws.ID, Content: "Write a receipt after approval", ApprovalMode: "smart", Async: true})
		if code != 200 || !response.Accepted {
			t.Fatalf("admission: %d %+v", code, response)
		}
		waitUntil(t, time.Second, func() bool {
			pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
			for _, a := range pending {
				if a.RequestedChannel == name {
					return true
				}
			}
			return false
		}, "human ask did not become durable")
	}
	waitUntil(t, 1500*time.Millisecond, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "human waits retained both workers beyond their grace")
	pending, err := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending approvals: %+v %v", pending, err)
	}
	for _, a := range pending {
		run, _ := store.GetRun(ctx, owner.TenantID, a.RunID)
		cp, _ := store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, a.RunID)
		if run == nil || run.Status != "waiting_user" || a.WaiterState != "parked" || cp == nil || cp.Outcome != "await_human" || cp.WaitRef != a.ID || cp.WaitToolCallID == "" {
			t.Fatalf("human wait lacks exact checkpoint: run=%+v approval=%+v cp=%+v", run, a, cp)
		}
		ws, _ := store.GetWorkspace(ctx, owner.TenantID, run.WorkspaceID)
		if _, err := os.Stat(filepath.Join(ws.LocalPath, "receipt.txt")); !os.IsNotExist(err) {
			t.Fatalf("unapproved side effect occurred: %v", err)
		}
	}
	ws := humanWaitWorkspace(t, store, owner, "third")
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "third", WorkspaceID: ws.ID, Content: "independent work", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("third admission: %d %+v", code, response)
	}
	waitUntil(t, time.Second, func() bool { return provider.independent.Load() == 1 }, "parked human waits kept the agent pool occupied")
}

func TestLateApprovalRestoresSuspendedToolCall(t *testing.T) {
	daemon, store, _, owner := newHumanWaitServer(t)
	ctx := context.Background()
	ws := humanWaitWorkspace(t, store, owner, "late")
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "late", WorkspaceID: ws.ID, Content: "Write a receipt after approval", ApprovalMode: "smart", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("admission: %d %+v", code, response)
	}
	var approval *control.ApprovalRequest
	waitUntil(t, 2*time.Second, func() bool {
		pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
		if len(pending) == 1 && pending[0].WaiterState == "parked" && daemon.coordinator().activeCount(owner.PersonID) == 0 {
			approval = &pending[0]
			return true
		}
		return false
	}, "approval did not park")
	t.Cleanup(func() {
		if t.Failed() {
			events, _ := store.ListTaskEvents(ctx, approval.TaskID, 100)
			for _, event := range events {
				t.Logf("%s %s", event.Type, event.Payload)
			}
		}
	})
	if _, err := daemon.respondApprovalByToken(ctx, owner, approval.ID, "approved", "late", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(ws.LocalPath, "receipt.txt"))
		return err == nil && string(data) == "approved effect"
	}, "answer did not resume the exact suspended tool call")
	pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
	if len(pending) != 0 {
		t.Fatalf("approval was asked again: %+v", pending)
	}
}

func TestClarificationParksAndConsumesItsExactAnswer(t *testing.T) {
	daemon, store, provider, owner := newHumanWaitServer(t)
	provider.clarification = true
	ctx := context.Background()
	ws := humanWaitWorkspace(t, store, owner, "question")
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "question", WorkspaceID: ws.ID, Content: "Choose the receipt format", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("admission: %d %+v", code, response)
	}
	var question *control.ClarifyRequest
	waitUntil(t, 2*time.Second, func() bool {
		pending, _ := store.ListClarifyRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
		if len(pending) == 1 && pending[0].ParkedAt != nil && daemon.coordinator().activeCount(owner.PersonID) == 0 {
			question = &pending[0]
			return true
		}
		return false
	}, "clarification retained worker instead of parking")
	if _, err := daemon.answerClarifyTarget(ctx, owner, question, "json", "question"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, func() bool {
		events, _ := store.ListTaskEvents(ctx, question.TaskID, 100)
		for _, event := range events {
			if event.Type == "run.started" && event.RunID != question.RunID {
				return true
			}
		}
		return false
	}, "late clarification answer did not resume")
	waitUntil(t, 2*time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "clarification continuation did not complete")
	pending, _ := store.ListClarifyRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
	if len(pending) != 0 {
		t.Fatalf("question was asked twice: %+v", pending)
	}
}

func waitParkedApproval(t *testing.T, daemon *Server, store *control.Store, owner *control.IdentityContext) *control.ApprovalRequest {
	t.Helper()
	var approval *control.ApprovalRequest
	// Allow SQLite's bounded writer wait under race instrumentation, while
	// still requiring parking before the fixture's ten-second human deadline.
	waitUntil(t, 5*time.Second, func() bool {
		pending, _ := store.ListApprovalRequests(context.Background(), owner.TenantID, owner.PersonID, "pending", 10)
		if len(pending) == 1 && pending[0].WaiterState == "parked" && daemon.coordinator().activeCount(owner.PersonID) == 0 {
			approval = &pending[0]
			return true
		}
		return false
	}, "approval did not park")
	return approval
}
func startHumanWait(t *testing.T, daemon *Server, ws *control.Workspace, channel string) {
	t.Helper()
	response, code := daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: channel, WorkspaceID: ws.ID, Content: "Write a receipt after approval", ApprovalMode: "smart", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("admission: %d %+v", code, response)
	}
}
func TestHumanWaitResumePreservesCompletedReadAndDenial(t *testing.T) {
	for _, decision := range []string{"approved", "rejected"} {
		t.Run(decision, func(t *testing.T) {
			daemon, store, provider, owner := newHumanWaitServer(t)
			provider.readFirst = true
			ws := humanWaitWorkspace(t, store, owner, "ledger")
			path := filepath.Join(ws.LocalPath, "input.txt")
			if err := os.WriteFile(path, []byte("before parking"), 0600); err != nil {
				t.Fatal(err)
			}
			startHumanWait(t, daemon, ws, "ledger")
			approval := waitParkedApproval(t, daemon, store, owner)
			if err := os.WriteFile(path, []byte("after parking"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := daemon.respondApprovalByToken(context.Background(), owner, approval.ID, decision, "ledger", control.ApprovalDecisionInput{}); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, 2*time.Second, func() bool { return provider.retainedRead.Load() }, "completed read/result pair was discarded or replayed")
			waitUntil(t, 2*time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "continuation did not settle")
			data, err := os.ReadFile(filepath.Join(ws.LocalPath, "receipt.txt"))
			if decision == "approved" && (err != nil || string(data) != "approved effect") {
				t.Fatalf("approved effect: %q %v", data, err)
			}
			if decision == "rejected" && !os.IsNotExist(err) {
				t.Fatalf("rejected action executed: %q %v", data, err)
			}
			pending, _ := store.ListApprovalRequests(context.Background(), owner.TenantID, owner.PersonID, "pending", 10)
			if len(pending) != 0 {
				t.Fatalf("denial/approval was asked again: %+v", pending)
			}
		})
	}
}
func TestFastApprovalStaysInItsOriginalTurn(t *testing.T) {
	daemon, store, _, owner := newHumanWaitServer(t)
	daemon.HumanWaitGrace = 2 * time.Second
	ws := humanWaitWorkspace(t, store, owner, "fast")
	startHumanWait(t, daemon, ws, "fast")
	var approval *control.ApprovalRequest
	waitUntil(t, time.Second, func() bool {
		pending, _ := store.ListApprovalRequests(context.Background(), owner.TenantID, owner.PersonID, "pending", 10)
		if len(pending) == 1 {
			approval = &pending[0]
			return true
		}
		return false
	}, "approval missing")
	if _, err := daemon.respondApprovalByToken(context.Background(), owner, approval.ID, "approved", "fast", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(ws.LocalPath, "receipt.txt"))
		return err == nil && string(data) == "approved effect"
	}, "fast answer did not execute")
	events, _ := store.ListTaskEvents(context.Background(), approval.TaskID, 100)
	for _, event := range events {
		if event.Type == "run.started" && event.RunID != approval.RunID {
			t.Fatalf("fast answer made a child run: %s", event.RunID)
		}
	}
	if q, _ := store.GetQueuedByIdempotencyKey(context.Background(), owner.TenantID, "approval-resume:"+approval.ID); q != nil {
		t.Fatalf("fast answer queued continuation: %+v", q)
	}
}
func TestHumanWaitCheckpointWriteFailureRetainsCancellableWorker(t *testing.T) {
	for _, kind := range []string{"approval", "clarification"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(`CREATE TRIGGER fail_human_checkpoint BEFORE INSERT ON loop_checkpoints WHEN NEW.outcome='await_human' BEGIN SELECT RAISE(ABORT,'injected checkpoint write failure'); END`); err != nil {
				t.Fatal(err)
			}
			daemon, _, provider, owner := newHumanWaitServerWithStore(t, store)
			provider.clarification = kind == "clarification"
			daemon.HumanWaitGrace = 25 * time.Millisecond
			daemon.ApprovalWait = 100 * time.Millisecond
			daemon.ApprovalWaitUnattended = 100 * time.Millisecond
			ws := humanWaitWorkspace(t, store, owner, "failed-checkpoint")
			startHumanWait(t, daemon, ws, "failed-checkpoint")
			var runID, taskID, waitID string
			waitUntil(t, time.Second, func() bool {
				if kind == "approval" {
					pending, _ := store.ListApprovalRequests(context.Background(), owner.TenantID, owner.PersonID, "pending", 10)
					if len(pending) == 1 {
						runID, taskID, waitID = pending[0].RunID, pending[0].TaskID, pending[0].ID
					}
				} else {
					pending, _ := store.ListClarifyRequests(context.Background(), owner.TenantID, owner.PersonID, "pending", 10)
					if len(pending) == 1 {
						runID, taskID, waitID = pending[0].RunID, pending[0].TaskID, pending[0].ID
					}
				}
				if runID == "" {
					return false
				}
				events, _ := store.ListTaskEvents(context.Background(), taskID, 100)
				for _, event := range events {
					if event.Type == "checkpoint.failed" && strings.Contains(string(event.Payload), "worker_retained") {
						return true
					}
				}
				return false
			}, "checkpoint failure was not visible")
			<-time.After(150 * time.Millisecond)
			run, _ := store.GetRun(context.Background(), owner.TenantID, runID)
			if run.Status != "running" || daemon.coordinator().activeCount(owner.PersonID) != 1 {
				t.Fatalf("failed checkpoint released worker: run=%+v", run)
			}
			if kind == "approval" {
				current, _ := store.GetApprovalRequest(context.Background(), owner.TenantID, waitID)
				if current.WaiterState != "live" {
					t.Fatalf("failed approval checkpoint became parked: %+v", current)
				}
			} else {
				current, _ := store.GetClarifyRequest(context.Background(), owner.TenantID, waitID)
				if current.ParkedAt != nil {
					t.Fatalf("failed clarification checkpoint became parked: %+v", current)
				}
			}
			daemon.coordinator().stopAllActive("cancel failed checkpoint")
			waitUntil(t, time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "failed waiter remained occupied after cancellation")
		})
	}
}

func TestNestedHumanWaitRetainsItsUnreplayableToolStack(t *testing.T) {
	for _, kind := range []string{"approval", "clarification"} {
		t.Run(kind, func(t *testing.T) {
			daemon, store, owner, task, run := newClarifyTestServer(t)
			daemon.HumanWaitGrace = 25 * time.Millisecond
			daemon.ApprovalWait = 100 * time.Millisecond
			daemon.ApprovalWaitUnattended = 100 * time.Millisecond
			parent, cancel := context.WithCancel(context.Background())
			ctx := kernel.ForkDelegationContext(kernel.WithDelegationNamespace(parent, "delegated-call"))
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				if kind == "approval" {
					_, _ = daemon.coordinator().toolApprovalHandler(owner, task, run, "cli")(ctx, tools.ToolApprovalRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, ToolName: "write_file", Args: map[string]interface{}{"path": "receipt.txt", "content": "not dispatched"}})
				} else {
					_, _ = daemon.coordinator().gatewayClarifyWait(owner, task, run, "cli")(ctx, "Which environment?", nil)
				}
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Error("nested waiter ignored cancellation")
				}
			})
			waitUntil(t, time.Second, func() bool {
				events, _ := store.ListTaskEvents(context.Background(), task.ID, 100)
				for _, event := range events {
					if event.Type == "checkpoint.failed" && strings.Contains(string(event.Payload), "worker_retained") {
						return true
					}
				}
				return false
			}, "unreplayable nested wait was not diagnosed")
			select {
			case <-finished:
				t.Fatal("nested tool stack was released without a checkpoint")
			case <-time.After(150 * time.Millisecond):
			}
			cp, _ := store.IncompleteLoopCheckpointForRun(context.Background(), owner.TenantID, run.ID)
			if cp != nil {
				t.Fatalf("nested effects became replayable: %+v", cp)
			}
		})
	}
}

type humanWaitSender struct{ messages chan delivery.Message }

func (s *humanWaitSender) Send(_ context.Context, msg delivery.Message) error {
	s.messages <- msg
	return nil
}
func TestHumanWaitNotificationWriteFailureUsesFallback(t *testing.T) {
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER fail_human_event BEFORE INSERT ON task_events WHEN NEW.type='approval.requested' BEGIN SELECT RAISE(ABORT,'injected notification write failure'); END`); err != nil {
		t.Fatal(err)
	}
	daemon, _, _, owner := newHumanWaitServerWithStore(t, store)
	if _, err := store.BindAccount(context.Background(), owner.TenantID, owner.PersonID, "weixin", "synthetic-im", "IM"); err != nil {
		t.Fatal(err)
	}
	sender := &humanWaitSender{messages: make(chan delivery.Message, 20)}
	daemon.Delivery = delivery.NewService(store, sender, delivery.Options{})
	daemon.touchPresence(context.Background(), owner)
	ws := humanWaitWorkspace(t, store, owner, "notification-failure")
	startHumanWait(t, daemon, ws, "notification-failure")
	approval := waitParkedApproval(t, daemon, store, owner)
	select {
	case msg := <-sender.messages:
		if msg.Kind != delivery.KindApproval || msg.ApprovalID != approval.ID {
			t.Fatalf("missing human fallback: %+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("both live event and push were silenced")
	}
}
func TestExplicitNewTaskDoesNotAnswerParkedClarification(t *testing.T) {
	daemon, store, provider, owner := newHumanWaitServer(t)
	provider.clarification = true
	first := humanWaitWorkspace(t, store, owner, "old-question")
	response, code := daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "one-session", WorkspaceID: first.ID, Content: "Choose the receipt format", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("admission: %d %+v", code, response)
	}
	waitUntil(t, time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "question did not park")
	second := humanWaitWorkspace(t, store, owner, "new-work")
	response, code = daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "one-session", WorkspaceID: second.ID, Content: "independent work", ForceNew: true, Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("explicit new task was consumed as answer: %d %+v", code, response)
	}
	waitUntil(t, time.Second, func() bool { return provider.independent.Load() == 1 }, "explicit new work did not run")
	pending, _ := store.ListClarifyRequests(context.Background(), owner.TenantID, owner.PersonID, "pending", 10)
	if len(pending) != 1 {
		t.Fatalf("new task answered old question: %+v", pending)
	}
}

func TestCrossEndpointHumanAnswerRestoresOriginCLIStream(t *testing.T) {
	for _, kind := range []string{"approval", "clarification"} {
		t.Run(kind, func(t *testing.T) {
			daemon, store, provider, owner := newHumanWaitServer(t)
			provider.clarification = kind == "clarification"
			ctx := context.Background()
			mobile, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, "telegram", "synthetic-mobile", "Mobile")
			if err != nil {
				t.Fatal(err)
			}
			ws := humanWaitWorkspace(t, store, owner, "origin")
			startHumanWait(t, daemon, ws, "origin-cli")
			var parentID, taskID, waitID string
			waitUntil(t, 2*time.Second, func() bool {
				if kind == "approval" {
					pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
					if len(pending) == 1 && pending[0].WaiterState == "parked" {
						parentID, taskID, waitID = pending[0].RunID, pending[0].TaskID, pending[0].ID
					}
				} else {
					pending, _ := store.ListClarifyRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
					if len(pending) == 1 && pending[0].ParkedAt != nil {
						parentID, taskID, waitID = pending[0].RunID, pending[0].TaskID, pending[0].ID
					}
				}
				return parentID != "" && daemon.coordinator().activeCount(owner.PersonID) == 0
			}, "human wait did not park")
			own, closeOwn := daemon.events().subscribe(owner.PersonID, "origin-cli", "")
			defer closeOwn()
			other, closeOther := daemon.events().subscribe(owner.PersonID, mobile.PlatformUserID, "")
			defer closeOther()
			req := api.MessageRequest{Platform: mobile.Platform, PlatformUserID: mobile.PlatformUserID, Channel: mobile.PlatformUserID, Content: "/approve " + waitID}
			if kind == "clarification" {
				req.Content = "json"
				req.ClarifyID = waitID
			}
			response, code := daemon.ProcessMessage(ctx, req)
			if code != 200 || response.Error != "" {
				t.Fatalf("cross-endpoint answer: %d %+v", code, response)
			}
			var sawDelta, sawForeground, sawFinal bool
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			for !sawFinal {
				select {
				case event := <-own.ch:
					if event.RunID == parentID {
						continue
					}
					if event.Channel != "origin-cli" {
						t.Fatalf("origin event migrated session: %+v", event)
					}
					switch event.Type {
					case "assistant.delta":
						sawDelta = true
					case "run.started":
						sawForeground = strings.Contains(string(event.Payload), `"presentation":"foreground"`)
					case "run.finished":
						sawFinal = strings.Contains(string(event.Payload), "final_answer")
					}
				case <-deadline.C:
					t.Fatalf("origin missed stream/final: delta=%v foreground=%v final=%v", sawDelta, sawForeground, sawFinal)
				}
			}
			if !sawDelta || !sawForeground {
				t.Fatalf("origin did not remain foreground: delta=%v foreground=%v", sawDelta, sawForeground)
			}
			for {
				select {
				case event := <-other.ch:
					if event.Type == "assistant.delta" || event.Type == "tool.started" || event.Type == "tool.completed" {
						t.Fatalf("answer endpoint replayed origin transcript: %+v", event)
					}
				default:
					goto inspected
				}
			}
		inspected:
			prefix := "approval-resume:"
			if kind == "clarification" {
				prefix = "clarify-resume:"
			}
			queued, err := store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, prefix+waitID)
			if err != nil || queued == nil || queued.Platform != "cli" || queued.Channel != "origin-cli" || queued.PlatformUserID != "local" || queued.ReplyToRunID != parentID || queued.WorkspaceID != ws.ID {
				t.Fatalf("continuation route/scope: %+v %v", queued, err)
			}
			events, _ := store.ListTaskEvents(ctx, taskID, 100)
			var calls int
			for _, event := range events {
				if event.Type == "run.started" && event.RunID != parentID {
					calls++
				}
			}
			if calls != 1 {
				t.Fatalf("answer did not create one exact child: %d", calls)
			}
		})
	}
}

func TestSuccessiveHumanWaitsNeverReplayEarlierEffects(t *testing.T) {
	daemon, store, provider, owner := newHumanWaitServer(t)
	provider.doubleWrite = true
	ctx := context.Background()
	ws := humanWaitWorkspace(t, store, owner, "successive")
	startHumanWait(t, daemon, ws, "successive")
	var taskID, firstID string
	for i := 0; i < 2; i++ {
		approval := waitParkedApproval(t, daemon, store, owner)
		if i == 0 {
			taskID, firstID = approval.TaskID, approval.ID
		} else if approval.ID == firstID {
			t.Fatal("first approval was requested again")
		}
		if _, err := daemon.respondApprovalByToken(ctx, owner, approval.ID, "approved", "successive", control.ApprovalDecisionInput{}); err != nil {
			t.Fatal(err)
		}
		path := "receipt.txt"
		if i == 1 {
			path = "second.txt"
		}
		waitUntil(t, 2*time.Second, func() bool { _, err := os.Stat(filepath.Join(ws.LocalPath, path)); return err == nil }, "approved call did not execute")
	}
	waitUntil(t, 2*time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "second child did not settle")
	events, _ := store.ListTaskEvents(ctx, taskID, 200)
	invoked := make(map[string]int)
	for _, event := range events {
		if event.Type == "tool.completed" {
			var result struct {
				Invoked bool   `json:"invoked"`
				CallID  string `json:"tool_call_id"`
			}
			_ = json.Unmarshal(event.Payload, &result)
			if result.Invoked {
				invoked[result.CallID]++
			}
		}
	}
	if invoked["pending-write"] != 1 || invoked["pending-second"] != 1 {
		t.Fatalf("successive waits repeated effects: %+v", invoked)
	}
}

func TestHumanWaitContinuationKeepsScopeAndCurrentNarrowing(t *testing.T) {
	for _, kind := range []string{"approval", "clarification"} {
		t.Run(kind, func(t *testing.T) {
			daemon, store, provider, owner := newHumanWaitServer(t)
			provider.clarification = kind == "clarification"
			ctx := context.Background()
			original := humanWaitWorkspace(t, store, owner, "frozen")
			different := humanWaitWorkspace(t, store, owner, "later-selection")
			startHumanWait(t, daemon, original, "scope-origin")
			var parentID, taskID, waitID string
			waitUntil(t, 2*time.Second, func() bool {
				if kind == "approval" {
					pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
					if len(pending) == 1 && pending[0].WaiterState == "parked" {
						parentID, taskID, waitID = pending[0].RunID, pending[0].TaskID, pending[0].ID
					}
				} else {
					pending, _ := store.ListClarifyRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
					if len(pending) == 1 && pending[0].ParkedAt != nil {
						parentID, taskID, waitID = pending[0].RunID, pending[0].TaskID, pending[0].ID
					}
				}
				return parentID != "" && daemon.coordinator().activeCount(owner.PersonID) == 0
			}, "human wait did not park")
			admitted, err := store.RunAdmissionSnapshot(ctx, owner.TenantID, parentID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetTaskWorkspace(ctx, owner.TenantID, taskID, different.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.SetCurrentWorkspace(ctx, owner.TenantID, owner.PersonID, different.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.SetPersonSetting(ctx, owner.TenantID, owner.PersonID, personSettingApprovalMode, "read-only"); err != nil {
				t.Fatal(err)
			}
			prefix := "approval-resume:"
			if kind == "approval" {
				if _, err := daemon.respondApprovalByToken(ctx, owner, waitID, "approved", "scope-origin", control.ApprovalDecisionInput{}); err != nil {
					t.Fatal(err)
				}
			} else {
				prefix = "clarify-resume:"
				question, _ := store.GetClarifyRequest(ctx, owner.TenantID, waitID)
				if _, err := daemon.answerClarifyTarget(ctx, owner, question, "json", "scope-origin"); err != nil {
					t.Fatal(err)
				}
			}
			var child *control.Run
			waitUntil(t, 2*time.Second, func() bool {
				events, _ := store.ListTaskEvents(ctx, taskID, 100)
				for _, event := range events {
					if event.Type == "run.started" && event.RunID != parentID {
						child, _ = store.GetRun(ctx, owner.TenantID, event.RunID)
						return child != nil
					}
				}
				return false
			}, "child did not start")
			waitUntil(t, 2*time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "child did not settle")
			row, err := store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, prefix+waitID)
			if err != nil || row == nil || row.WorkspaceID != original.ID || child.WorkspaceID != original.ID || child.ResumesRunID != parentID {
				t.Fatalf("human answer moved execution scope: child=%+v queue=%+v %v", child, row, err)
			}
			childAdmission, err := store.RunAdmissionSnapshot(ctx, owner.TenantID, child.ID)
			if err != nil || childAdmission != admitted {
				t.Fatalf("answer changed frozen admission: %+v want %+v err=%v", childAdmission, admitted, err)
			}
			events, err := store.ListTaskEvents(ctx, taskID, 100)
			if err != nil {
				t.Fatal(err)
			}
			narrowed := false
			for _, event := range events {
				if event.RunID == child.ID && event.Type == "run.authority_narrowed" && strings.Contains(string(event.Payload), "read-only") {
					narrowed = true
				}
			}
			if !narrowed {
				t.Fatal("continuation did not apply the current narrower approval mode")
			}
			// Read-only asks before writes. An explicit exact approval permits
			// this suspended action, while its execution roots stay frozen.
			_, effectErr := os.Stat(filepath.Join(original.LocalPath, "receipt.txt"))
			if kind == "approval" && effectErr != nil {
				t.Fatalf("exact approved action was not resumed in its original workspace: %v", effectErr)
			}
			if kind == "clarification" && !os.IsNotExist(effectErr) {
				t.Fatalf("answer produced an unrelated write: %v", effectErr)
			}
			if _, err := os.Stat(filepath.Join(different.LocalPath, "receipt.txt")); !os.IsNotExist(err) {
				t.Fatalf("answer used the later-selected workspace: %v", err)
			}
		})
	}
}
