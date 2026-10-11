package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/tools"
	"selfmind/internal/tools/sandbox"
	"strings"
	"testing"
	"time"
)

func newExecutionWaitServer(t *testing.T, store *control.Store) (*Server, *humanWaitProvider, *control.IdentityContext) {
	t.Helper()
	daemon, _, provider, owner := newHumanWaitServerWithStore(t, store)
	provider.readFirst = true
	configureExecutionWaitWorkers(t, daemon, provider)
	return daemon, provider, owner
}
func configureExecutionWaitWorkers(t *testing.T, daemon *Server, provider llm.Provider) {
	t.Helper()
	agents := make([]*kernel.Agent, 2)
	for i := range agents {
		dispatcher := tools.NewDispatcherWithRegistry(tools.NewRegistry())
		dispatcher.RegisterTool(tools.NewWriteFileTool())
		dispatcher.RegisterTool(tools.NewReadFileTool())
		dispatcher.RegisterTool(tools.NewExecuteCommandTool())
		dispatcher.InjectMiddleware(tools.WorkspaceScopeMiddleware())
		dispatcher.InjectMiddleware(tools.SmartApprovalMiddleware(t.TempDir()))
		dispatcher.InjectResultMiddleware(tools.ManagedExecutionMiddleware())
		agents[i] = kernel.NewAgent(memory.NewMemoryManager(nil), dispatcher, provider, "test", 4, 1, nil)
	}
	daemon.Gateway = router.NewGateway(agents[0], nil)
	daemon.Gateway.EnableWorkerPool(agents[1:])
	daemon.ExecutionRuntime = tools.NewExecutionRuntime("execution-wait", t.TempDir(), sandbox.PrivateState{}, nil)
	daemon.ExecutionWaitGrace = 20 * time.Millisecond
	daemon.Delivery = delivery.NewService(daemon.Control, &syncedSender{}, delivery.Options{})
}
func holdExecutionLane(t *testing.T, daemon *Server, owner *control.IdentityContext) func() {
	t.Helper()
	policy := tools.CurrentExecSandboxPolicy()
	tools.SetExecSandbox(false, false, false)
	t.Cleanup(func() { tools.SetExecSandbox(policy.Enabled, policy.Required, policy.AllowNetwork) })
	root := t.TempDir()
	run := "held-host-" + filepath.Base(root)
	cleanup := tools.SetExecutionScope(run, tools.ExecutionScope{TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run, WorkspaceRoot: root, AllowedRoots: []string{root}, Runtime: daemon.ExecutionRuntime})
	t.Cleanup(cleanup)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tools.NewExecuteCommandTool().Execute(map[string]interface{}{"_context": tools.WithExecutionScopeKey(ctx, tools.ExecutionScopeKeyForRun(run)), "command": "touch started; while [ ! -f release ]; do sleep .01; done", "cwd": root, "sandbox": "host"})
		done <- err
	}()
	waitUntil(t, time.Second, func() bool { _, err := os.Stat(filepath.Join(root, "started")); return err == nil }, "host did not enter its lane")
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("host did not release its lane")
		}
	}
	t.Cleanup(release)
	return release
}
func TestExecutionLaneWaitParksExactCallAndResumesItsOriginWithSavedEvidence(t *testing.T) {
	store := controltest.NewStore(t)
	defer store.Close()
	daemon, provider, owner := newExecutionWaitServer(t, store)
	release := holdExecutionLane(t, daemon, owner)
	ws := humanWaitWorkspace(t, store, owner, "lane-work")
	if err := os.WriteFile(filepath.Join(ws.LocalPath, "input.txt"), []byte("before parking"), 0600); err != nil {
		t.Fatal(err)
	}
	response, code := daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "origin-lane", WorkspaceID: ws.ID, Content: "Write the receipt using input.txt", ApprovalMode: "full-auto", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("admission=%d %+v", code, response)
	}
	var parent *control.Run
	var queue *control.QueuedTask
	waitUntil(t, 2*time.Second, func() bool {
		rows, _ := store.ListRecentRunsForPerson(context.Background(), owner.TenantID, owner.PersonID, 5)
		if len(rows) != 1 {
			return false
		}
		parent, _ = store.GetRun(context.Background(), owner.TenantID, rows[0].RunID)
		queue, _ = store.GetQueuedByIdempotencyKey(context.Background(), owner.TenantID, "execution-lane:"+parent.ID)
		return parent.Status == "waiting_external" && queue != nil && daemon.coordinator().activeCount(owner.PersonID) == 0
	}, "lane wait did not checkpoint and release its worker")
	cp, err := store.IncompleteLoopCheckpointForRun(context.Background(), owner.TenantID, parent.ID)
	if err != nil || cp == nil || cp.Outcome != "await_execution" || cp.WaitKind != control.LoopCheckpointWaitExecutionLane || cp.WaitToolCallID != "pending-write" {
		t.Fatalf("pending boundary=%+v %v", cp, err)
	}
	if _, err := os.Stat(filepath.Join(ws.LocalPath, "receipt.txt")); !os.IsNotExist(err) {
		t.Fatalf("file effect happened before lane acquisition: %v", err)
	}
	daemon.coordinator().drainQueue(owner)
	queue, _ = store.GetQueued(context.Background(), owner.TenantID, queue.ID)
	if queue.Status != control.QueueStatusQueued {
		t.Fatalf("busy lane launched continuation: %+v", queue)
	}
	other := humanWaitWorkspace(t, store, owner, "independent")
	independent, code := daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "independent", WorkspaceID: other.ID, Content: "independent work", ApprovalMode: "full-auto"})
	if code != 200 || provider.independent.Load() != 1 || independent.Outcome == nil || independent.Outcome.Status != "done" {
		t.Fatalf("host lane blocked model work: %d %+v", code, independent)
	}
	if err := os.WriteFile(filepath.Join(ws.LocalPath, "input.txt"), []byte("after parking"), 0600); err != nil {
		t.Fatal(err)
	}
	release()
	daemon.coordinator().drainQueue(owner)
	waitUntil(t, 3*time.Second, func() bool {
		queue, _ = store.GetQueued(context.Background(), owner.TenantID, queue.ID)
		return queue.Status == control.QueueStatusDone
	}, "lane continuation did not finish")
	child, _ := store.GetRun(context.Background(), owner.TenantID, queue.RunID)
	if child == nil || child.ResumesRunID != parent.ID || child.Channel != "origin-lane" || !provider.retainedRead.Load() {
		t.Fatalf("resume drift/replayed evidence: child=%+v retained=%v", child, provider.retainedRead.Load())
	}
	parentAdmission, parentErr := store.RunAdmissionSnapshot(context.Background(), owner.TenantID, parent.ID)
	childAdmission, childErr := store.RunAdmissionSnapshot(context.Background(), owner.TenantID, child.ID)
	if parentErr != nil || childErr != nil || childAdmission != parentAdmission {
		t.Fatalf("execution authority drift: parent=%+v child=%+v %v %v", parentAdmission, childAdmission, parentErr, childErr)
	}
	payload, _ := store.RunStartedPayload(context.Background(), owner.TenantID, owner.PersonID, child.ID)
	var presentation struct {
		Presentation string `json:"presentation"`
		Origin       string `json:"origin"`
	}
	_ = json.Unmarshal(payload, &presentation)
	answer, _ := store.RunAssistantContent(context.Background(), owner.TenantID, owner.PersonID, child.ID)
	if presentation.Presentation != "foreground" || presentation.Origin != runOriginResource || !strings.Contains(answer, "Completed the exact approved operation") {
		t.Fatalf("D8 origin/final answer lost: %+v %q", presentation, answer)
	}
}
func TestExecutionLaneCheckpointFailureRetainsCancellableWorkerAndNoQueue(t *testing.T) {
	for _, failure := range []string{"checkpoint", "parking"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			defer store.Close()
			raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			trigger := `CREATE TRIGGER fail_execution_checkpoint BEFORE INSERT ON loop_checkpoints WHEN NEW.outcome='await_execution' BEGIN SELECT RAISE(ABORT,'injected checkpoint failure'); END`
			if failure == "parking" {
				trigger = `CREATE TRIGGER fail_execution_parking BEFORE INSERT ON task_queue WHEN NEW.idempotency_key LIKE 'execution-lane:%' BEGIN SELECT RAISE(ABORT,'injected parking failure'); END`
			}
			if _, err := raw.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			daemon, _, owner := newExecutionWaitServer(t, store)
			holdExecutionLane(t, daemon, owner)
			ws := humanWaitWorkspace(t, store, owner, "failure")
			_ = os.WriteFile(filepath.Join(ws.LocalPath, "input.txt"), []byte("before parking"), 0600)
			response, code := daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "lane-failure", WorkspaceID: ws.ID, Content: "Write a receipt", ApprovalMode: "full-auto", Async: true})
			if code != 200 || !response.Accepted {
				t.Fatalf("admission=%d %+v", code, response)
			}
			var runID string
			waitUntil(t, 2*time.Second, func() bool {
				rows, _ := store.ListRecentRunsForPerson(context.Background(), owner.TenantID, owner.PersonID, 5)
				if len(rows) != 1 {
					return false
				}
				runID = rows[0].RunID
				events, _ := store.ListRunEvents(context.Background(), owner.TenantID, owner.PersonID, rows[0].TaskID, runID, 100)
				for _, event := range events {
					if event.Type == "checkpoint.failed" && strings.Contains(string(event.Payload), "worker_retained") {
						return true
					}
				}
				return false
			}, "failed lane checkpoint was silent")
			run, _ := store.GetRun(context.Background(), owner.TenantID, runID)
			queue, _ := store.GetQueuedByIdempotencyKey(context.Background(), owner.TenantID, "execution-lane:"+runID)
			if run.Status != "running" || queue != nil || daemon.coordinator().activeCount(owner.PersonID) != 1 {
				t.Fatalf("failed parking released work: run=%+v queue=%+v", run, queue)
			}
			daemon.coordinator().stopAllActive("cancel failed execution wait")
			waitUntil(t, time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "retained lane waiter ignored cancellation")
		})
	}
}

func newTestExecutionRuntime(t *testing.T) *tools.ExecutionRuntime {
	t.Helper()
	return tools.NewExecutionRuntime(t.TempDir(), t.TempDir(), sandbox.PrivateState{}, nil)
}

type executionHostProvider struct{ *humanWaitProvider }

func (p *executionHostProvider) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	for _, m := range req.Messages {
		if m.Role == "tool" {
			return p.humanWaitProvider.StreamChat(ctx, req)
		}
	}
	out := make(chan llm.StreamEvent, 1)
	out <- llm.StreamEvent{ToolCalls: []llm.ToolCall{{ID: "completed-read", Function: "read_file", Args: `{"path":"input.txt"}`}, {ID: "pending-host", Function: "terminal", Args: `{"command":"printf resumed > host-receipt","sandbox":"host"}`}}}
	close(out)
	return out, nil
}

func TestHostExecutionLaneCancellationRemovesWriterIntentImmediately(t *testing.T) {
	for _, action := range []string{"stop", "drop", "clear"} {
		t.Run(action, func(t *testing.T) {
			store := controltest.NewStore(t)
			defer store.Close()
			daemon, _, owner := newExecutionWaitServer(t, store)
			configureExecutionWaitWorkers(t, daemon, &executionHostProvider{&humanWaitProvider{readFirst: true}})
			release := holdExecutionLane(t, daemon, owner)
			ws := humanWaitWorkspace(t, store, owner, "queued-host")
			if err := os.WriteFile(filepath.Join(ws.LocalPath, "input.txt"), []byte("before parking"), 0600); err != nil {
				t.Fatal(err)
			}
			response, code := daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "host-origin", WorkspaceID: ws.ID, Content: "Run the host command", ApprovalMode: "full-auto", Async: true})
			if code != 200 || !response.Accepted {
				t.Fatalf("admission=%d %+v", code, response)
			}
			var parent *control.Run
			var queue *control.QueuedTask
			waitUntil(t, 2*time.Second, func() bool {
				rows, _ := store.ListRecentRunsForPerson(context.Background(), owner.TenantID, owner.PersonID, 5)
				if len(rows) != 1 {
					return false
				}
				parent, _ = store.GetRun(context.Background(), owner.TenantID, rows[0].RunID)
				queue, _ = store.GetQueuedByIdempotencyKey(context.Background(), owner.TenantID, "execution-lane:"+parent.ID)
				return parent.Status == "waiting_external" && queue != nil && daemon.coordinator().activeCount(owner.PersonID) == 0
			}, "host did not durably park")
			cp, err := store.IncompleteLoopCheckpointForRun(context.Background(), owner.TenantID, parent.ID)
			refs, readErr := store.QueuedHostExecutionWaits(context.Background(), owner.TenantID, owner.PersonID)
			if err != nil || readErr != nil || cp == nil || cp.WaitKind != control.LoopCheckpointWaitExecutionHostLane || cp.WaitToolCallID != "pending-host" || len(refs) != 1 || refs[0] != parent.ID {
				t.Fatalf("host boundary=%+v refs=%v errors=%v/%v", cp, refs, err, readErr)
			}
			release()
			if daemon.ExecutionRuntime.ExecutionLaneReady(owner.TenantID, owner.PersonID) || !daemon.ExecutionRuntime.ExecutionLaneReady(owner.TenantID, owner.PersonID, parent.ID) {
				t.Fatal("parked host lost priority or blocked itself")
			}
			command := "/queue " + action
			if action == "drop" {
				command = "/queue drop 1"
			}
			if action == "stop" {
				command = "/stop " + parent.ID
			}
			handled, reply, _, err := daemon.tryHandleControlCommand(context.Background(), owner, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "host-origin", Content: command})
			if err != nil || !handled {
				t.Fatalf("cancel %s: %v %q %v", action, handled, reply, err)
			}
			parent, _ = store.GetRun(context.Background(), owner.TenantID, parent.ID)
			queue, _ = store.GetQueued(context.Background(), owner.TenantID, queue.ID)
			if parent.Status != "cancelled" || queue.Status != control.QueueStatusCancelled || !daemon.ExecutionRuntime.ExecutionLaneReady(owner.TenantID, owner.PersonID) {
				t.Fatalf("cancel retained host intent: parent=%+v queue=%+v ready=%v reply=%q", parent, queue, daemon.ExecutionRuntime.ExecutionLaneReady(owner.TenantID, owner.PersonID), reply)
			}
			if _, err := os.Stat(filepath.Join(ws.LocalPath, "host-receipt")); !os.IsNotExist(err) {
				t.Fatalf("cancelled host dispatched: %v", err)
			}
		})
	}
}

func TestHostExecutionLaneResumesOwnFIFOIntentAfterDaemonRuntimeReplacement(t *testing.T) {
	store := controltest.NewStore(t)
	defer store.Close()
	daemon, _, owner := newExecutionWaitServer(t, store)
	provider := &executionHostProvider{&humanWaitProvider{readFirst: true}}
	configureExecutionWaitWorkers(t, daemon, provider)
	release := holdExecutionLane(t, daemon, owner)
	ws := humanWaitWorkspace(t, store, owner, "resumed-host")
	_ = os.WriteFile(filepath.Join(ws.LocalPath, "input.txt"), []byte("before parking"), 0600)
	response, code := daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "host-origin", WorkspaceID: ws.ID, Content: "Run the host command", ApprovalMode: "full-auto", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("admission=%d %+v", code, response)
	}
	var queue *control.QueuedTask
	waitUntil(t, 2*time.Second, func() bool {
		rows, _ := store.ListQueued(context.Background(), owner.TenantID, owner.PersonID, control.QueueStatusQueued)
		if len(rows) != 1 || daemon.coordinator().activeCount(owner.PersonID) != 0 {
			return false
		}
		queue = &rows[0]
		return strings.HasPrefix(queue.IdempotencyKey, "execution-lane:")
	}, "host did not park")
	release()
	// Recovery derives intent from the owned durable queue, without any lane memory.
	daemon.ExecutionRuntime = newTestExecutionRuntime(t)
	if _, err := daemon.refreshExecutionWaits(context.Background(), owner.TenantID, owner.PersonID); err != nil {
		t.Fatal(err)
	}
	if daemon.ExecutionRuntime.ExecutionLaneReady(owner.TenantID, owner.PersonID) || !daemon.ExecutionRuntime.ExecutionLaneReady(owner.TenantID, owner.PersonID, queue.ReplyToRunID) {
		t.Fatal("recovered intent blocked itself or disappeared")
	}
	daemon.coordinator().drainQueue(owner)
	waitUntil(t, 3*time.Second, func() bool {
		queue, _ = store.GetQueued(context.Background(), owner.TenantID, queue.ID)
		return queue.Status == control.QueueStatusDone
	}, "own host intent never became dispatchable")
	content, err := os.ReadFile(filepath.Join(ws.LocalPath, "host-receipt"))
	if err != nil || string(content) != "resumed" || !provider.retainedRead.Load() || !daemon.ExecutionRuntime.ExecutionLaneReady(owner.TenantID, owner.PersonID) {
		t.Fatalf("host did not resume exact ledger/release intent: %q %v retained=%v", content, err, provider.retainedRead.Load())
	}
}
