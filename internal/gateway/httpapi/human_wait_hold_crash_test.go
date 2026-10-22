package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

// toolGate holds one tool at the edge of its body, after the approval, so a test
// can freeze what is durable at that exact instant. A crash leaves precisely
// that behind; nothing runs after it.
type toolGate struct {
	tool          string
	before, after bool
	reached       chan struct{}
	release       chan struct{}
	once          sync.Once
}

func newToolGate(tool string, before, after bool) *toolGate {
	return &toolGate{tool: tool, before: before, after: after, reached: make(chan struct{}, 8), release: make(chan struct{})}
}

// open lets every held call go. Register it with t.Cleanup after the daemon is
// built, so it runs before the daemon waits for its Runs to end.
func (g *toolGate) open() { g.once.Do(func() { close(g.release) }) }

func (g *toolGate) hold() {
	g.reached <- struct{}{}
	<-g.release
}

func (g *toolGate) customize() func(*tools.Dispatcher) {
	return func(d *tools.Dispatcher) {
		d.InjectMiddleware(func(next tools.ToolExecutor) tools.ToolExecutor {
			return func(args map[string]interface{}) (string, error) {
				if name, _ := args["_tool_name"].(string); name != g.tool {
					return next(args)
				}
				if g.before {
					g.hold()
				}
				out, err := next(args)
				if g.after {
					g.hold()
				}
				return out, err
			}
		})
	}
}

func (g *toolGate) waitReached(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the tool never reached its gate")
	}
}

// freezeStore is a second store over a copy of what is durable now: the state a
// daemon that died at this instant leaves behind. The live daemon keeps running
// on the original and never sees the copy.
func freezeStore(t *testing.T, store *control.Store) *control.Store {
	t.Helper()
	snapshot, err := store.BackupWorkHistorySnapshot(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "control.db"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	frozen, err := control.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = frozen.Close() })
	return frozen
}

// recoverFrozen boots a fresh daemon on a frozen store the way the real one
// boots: every Run left running is interrupted, the queue and the decisions
// that were durable but unconsumed are repaired, then ready work starts.
func recoverFrozen(t *testing.T, frozen *control.Store) (*Server, *control.IdentityContext) {
	t.Helper()
	ctx := context.Background()
	restarted, _, _, owner := newHumanWaitServerWithStore(t, frozen)
	if _, err := frozen.MarkInterruptedRuns(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RecoverQueuedAtBoot(ctx); err != nil {
		t.Fatal(err)
	}
	restarted.DrainReadyQueued(ctx)
	return restarted, owner
}

func dispatchesOf(t *testing.T, store *control.Store, owner *control.IdentityContext, taskID, tool string) int {
	t.Helper()
	return countEvents(taskEvents(t, store, owner, taskID), "tool.completed", `"tool":"`+tool+`"`, `"invoked":true`)
}

// A crash while the wait stays in its Run leaves a Run that was running, an ask
// that was live and the exact pending-call checkpoint the wait wrote when it
// started: the same durable facts as a park whose commit was cut off.
func TestCrashDuringTheHoldRecoversTheExactCall(t *testing.T) {
	ctx := context.Background()
	daemon, store, _, owner := newHoldServer(t)
	ws := humanWaitWorkspace(t, store, owner, "crash-hold")
	startHeldHumanWait(t, daemon, ws, "crash-hold")
	approval, run := waitHeldApproval(t, store, owner)
	receipt := filepath.Join(ws.LocalPath, "receipt.txt")
	frozen := freezeStore(t, store)
	restarted, owner2 := recoverFrozen(t, frozen)

	recovered, _ := frozen.GetRun(ctx, owner2.TenantID, run.ID)
	ask, _ := frozen.GetApprovalRequest(ctx, owner2.TenantID, approval.ID)
	cp, _ := frozen.IncompleteLoopCheckpointForRun(ctx, owner2.TenantID, run.ID)
	if recovered.Status != "interrupted" || ask.Status != "pending" || ask.WaiterState != "parked" ||
		cp == nil || cp.Outcome != "await_human" || cp.WaitRef != approval.ID || cp.WaitToolCallID == "" {
		t.Fatalf("after boot: run=%+v ask=%+v checkpoint=%+v", recovered, ask, cp)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatalf("recovery acted without the person: %v", err)
	}
	// The person answers the daemon that came up: the exact call, once.
	answerLaterResumesExactly(t, restarted, frozen, owner2, approval, "crash-hold", receipt)
	if pending, _ := frozen.ListApprovalRequests(ctx, owner2.TenantID, owner2.PersonID, "pending", 10); len(pending) != 0 {
		t.Fatalf("the approval was asked again: %+v", pending)
	}
}

// A crash after the answer was recorded and before the waiter claimed it is the
// window the decision recovery already owns: it resumes the exact call.
func TestCrashBeforeTheClaimResumesTheRecordedDecisionOnce(t *testing.T) {
	ctx := context.Background()
	daemon, store, _, owner := newHoldServer(t)
	ws := humanWaitWorkspace(t, store, owner, "crash-answered")
	startHeldHumanWait(t, daemon, ws, "crash-answered")
	approval, _ := waitHeldApproval(t, store, owner)
	frozen := freezeStore(t, store)
	if _, err := frozen.RespondApprovalRequest(ctx, owner.TenantID, owner.PersonID, approval.ID, "approved", "crash-answered", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	restarted, owner2 := recoverFrozen(t, frozen)
	child := resumedChild(t, frozen, owner2, approval)
	waitUntil(t, 10*time.Second, func() bool {
		current, _ := frozen.GetRun(ctx, owner2.TenantID, child.ID)
		return current != nil && current.Status != "running" && restarted.coordinator().activeCount(owner2.PersonID) == 0
	}, "the recovered decision did not resume")
	data, err := os.ReadFile(filepath.Join(ws.LocalPath, "receipt.txt"))
	if err != nil || string(data) != "approved effect" {
		t.Fatalf("the approved write: %q %v", data, err)
	}
	if got := dispatchesOf(t, frozen, owner2, approval.TaskID, "write_file"); got != 1 {
		t.Fatalf("the approved write was dispatched %d times", got)
	}
	if pending, _ := frozen.ListApprovalRequests(ctx, owner2.TenantID, owner2.PersonID, "pending", 10); len(pending) != 0 {
		t.Fatalf("the approval was asked again: %+v", pending)
	}
}

// answeredInPlaceThenFrozen answers a held approval and freezes the store at the
// gate. The tool is write_file, so the gate sits at the body of the approved call.
func answeredInPlaceThenFrozen(t *testing.T, gate *toolGate) (store, frozen *control.Store, owner *control.IdentityContext, approval *control.ApprovalRequest, run *control.Run, receipt string) {
	t.Helper()
	daemon, store, _, owner := newHoldServer(t, gate.customize())
	t.Cleanup(gate.open)
	ws := humanWaitWorkspace(t, store, owner, "crash-gate")
	startHeldHumanWait(t, daemon, ws, "crash-gate")
	approval, run = waitHeldApproval(t, store, owner)
	if _, err := daemon.respondApprovalByToken(context.Background(), owner, approval.ID, "approved", "crash-gate", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	gate.waitReached(t)
	frozen = freezeStore(t, store)
	return store, frozen, owner, approval, run, filepath.Join(ws.LocalPath, "receipt.txt")
}

// A crash after the answer was claimed in place, before the approved call began,
// leaves a consumed decision and a checkpoint that names nothing pending. The
// call was never dispatched and nothing may dispatch it from what is left: the
// work is asked again, never silently run on an approval already spent.
func TestCrashAfterTheClaimNeverReplaysTheApprovedCall(t *testing.T) {
	ctx := context.Background()
	_, frozen, owner, approval, run, receipt := answeredInPlaceThenFrozen(t, newToolGate("write_file", true, false))

	ask, _ := frozen.GetApprovalRequest(ctx, owner.TenantID, approval.ID)
	if ask.Status != "approved" || ask.WaiterState != "claimed" || ask.ClaimedByRunID != run.ID {
		t.Fatalf("the claim was not durable: %+v", ask)
	}
	requireNoBinding(t, frozen, owner, run.ID)
	if _, err := os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatalf("the approved call began before the gate: %v", err)
	}

	restarted, owner2 := recoverFrozen(t, frozen)
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatalf("recovery dispatched the approved call: %v", err)
	}
	if got := dispatchesOf(t, frozen, owner2, approval.TaskID, "write_file"); got != 0 {
		t.Fatalf("recovery dispatched the approved call %d times", got)
	}
	requireNoBinding(t, frozen, owner2, run.ID)
	if _, err := restarted.respondApprovalByToken(ctx, owner2, approval.ID, "approved", "crash-gate", control.ApprovalDecisionInput{}); err == nil {
		t.Fatal("an approval that was already consumed could be answered again")
	}
}

// A crash after the approved call ran, before the loop wrote its next
// checkpoint, must not run it a second time: nothing in what is durable names it
// as pending.
func TestCrashAfterTheApprovedCallRanNeverRunsItAgain(t *testing.T) {
	ctx := context.Background()
	_, frozen, owner, approval, run, receipt := answeredInPlaceThenFrozen(t, newToolGate("write_file", false, true))

	data, err := os.ReadFile(receipt)
	if err != nil || string(data) != "approved effect" {
		t.Fatalf("the approved call had not run at the gate: %q %v", data, err)
	}
	requireNoBinding(t, frozen, owner, run.ID)

	restarted, owner2 := recoverFrozen(t, frozen)
	time.Sleep(500 * time.Millisecond)
	if got := dispatchesOf(t, frozen, owner2, approval.TaskID, "write_file"); got != 0 {
		t.Fatalf("recovery dispatched an effect that had already happened, %d times", got)
	}
	if pending, _ := frozen.ListApprovalRequests(ctx, owner2.TenantID, owner2.PersonID, "pending", 10); len(pending) != 0 {
		t.Fatalf("recovery asked for the approval again: %+v", pending)
	}
	if _, err := restarted.respondApprovalByToken(ctx, owner2, approval.ID, "approved", "crash-gate", control.ApprovalDecisionInput{}); err == nil {
		t.Fatal("an approval that was already consumed could be answered again")
	}
}

// The calls after a question in the same batch can have effects, and they run in
// the Run once the answer is in. The question's binding must be gone before they
// do: a crash among them could otherwise replay them from it.
func TestAnsweredQuestionLeavesNoBindingWhileTheRestOfItsBatchRuns(t *testing.T) {
	ctx := context.Background()
	gate := newToolGate("read_file", true, false)
	daemon, store, provider, owner := newHoldServer(t, gate.customize())
	t.Cleanup(gate.open)
	provider.clarifyThenRead = true
	ws := humanWaitWorkspace(t, store, owner, "batch")
	attachHoldSession(t, daemon, owner.PersonID, "batch")
	if err := os.WriteFile(filepath.Join(ws.LocalPath, "input.txt"), []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "batch", WorkspaceID: ws.ID, Content: "Choose the receipt format", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("admission: %d %+v", code, response)
	}
	question, run := waitHeldQuestion(t, store, owner)
	if _, err := daemon.answerClarifyTarget(ctx, owner, question, "json", "batch"); err != nil {
		t.Fatal(err)
	}
	gate.waitReached(t)
	frozen := freezeStore(t, store)
	// The answer is consumed and the read has not begun: nothing pending remains.
	requireNoBinding(t, frozen, owner, run.ID)
	answered, _ := frozen.GetClarifyRequest(ctx, owner.TenantID, question.ID)
	if answered.Status != "answered" {
		t.Fatalf("the question was not answered at the gate: %+v", answered)
	}
	_, owner2 := recoverFrozen(t, frozen)
	time.Sleep(500 * time.Millisecond)
	if got := dispatchesOf(t, frozen, owner2, question.TaskID, "read_file"); got != 0 {
		t.Fatalf("recovery dispatched a later call of the batch %d times", got)
	}
}
