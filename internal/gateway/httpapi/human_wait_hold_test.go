package httpapi

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

// newHoldServer is a daemon whose terminal sessions may hold their human waits.
// Its grace is short, so a wait that was going to park has done so long before
// an assertion looks.
func newHoldServer(t *testing.T, customize ...func(*tools.Dispatcher)) (*Server, *control.Store, *humanWaitProvider, *control.IdentityContext) {
	t.Helper()
	daemon, store, provider, owner := newHumanWaitServerWithStore(t, controltest.NewStore(t), customize...)
	daemon.HumanWaitHold = 0
	daemon.HumanWaitGrace = 20 * time.Millisecond
	return daemon, store, provider, owner
}

// attachHoldSession models the live event stream of the terminal that asked.
func attachHoldSession(t *testing.T, daemon *Server, personID, channel string) func() {
	t.Helper()
	_, detach := daemon.events().subscribe(personID, channel, "")
	t.Cleanup(detach)
	return detach
}

func startHeldHumanWait(t *testing.T, daemon *Server, ws *control.Workspace, channel string) func() {
	t.Helper()
	detach := attachHoldSession(t, daemon, ws.OwnerPersonID, channel)
	startHumanWait(t, daemon, ws, channel)
	return detach
}

// waitHeldApproval waits for the one pending approval of a Run that kept it:
// still live, its Run still running, and the exact pending-call checkpoint
// bound to it. It then lets several graces pass to prove nothing parked it.
func waitHeldApproval(t *testing.T, store *control.Store, owner *control.IdentityContext) (*control.ApprovalRequest, *control.Run) {
	t.Helper()
	ctx := context.Background()
	var approval *control.ApprovalRequest
	waitUntil(t, 5*time.Second, func() bool {
		pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
		if len(pending) != 1 || pending[0].WaiterState != "live" {
			return false
		}
		cp, _ := store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, pending[0].RunID)
		if cp == nil || cp.Outcome != "await_human" || cp.WaitRef != pending[0].ID || cp.WaitToolCallID == "" {
			return false
		}
		approval = &pending[0]
		return true
	}, "the ask did not stay in its Run with its pending-call checkpoint armed")
	time.Sleep(150 * time.Millisecond)
	current, _ := store.GetApprovalRequest(ctx, owner.TenantID, approval.ID)
	run, _ := store.GetRun(ctx, owner.TenantID, approval.RunID)
	if current == nil || current.WaiterState != "live" || run == nil || run.Status != "running" {
		t.Fatalf("the hold did not outlast the grace: approval=%+v run=%+v", current, run)
	}
	return approval, run
}

func waitHeldQuestion(t *testing.T, store *control.Store, owner *control.IdentityContext) (*control.ClarifyRequest, *control.Run) {
	t.Helper()
	ctx := context.Background()
	var question *control.ClarifyRequest
	waitUntil(t, 5*time.Second, func() bool {
		pending, _ := store.ListClarifyRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
		if len(pending) != 1 || pending[0].ParkedAt != nil {
			return false
		}
		cp, _ := store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, pending[0].RunID)
		if cp == nil || cp.Outcome != "await_human" || cp.WaitRef != pending[0].ID {
			return false
		}
		question = &pending[0]
		return true
	}, "the question did not stay in its Run with its pending-call checkpoint armed")
	time.Sleep(150 * time.Millisecond)
	current, _ := store.GetClarifyRequest(ctx, owner.TenantID, question.ID)
	run, _ := store.GetRun(ctx, owner.TenantID, question.RunID)
	if current == nil || current.ParkedAt != nil || run == nil || run.Status != "running" {
		t.Fatalf("the hold did not outlast the grace: question=%+v run=%+v", current, run)
	}
	return question, run
}

// taskEvents is every event of the task in append order. ListTaskEvents returns
// only the newest few, so a chain of Runs would lose its early events.
func taskEvents(t *testing.T, store *control.Store, owner *control.IdentityContext, taskID string) []control.Event {
	t.Helper()
	var all []control.Event
	cursor := int64(0)
	for {
		page, err := store.ListTaskEventsAfter(context.Background(), taskID, cursor, 200)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if len(page) < 200 {
			return all
		}
		cursor = page[len(page)-1].Cursor
	}
}

func countEvents(events []control.Event, eventType string, contains ...string) int {
	n := 0
	for _, event := range events {
		matched := event.Type == eventType
		for _, part := range contains {
			matched = matched && strings.Contains(string(event.Payload), part)
		}
		if matched {
			n++
		}
	}
	return n
}

// requireNoBinding fails when the Run still carries a pending-call binding: a
// consumed or abandoned wait must not leave one for a later resume to dispatch.
func requireNoBinding(t *testing.T, store *control.Store, owner *control.IdentityContext, runID string) {
	t.Helper()
	cp, err := store.IncompleteLoopCheckpointForRun(context.Background(), owner.TenantID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if cp != nil && (cp.Outcome == "await_human" || cp.WaitRef != "" || cp.WaitToolCallID != "") {
		t.Fatalf("run %s still carries a pending-call binding: %+v", runID, cp)
	}
}

// waitNoBinding is requireNoBinding for a wait that ended by cancellation. The
// Run is finalized without waiting for its wait's handler to unwind, so the
// handler's cleanup may land after the Run is already finished; it must land.
func waitNoBinding(t *testing.T, store *control.Store, owner *control.IdentityContext, runID string) {
	t.Helper()
	waitUntil(t, 5*time.Second, func() bool {
		cp, _ := store.IncompleteLoopCheckpointForRun(context.Background(), owner.TenantID, runID)
		return cp == nil || (cp.Outcome != "await_human" && cp.WaitRef == "" && cp.WaitToolCallID == "")
	}, "the ended wait left its pending-call binding on its Run")
}

func TestApprovalAnsweredInsideTheHoldContinuesTheSameRun(t *testing.T) {
	for _, decision := range []string{"approved", "rejected"} {
		t.Run(decision, func(t *testing.T) {
			ctx := context.Background()
			daemon, store, _, owner := newHoldServer(t)
			ws := humanWaitWorkspace(t, store, owner, "hold")
			startHeldHumanWait(t, daemon, ws, "hold")
			approval, run := waitHeldApproval(t, store, owner)
			if _, err := os.Stat(filepath.Join(ws.LocalPath, "receipt.txt")); !os.IsNotExist(err) {
				t.Fatalf("the unanswered ask had an effect: %v", err)
			}
			if _, err := daemon.respondApprovalByToken(ctx, owner, approval.ID, decision, "hold", control.ApprovalDecisionInput{}); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, 10*time.Second, func() bool {
				current, _ := store.GetRun(ctx, owner.TenantID, run.ID)
				return current != nil && current.Status != "running" && daemon.coordinator().activeCount(owner.PersonID) == 0
			}, "the answered Run did not finish")

			runs, err := store.ListTaskRuns(ctx, owner.TenantID, approval.TaskID, 10)
			if err != nil || len(runs) != 1 || runs[0].ID != run.ID || runs[0].ResumesRunID != "" {
				t.Fatalf("the answer started another Run: %+v %v", runs, err)
			}
			if runs[0].Status == "waiting_user" {
				t.Fatalf("the Run parked after being answered: %+v", runs[0])
			}
			answered, _ := store.GetApprovalRequest(ctx, owner.TenantID, approval.ID)
			if answered.Status != decision || answered.WaiterState != "claimed" || answered.ClaimedByRunID != run.ID || answered.ResumeQueueID != "" {
				t.Fatalf("approval after the answer: %+v", answered)
			}
			events := taskEvents(t, store, owner, approval.TaskID)
			if countEvents(events, "approval.parked") != 0 || countEvents(events, "approval.requested") != 1 {
				t.Fatalf("lifecycle events: parked=%d requested=%d", countEvents(events, "approval.parked"), countEvents(events, "approval.requested"))
			}
			requireNoBinding(t, store, owner, run.ID)
			data, readErr := os.ReadFile(filepath.Join(ws.LocalPath, "receipt.txt"))
			wrote := countEvents(events, "tool.completed", `"tool":"write_file"`, `"invoked":true`)
			if decision == "approved" && (readErr != nil || string(data) != "approved effect" || wrote != 1) {
				t.Fatalf("approved effect: %q %v dispatched %d times", data, readErr, wrote)
			}
			if decision == "rejected" && (!os.IsNotExist(readErr) || wrote != 0) {
				t.Fatalf("rejected action ran: %q %v dispatched %d times", data, readErr, wrote)
			}
		})
	}
}

func TestQuestionAnsweredInsideTheHoldContinuesTheSameRun(t *testing.T) {
	ctx := context.Background()
	daemon, store, provider, owner := newHoldServer(t)
	provider.clarification = true
	ws := humanWaitWorkspace(t, store, owner, "hold-question")
	attachHoldSession(t, daemon, owner.PersonID, "hold-question")
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "hold-question", WorkspaceID: ws.ID, Content: "Choose the receipt format", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("admission: %d %+v", code, response)
	}
	question, run := waitHeldQuestion(t, store, owner)
	if _, err := daemon.answerClarifyTarget(ctx, owner, question, "json", "hold-question"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		current, _ := store.GetRun(ctx, owner.TenantID, run.ID)
		return current != nil && current.Status != "running" && daemon.coordinator().activeCount(owner.PersonID) == 0
	}, "the answered Run did not finish")
	runs, err := store.ListTaskRuns(ctx, owner.TenantID, question.TaskID, 10)
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID || runs[0].Status == "waiting_user" {
		t.Fatalf("the answer started another Run or parked it: %+v %v", runs, err)
	}
	answered, _ := store.GetClarifyRequest(ctx, owner.TenantID, question.ID)
	if answered.Status != "answered" || answered.ParkedAt != nil {
		t.Fatalf("question after the answer: %+v", answered)
	}
	requireNoBinding(t, store, owner, run.ID)
	pending, _ := store.ListClarifyRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
	if len(pending) != 0 {
		t.Fatalf("the question was asked again: %+v", pending)
	}
}

func TestHeldWaitStaysWhileAttachedAndNothingWaits(t *testing.T) {
	ctx := context.Background()
	daemon, store, _, owner := newHoldServer(t)
	ws := humanWaitWorkspace(t, store, owner, "quiet")
	startHeldHumanWait(t, daemon, ws, "quiet")
	approval, run := waitHeldApproval(t, store, owner)
	// Several polls later nothing has changed: the hold needs a reason to end.
	time.Sleep(2500 * time.Millisecond)
	current, _ := store.GetApprovalRequest(ctx, owner.TenantID, approval.ID)
	now, _ := store.GetRun(ctx, owner.TenantID, run.ID)
	if current.WaiterState != "live" || now.Status != "running" || daemon.coordinator().activeCount(owner.PersonID) != 1 {
		t.Fatalf("the hold ended without a reason: approval=%+v run=%+v", current, now)
	}
}

// parkedBecause waits for the approval to park and returns the reason it kept.
// The parked event follows the park commit, so it is awaited here too: a caller
// that counts events right after the park would otherwise read it too early.
func parkedBecause(t *testing.T, daemon *Server, store *control.Store, owner *control.IdentityContext, approvalID string) *control.ApprovalRequest {
	t.Helper()
	var parked *control.ApprovalRequest
	waitUntil(t, 8*time.Second, func() bool {
		current, _ := store.GetApprovalRequest(context.Background(), owner.TenantID, approvalID)
		run, _ := store.GetRun(context.Background(), owner.TenantID, current.RunID)
		if current.WaiterState == "parked" && run != nil && run.Status == "waiting_user" {
			parked = current
			return true
		}
		return false
	}, "the held wait did not release its worker")
	waitUntil(t, 5*time.Second, func() bool {
		return countEvents(taskEvents(t, store, owner, parked.TaskID), "approval.parked") >= 1
	}, "the parked wait never announced that it parked")
	return parked
}

// answerLaterResumesExactly answers a parked approval and checks the old path
// is intact: another Run resumes the exact call, which runs exactly once.
func answerLaterResumesExactly(t *testing.T, daemon *Server, store *control.Store, owner *control.IdentityContext, approval *control.ApprovalRequest, channel, receipt string) {
	t.Helper()
	answerLater(t, daemon, store, owner, approval, channel, receipt, false)
}

// answerLater is answerLaterResumesExactly, or, when another Run wrote the same
// directory while this one was parked (overlapped), the existing rule that the
// cached call is handed back for revalidation instead of being dispatched blind.
func answerLater(t *testing.T, daemon *Server, store *control.Store, owner *control.IdentityContext, approval *control.ApprovalRequest, channel, receipt string, overlapped bool) {
	t.Helper()
	ctx := context.Background()
	// A drain that arrives while a finishing Run's drain is still running is
	// skipped, and only the daemon's periodic tick, which these tests do not run,
	// would then start the queued continuation. Answer once nothing is unwinding
	// or draining, as waitParkedApproval does for the first of the two.
	waitUntil(t, 10*time.Second, func() bool {
		c := daemon.coordinator()
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.active[owner.PersonID]) == 0 && !c.draining[owner.PersonID]
	}, "an earlier Run was still unwinding when the answer was due")
	if _, err := daemon.respondApprovalByToken(ctx, owner, approval.ID, "approved", channel, control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	child := resumedChild(t, store, owner, approval)
	waitUntil(t, 10*time.Second, func() bool {
		current, _ := store.GetRun(ctx, owner.TenantID, child.ID)
		return current != nil && current.Status != "running" && daemon.coordinator().activeCount(owner.PersonID) == 0
	}, "the continuation did not finish")
	events := taskEvents(t, store, owner, approval.TaskID)
	wrote := countEvents(events, "tool.completed", `"tool":"write_file"`, `"invoked":true`)
	if overlapped {
		if _, err := os.Stat(receipt); !os.IsNotExist(err) || wrote != 0 || countEvents(events, "tool.completed", "resume_revalidation_required") != 1 {
			t.Fatalf("overlapping work during the park must hand the call back, not run it: stat=%v dispatched=%d revalidations=%d",
				err, wrote, countEvents(events, "tool.completed", "resume_revalidation_required"))
		}
		return
	}
	data, err := os.ReadFile(receipt)
	if err != nil || string(data) != "approved effect" {
		t.Fatalf("the approved write: %q %v", data, err)
	}
	if wrote != 1 {
		t.Fatalf("the approved write was dispatched %d times", wrote)
	}
}

func TestHeldWaitParksWhenOtherWorkNeedsTheSlot(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		limit  int
		shared bool // the other work writes the same directory
	}{
		{"active run cap", 1, false},
		{"same writable directory", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon, store, provider, owner := newHoldServer(t)
			daemon.coordinator().activeLimit = tc.limit
			first := humanWaitWorkspace(t, store, owner, "first")
			startHeldHumanWait(t, daemon, first, "first")
			approval, _ := waitHeldApproval(t, store, owner)
			other := humanWaitWorkspace(t, store, owner, "second")
			if tc.shared {
				other = first
			}
			response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "second", WorkspaceID: other.ID, Content: "independent work", Async: true})
			if code != 200 || !response.Accepted || response.Turn == nil || response.Turn.Status != "queued" {
				t.Fatalf("the other work was not queued behind the held Run: %d %+v", code, response)
			}
			parked := parkedBecause(t, daemon, store, owner, approval.ID)
			if parked.ParkReason != control.HumanWaitParkCapacity {
				t.Fatalf("park reason = %q, want capacity", parked.ParkReason)
			}
			waitUntil(t, 8*time.Second, func() bool { return provider.independent.Load() == 1 }, "the queued work never got the slot the held Run released")
			if countEvents(taskEvents(t, store, owner, approval.TaskID), "approval.parked", `"reason":"capacity"`) != 1 {
				t.Fatal("the parked event did not carry its reason")
			}
			answerLater(t, daemon, store, owner, approval, "first", filepath.Join(first.LocalPath, "receipt.txt"), tc.shared)
		})
	}
}

func TestTwoHeldWaitsStillLetOtherWorkRun(t *testing.T) {
	ctx := context.Background()
	daemon, store, provider, owner := newHoldServer(t)
	for _, name := range []string{"first", "second"} {
		ws := humanWaitWorkspace(t, store, owner, name)
		startHeldHumanWait(t, daemon, ws, name)
		waitUntil(t, 5*time.Second, func() bool {
			pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
			for _, a := range pending {
				if a.RequestedChannel == name && a.WaiterState == "live" {
					return true
				}
			}
			return false
		}, "the ask did not stay in its Run")
	}
	if daemon.coordinator().activeCount(owner.PersonID) != 2 {
		t.Fatalf("both waits should hold a slot: %d active", daemon.coordinator().activeCount(owner.PersonID))
	}
	ws := humanWaitWorkspace(t, store, owner, "third")
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "third", WorkspaceID: ws.ID, Content: "independent work", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("third admission: %d %+v", code, response)
	}
	waitUntil(t, 8*time.Second, func() bool { return provider.independent.Load() == 1 }, "two held waits kept the person's other work from running")
}

func TestHeldWaitParksWhenItsClientDetaches(t *testing.T) {
	daemon, store, _, owner := newHoldServer(t)
	ws := humanWaitWorkspace(t, store, owner, "detach")
	detach := startHeldHumanWait(t, daemon, ws, "detach")
	approval, _ := waitHeldApproval(t, store, owner)
	// The terminal goes quiet for longer than a heartbeat stays valid.
	detach()
	parked := parkedBecause(t, daemon, store, owner, approval.ID)
	if parked.ParkReason != control.HumanWaitParkClientDetached {
		t.Fatalf("park reason = %q, want client_detached", parked.ParkReason)
	}
	answerLaterResumesExactly(t, daemon, store, owner, approval, "detach", filepath.Join(ws.LocalPath, "receipt.txt"))
}

func TestHeldWaitParksWhenTheDaemonDrainsAndRecoversAfterRestart(t *testing.T) {
	ctx := context.Background()
	daemon, store, _, owner := newHoldServer(t)
	ws := humanWaitWorkspace(t, store, owner, "drain")
	startHeldHumanWait(t, daemon, ws, "drain")
	approval, _ := waitHeldApproval(t, store, owner)
	daemon.beginDraining("test restart")
	parked := parkedBecause(t, daemon, store, owner, approval.ID)
	if parked.ParkReason != control.HumanWaitParkDraining {
		t.Fatalf("park reason = %q, want draining", parked.ParkReason)
	}
	// A restart waits for every Run to leave its worker, which it now has.
	idle, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if !daemon.waitForIdle(idle) {
		t.Fatal("the drain would wait for a Run that already parked")
	}
	// The next daemon answers the same request and resumes the exact call.
	restarted, _, _, _ := newHumanWaitServerWithStore(t, store)
	answerLaterResumesExactly(t, restarted, store, owner, approval, "drain", filepath.Join(ws.LocalPath, "receipt.txt"))
}

func TestHeldWaitParksAtItsCap(t *testing.T) {
	ctx := context.Background()
	daemon, store, _, owner := newHoldServer(t)
	daemon.HumanWaitHold = 1200 * time.Millisecond
	ws := humanWaitWorkspace(t, store, owner, "cap")
	started := time.Now()
	startHeldHumanWait(t, daemon, ws, "cap")
	approval, _ := waitHeldApproval(t, store, owner)
	parked := parkedBecause(t, daemon, store, owner, approval.ID)
	if parked.ParkReason != control.HumanWaitParkHoldCap {
		t.Fatalf("park reason = %q, want hold_cap", parked.ParkReason)
	}
	if held := time.Since(started); held < time.Second {
		t.Fatalf("parked after %s, before its cap", held)
	}
	if countEvents(taskEvents(t, store, owner, approval.TaskID), "approval.parked", `"reason":"hold_cap"`) != 1 {
		t.Fatal("the parked event did not carry its reason")
	}
	cp, _ := store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, approval.RunID)
	if cp == nil || cp.Outcome != "await_human" || cp.WaitRef != approval.ID {
		t.Fatalf("the parked Run lost its exact checkpoint: %+v", cp)
	}
	answerLaterResumesExactly(t, daemon, store, owner, approval, "cap", filepath.Join(ws.LocalPath, "receipt.txt"))
}

func TestWaitsThatMayNotHoldParkAtTheGrace(t *testing.T) {
	ctx := context.Background()
	t.Run("selfmind send has no panel", func(t *testing.T) {
		daemon, store, _, owner := newHoldServer(t)
		ws := humanWaitWorkspace(t, store, owner, "send")
		startHeldHumanWait(t, daemon, ws, "cli")
		var approval *control.ApprovalRequest
		waitUntil(t, 5*time.Second, func() bool {
			pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
			if len(pending) == 1 && pending[0].WaiterState == "parked" {
				approval = &pending[0]
				return true
			}
			return false
		}, "a wait that must not hold did not park at the grace")
		if approval.ParkReason != control.HumanWaitParkGrace {
			t.Fatalf("park reason = %q, want grace", approval.ParkReason)
		}
	})
	t.Run("an IM chat parks even while a terminal is attached", func(t *testing.T) {
		daemon, store, _, owner := newHoldServer(t)
		if _, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, "telegram", "tg_hold", "Me on Telegram"); err != nil {
			t.Fatal(err)
		}
		daemon.presenceTracker().Touch(owner.PersonID, "cli")
		ws := humanWaitWorkspace(t, store, owner, "im")
		response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "telegram", PlatformUserID: "tg_hold", Channel: "tg-chat", WorkspaceID: ws.ID, Content: "Write a receipt after approval", ApprovalMode: "smart", Async: true})
		if code != 200 || !response.Accepted {
			t.Fatalf("admission: %d %+v", code, response)
		}
		var approval *control.ApprovalRequest
		waitUntil(t, 5*time.Second, func() bool {
			pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
			if len(pending) == 1 && pending[0].WaiterState == "parked" {
				approval = &pending[0]
				return true
			}
			return false
		}, "an IM-origin wait did not park at the grace")
		if approval.ParkReason != control.HumanWaitParkGrace {
			t.Fatalf("park reason = %q, want grace", approval.ParkReason)
		}
	})
	t.Run("the hold is off", func(t *testing.T) {
		daemon, store, _, owner := newHoldServer(t)
		daemon.HumanWaitHold = -1
		ws := humanWaitWorkspace(t, store, owner, "off")
		startHeldHumanWait(t, daemon, ws, "off")
		var approval *control.ApprovalRequest
		waitUntil(t, 5*time.Second, func() bool {
			pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
			if len(pending) == 1 && pending[0].WaiterState == "parked" {
				approval = &pending[0]
				return true
			}
			return false
		}, "with the hold off the wait did not park at the grace")
		if approval.ParkReason != control.HumanWaitParkGrace {
			t.Fatalf("park reason = %q, want grace", approval.ParkReason)
		}
	})
}

func TestHumanWaitSourceCanHold(t *testing.T) {
	ctx := context.Background()
	cli := &control.IdentityContext{Platform: "cli"}
	for _, tc := range []struct {
		name     string
		identity *control.IdentityContext
		req      api.MessageRequest
		channel  string
		want     bool
	}{
		{"a terminal session", cli, api.MessageRequest{}, "5f3c1c52-8a43-4d1e-bd3f-0c1f1b3a2b11", true},
		{"a session reached by its approval answer", cli, api.MessageRequest{Origin: runOriginApproval}, "session-a", true},
		{"a provider wait continuation of terminal work", cli, api.MessageRequest{Origin: runOriginProviderWait}, "session-a", true},
		{"selfmind send", cli, api.MessageRequest{}, "cli", false},
		{"a terminal alias", cli, api.MessageRequest{}, "#TUI", false},
		{"no channel", cli, api.MessageRequest{}, "", false},
		{"telegram", &control.IdentityContext{Platform: "telegram"}, api.MessageRequest{}, "chat-1", false},
		{"weixin", &control.IdentityContext{Platform: "weixin"}, api.MessageRequest{}, "chat-1", false},
		{"a schedule", cli, api.MessageRequest{Origin: runOriginCron}, "session-a", false},
		{"a watcher finalization", cli, api.MessageRequest{Origin: runOriginWatch}, "session-a", false},
		{"a recovery", cli, api.MessageRequest{Origin: runOriginRecovery}, "session-a", false},
		{"no identity", nil, api.MessageRequest{}, "session-a", false},
	} {
		if got := humanWaitSourceCanHold(ctx, tc.req, tc.identity, tc.channel); got != tc.want {
			t.Errorf("%s: humanWaitSourceCanHold = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStartHumanWaitHoldRefusesWhenAnyConditionFails(t *testing.T) {
	ctx := context.Background()
	daemon, store, _, owner := newHoldServer(t)
	coordinator := daemon.coordinator()
	allowed := newHumanWaitSettings([]humanWaitOption{withHumanWaitHold("unit-session")})
	attachHoldSession(t, daemon, owner.PersonID, "unit-session")
	daemon.presenceTracker().Touch(owner.PersonID, "cli")
	if coordinator.startHumanWaitHold(ctx, allowed, owner, time.Minute) == nil {
		t.Fatal("an attached terminal with nothing waiting was refused")
	}
	if coordinator.startHumanWaitHold(ctx, humanWaitSettings{}, owner, time.Minute) != nil {
		t.Fatal("a Run that may not hold was given a hold")
	}
	if coordinator.startHumanWaitHold(ctx, allowed, nil, time.Minute) != nil {
		t.Fatal("a wait without a person was given a hold")
	}
	if coordinator.startHumanWaitHold(ctx, allowed, owner, 0) != nil || coordinator.startHumanWaitHold(ctx, allowed, owner, -time.Second) != nil {
		t.Fatal("a wait with no time left was given a hold")
	}
	if hold := coordinator.startHumanWaitHold(ctx, allowed, owner, 5*time.Second); hold == nil || time.Until(hold.deadline) > 5*time.Second {
		t.Fatalf("a hold outlasted the wait that bounds it: %+v", hold)
	}
	daemon.HumanWaitHold = -1
	if coordinator.startHumanWaitHold(ctx, allowed, owner, time.Minute) != nil {
		t.Fatal("a daemon with the hold off gave one")
	}
	daemon.HumanWaitHold = 0

	queued, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, Content: "needs a slot"})
	if err != nil {
		t.Fatal(err)
	}
	if coordinator.startHumanWaitHold(ctx, allowed, owner, time.Minute) != nil {
		t.Fatal("work waiting for a slot did not end the hold before it began")
	}
	if err := store.MarkQueued(ctx, owner.TenantID, queued.ID, control.QueueStatusCancelled); err != nil {
		t.Fatal(err)
	}
	daemon.beginDraining("restart")
	if coordinator.startHumanWaitHold(ctx, allowed, owner, time.Minute) != nil {
		t.Fatal("a draining daemon gave a hold")
	}
	daemon.cancelPendingShutdown()
	other := &control.IdentityContext{TenantID: owner.TenantID, PersonID: "somebody-else"}
	if coordinator.startHumanWaitHold(ctx, allowed, other, time.Minute) != nil {
		t.Fatal("a person with no terminal attached was given a hold")
	}
	if coordinator.startHumanWaitHold(ctx, allowed, owner, time.Minute) == nil {
		t.Fatal("clearing every condition did not restore the hold")
	}
}

// A wait that cannot be armed (no replayable top-level call) is refused without touching the Run's checkpoint.
func TestAnUnarmableWaitIsNotHeldAndLeavesTheCheckpointAlone(t *testing.T) {
	ctx := context.Background()
	daemon, store, _, owner := newHoldServer(t)
	ws := humanWaitWorkspace(t, store, owner, "unarmable")
	startHeldHumanWait(t, daemon, ws, "unarmable")
	approval, run := waitHeldApproval(t, store, owner)
	before, _ := store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, approval.RunID)
	// A context with no replayable top-level call cannot be armed.
	sink := daemon.coordinator().newLoopCheckpointSink(owner, &control.Task{ID: approval.TaskID}, run)
	bare := kernel.WithLoopCheckpointSink(ctx, sink)
	if arm := daemon.coordinator().armHumanWait(bare, owner, run.ID, control.LoopCheckpointWaitApproval, approval.ID, "write_file"); arm != nil {
		t.Fatalf("a wait with no pending call was armed: %+v", arm)
	}
	after, _ := store.IncompleteLoopCheckpointForRun(ctx, owner.TenantID, approval.RunID)
	if before == nil || after == nil || before.Outcome != after.Outcome || before.WaitRef != after.WaitRef || string(before.Snapshot) != string(after.Snapshot) {
		t.Fatalf("a refused arm rewrote the checkpoint: before=%+v after=%+v", before, after)
	}
}

// A held wait that ends without an answer or a park, because the person stopped
// it or a forced gateway stop interrupted it first, must leave no pending-call
// binding behind: the Run is not resumed from it, and nothing may read it as work
// still waiting on a person.
func TestEndingAHeldWaitWithoutAnAnswerLeavesNoPendingCallBinding(t *testing.T) {
	for _, kind := range []string{control.LoopCheckpointWaitApproval, control.LoopCheckpointWaitClarification} {
		for _, how := range []string{"stop", "forced gateway stop"} {
			t.Run(kind+"/"+how, func(t *testing.T) {
				ctx := context.Background()
				daemon, store, provider, owner := newHoldServer(t)
				ws := humanWaitWorkspace(t, store, owner, "stop")
				var run *control.Run
				if kind == control.LoopCheckpointWaitApproval {
					startHeldHumanWait(t, daemon, ws, "stop")
					_, run = waitHeldApproval(t, store, owner)
				} else {
					startHeldQuestion(t, daemon, provider, ws, "stop")
					_, run = waitHeldQuestion(t, store, owner)
				}
				if how == "stop" {
					if daemon.coordinator().stopActiveRun(owner.PersonID, run.ID) == nil {
						t.Fatal("the held Run was not active")
					}
				} else {
					// A forced stop marks the Run interrupted before it unwinds the wait.
					daemon.coordinator().stopAllActive("test forced stop")
				}
				waitUntil(t, 8*time.Second, func() bool {
					current, _ := store.GetRun(ctx, owner.TenantID, run.ID)
					return current != nil && current.Status != "running" && daemon.coordinator().activeCount(owner.PersonID) == 0
				}, "the ended Run did not unwind")
				waitNoBinding(t, store, owner, run.ID)
				if _, err := os.Stat(filepath.Join(ws.LocalPath, "receipt.txt")); !os.IsNotExist(err) {
					t.Fatalf("an ended ask had an effect: %v", err)
				}
			})
		}
	}
}

// An answer is never consumed while the checkpoint it would release cannot be put
// back: the wait keeps the answer, says so, and consumes it in place once it can.
func TestAnAnswerIsNotConsumedWhileItsCheckpointCannotBePutBack(t *testing.T) {
	for _, kind := range []string{control.LoopCheckpointWaitApproval, control.LoopCheckpointWaitClarification} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			daemon, _, provider, owner := newHumanWaitServerWithStore(t, store)
			daemon.HumanWaitHold, daemon.HumanWaitGrace = 0, 20*time.Millisecond
			ws := humanWaitWorkspace(t, store, owner, "fail-closed")
			var run *control.Run
			var taskID string
			var answer func()
			if kind == control.LoopCheckpointWaitApproval {
				startHeldHumanWait(t, daemon, ws, "fail-closed")
				var approval *control.ApprovalRequest
				approval, run = waitHeldApproval(t, store, owner)
				taskID = approval.TaskID
				answer = func() {
					if _, err := daemon.respondApprovalByToken(ctx, owner, approval.ID, "approved", "fail-closed", control.ApprovalDecisionInput{}); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				startHeldQuestion(t, daemon, provider, ws, "fail-closed")
				var question *control.ClarifyRequest
				question, run = waitHeldQuestion(t, store, owner)
				taskID = question.TaskID
				answer = func() {
					if _, err := daemon.answerClarifyTarget(ctx, owner, question, "json", "fail-closed"); err != nil {
						t.Fatal(err)
					}
				}
			}
			execRawIn(t, dir, `CREATE TRIGGER fail_restore BEFORE UPDATE ON loop_checkpoints BEGIN SELECT RAISE(ABORT,'checkpoint store fault'); END`)
			answer()
			waitUntil(t, 8*time.Second, func() bool {
				return countEvents(taskEvents(t, store, owner, taskID), "human_wait.control_failed") >= 1
			}, "the wait did not say that it cannot consume the answer yet")
			time.Sleep(1500 * time.Millisecond)
			current, _ := store.GetRun(ctx, owner.TenantID, run.ID)
			if current.Status != "running" {
				t.Fatalf("the answer was consumed without its checkpoint put back: %+v", current)
			}
			if kind == control.LoopCheckpointWaitApproval {
				pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "approved", 10)
				if len(pending) != 1 || pending[0].WaiterState != "live" || pending[0].ClaimedByRunID != "" {
					t.Fatalf("the decision was claimed although the checkpoint could not be put back: %+v", pending)
				}
				if _, err := os.Stat(filepath.Join(ws.LocalPath, "receipt.txt")); !os.IsNotExist(err) {
					t.Fatalf("the approved call ran before its wait ended: %v", err)
				}
			}
			execRawIn(t, dir, `DROP TRIGGER fail_restore`)
			waitUntil(t, 10*time.Second, func() bool {
				run, _ := store.GetRun(ctx, owner.TenantID, run.ID)
				return run != nil && run.Status != "running" && daemon.coordinator().activeCount(owner.PersonID) == 0
			}, "the answer was not consumed once the store worked")
			runs, _ := store.ListTaskRuns(ctx, owner.TenantID, taskID, 10)
			if len(runs) != 1 {
				t.Fatalf("the answer started another Run: %+v", runs)
			}
			requireNoBinding(t, store, owner, run.ID)
			if kind == control.LoopCheckpointWaitApproval {
				data, err := os.ReadFile(filepath.Join(ws.LocalPath, "receipt.txt"))
				if err != nil || string(data) != "approved effect" {
					t.Fatalf("the approved write: %q %v", data, err)
				}
			}
		})
	}
}

// Answering from another endpoint is the same answer: the held Run takes it.
func TestAnApprovalAnsweredFromAnotherEndpointContinuesTheHeldRun(t *testing.T) {
	ctx := context.Background()
	daemon, store, _, owner := newHoldServer(t)
	phone, err := store.BindAccount(ctx, owner.TenantID, owner.PersonID, "telegram", "tg_cross", "Me on Telegram")
	if err != nil {
		t.Fatal(err)
	}
	ws := humanWaitWorkspace(t, store, owner, "cross")
	startHeldHumanWait(t, daemon, ws, "cross")
	approval, run := waitHeldApproval(t, store, owner)
	if _, err := daemon.respondApprovalByToken(ctx, phone, approval.ID, "approved", "tg-chat", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		current, _ := store.GetRun(ctx, owner.TenantID, run.ID)
		return current != nil && current.Status != "running" && daemon.coordinator().activeCount(owner.PersonID) == 0
	}, "the answer from another endpoint did not continue the held Run")
	runs, _ := store.ListTaskRuns(ctx, owner.TenantID, approval.TaskID, 10)
	answered, _ := store.GetApprovalRequest(ctx, owner.TenantID, approval.ID)
	if len(runs) != 1 || answered.WaiterState != "claimed" || answered.ApprovedChannel != "tg-chat" {
		t.Fatalf("runs=%+v approval=%+v", runs, answered)
	}
	data, err := os.ReadFile(filepath.Join(ws.LocalPath, "receipt.txt"))
	if err != nil || string(data) != "approved effect" {
		t.Fatalf("the approved write: %q %v", data, err)
	}
}

func TestImActivityDoesNotKeepATerminalWaitHeld(t *testing.T) {
	daemon, store, _, owner := newHoldServer(t)
	ws := humanWaitWorkspace(t, store, owner, "phone")
	detach := startHeldHumanWait(t, daemon, ws, "phone")
	approval, _ := waitHeldApproval(t, store, owner)
	// The terminal goes quiet while the person is busy in a chat: somebody is
	// around, but not at the terminal that asked.
	detach()
	daemon.presenceTracker().Touch(owner.PersonID, "telegram")
	parked := parkedBecause(t, daemon, store, owner, approval.ID)
	if parked.ParkReason != control.HumanWaitParkClientDetached {
		t.Fatalf("park reason = %q, want client_detached", parked.ParkReason)
	}
}

func TestAnUnreadableQueueCountsAsPressure(t *testing.T) {
	ctx := context.Background()
	daemon, store, _, owner := newHoldServer(t)
	coordinator := daemon.coordinator()
	if coordinator.otherWorkWaitsForASlot(ctx, owner) {
		t.Fatal("an empty queue was reported as pressure")
	}
	if !(&RunCoordinator{srv: &Server{}}).otherWorkWaitsForASlot(ctx, owner) {
		t.Fatal("a daemon with no control store proved nobody was waiting")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if !coordinator.otherWorkWaitsForASlot(ctx, owner) {
		t.Fatal("a queue that cannot be read proved nobody was waiting")
	}
}

// execRawIn runs one statement against the control database in dir, beside the
// store, for the faults a test injects.
func execRawIn(t *testing.T, dir, statement string) {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

// A wait whose pending call cannot be checkpointed takes today's path: the
// grace runs, parking is refused and said so, and the ask stays in its Run, where
// its answer still lands.
func TestAWaitWhoseCheckpointCannotBeWrittenFallsBackToTheGracePath(t *testing.T) {
	for _, kind := range []string{control.LoopCheckpointWaitApproval, control.LoopCheckpointWaitClarification} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			daemon, _, provider, owner := newHumanWaitServerWithStore(t, store)
			daemon.HumanWaitHold, daemon.HumanWaitGrace = 0, 20*time.Millisecond
			execRawIn(t, dir, `CREATE TRIGGER fail_checkpoint_insert BEFORE INSERT ON loop_checkpoints BEGIN SELECT RAISE(ABORT,'checkpoint store fault'); END`)
			execRawIn(t, dir, `CREATE TRIGGER fail_checkpoint_update BEFORE UPDATE ON loop_checkpoints BEGIN SELECT RAISE(ABORT,'checkpoint store fault'); END`)
			ws := humanWaitWorkspace(t, store, owner, "no-checkpoint")
			var taskID, runID string
			if kind == control.LoopCheckpointWaitApproval {
				startHeldHumanWait(t, daemon, ws, "no-checkpoint")
			} else {
				startHeldQuestion(t, daemon, provider, ws, "no-checkpoint")
			}
			waitUntil(t, 5*time.Second, func() bool {
				if kind == control.LoopCheckpointWaitApproval {
					pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
					if len(pending) != 1 {
						return false
					}
					taskID, runID = pending[0].TaskID, pending[0].RunID
				} else {
					pending, _ := store.ListClarifyRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
					if len(pending) != 1 {
						return false
					}
					taskID, runID = pending[0].TaskID, pending[0].RunID
				}
				return countEvents(taskEvents(t, store, owner, taskID), "checkpoint.failed", `"outcome":"await_human"`, `"wait_kind":"`+kind+`"`) >= 1
			}, "the grace path did not run and report that it could not park")
			run, _ := store.GetRun(ctx, owner.TenantID, runID)
			if run.Status != "running" {
				t.Fatalf("an ask that could not be checkpointed left its Run: %+v", run)
			}
			if kind == control.LoopCheckpointWaitApproval {
				pending, _ := store.ListApprovalRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
				if pending[0].WaiterState != "live" {
					t.Fatalf("the approval left its waiter: %+v", pending[0])
				}
				if _, err := daemon.respondApprovalByToken(ctx, owner, pending[0].ID, "approved", "no-checkpoint", control.ApprovalDecisionInput{}); err != nil {
					t.Fatal(err)
				}
				waitUntil(t, 10*time.Second, func() bool {
					data, err := os.ReadFile(filepath.Join(ws.LocalPath, "receipt.txt"))
					return err == nil && string(data) == "approved effect" && daemon.coordinator().activeCount(owner.PersonID) == 0
				}, "the answer did not land in the Run that kept the ask")
			} else {
				pending, _ := store.ListClarifyRequests(ctx, owner.TenantID, owner.PersonID, "pending", 10)
				if pending[0].ParkedAt != nil {
					t.Fatalf("the question left its waiter: %+v", pending[0])
				}
				if _, err := daemon.answerClarifyTarget(ctx, owner, &pending[0], "json", "no-checkpoint"); err != nil {
					t.Fatal(err)
				}
				waitUntil(t, 10*time.Second, func() bool {
					current, _ := store.GetRun(ctx, owner.TenantID, runID)
					return current != nil && current.Status != "running" && daemon.coordinator().activeCount(owner.PersonID) == 0
				}, "the answer did not land in the Run that kept the question")
			}
			runs, _ := store.ListTaskRuns(ctx, owner.TenantID, taskID, 10)
			if len(runs) != 1 {
				t.Fatalf("the answer started another Run: %+v", runs)
			}
		})
	}
}

func startHeldQuestion(t *testing.T, daemon *Server, provider *humanWaitProvider, ws *control.Workspace, channel string) {
	t.Helper()
	provider.clarification = true
	attachHoldSession(t, daemon, ws.OwnerPersonID, channel)
	response, code := daemon.ProcessMessage(context.Background(), api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: channel, WorkspaceID: ws.ID, Content: "Choose the receipt format", Async: true})
	if code != 200 || !response.Accepted {
		t.Fatalf("admission: %d %+v", code, response)
	}
}

// questionParked waits for the question to release its worker.
func questionParked(t *testing.T, daemon *Server, store *control.Store, owner *control.IdentityContext, id string) *control.ClarifyRequest {
	t.Helper()
	var parked *control.ClarifyRequest
	waitUntil(t, 8*time.Second, func() bool {
		current, _ := store.GetClarifyRequest(context.Background(), owner.TenantID, id)
		run, _ := store.GetRun(context.Background(), owner.TenantID, current.RunID)
		if current.ParkedAt != nil && run != nil && run.Status == "waiting_user" {
			parked = current
			return true
		}
		return false
	}, "the held question did not release its worker")
	return parked
}

func TestHeldQuestionParksWhenOtherWorkNeedsTheSlot(t *testing.T) {
	ctx := context.Background()
	daemon, store, provider, owner := newHoldServer(t)
	daemon.coordinator().activeLimit = 1
	ws := humanWaitWorkspace(t, store, owner, "ask")
	startHeldQuestion(t, daemon, provider, ws, "ask")
	question, _ := waitHeldQuestion(t, store, owner)
	other := humanWaitWorkspace(t, store, owner, "other")
	// An explicit new task: a plain message would answer the pending question.
	response, code := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "other", WorkspaceID: other.ID, Content: "independent work", ForceNew: true, Async: true})
	if code != 200 || !response.Accepted || response.Turn == nil || response.Turn.Status != "queued" {
		t.Fatalf("the other work was not queued behind the held question: %d %+v", code, response)
	}
	questionParked(t, daemon, store, owner, question.ID)
	waitUntil(t, 8*time.Second, func() bool { return provider.independent.Load() == 1 }, "the queued work never got the slot the held question released")
	// The old path is intact: the late answer resumes it in another Run.
	if _, err := daemon.answerClarifyTarget(ctx, owner, question, "json", "ask"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		for _, event := range taskEvents(t, store, owner, question.TaskID) {
			if event.Type == "run.started" && event.RunID != question.RunID {
				return true
			}
		}
		return false
	}, "the late answer did not resume the parked question")
}

func TestHeldQuestionParksAtItsCap(t *testing.T) {
	daemon, store, provider, owner := newHoldServer(t)
	daemon.HumanWaitHold = 1200 * time.Millisecond
	ws := humanWaitWorkspace(t, store, owner, "ask-cap")
	started := time.Now()
	startHeldQuestion(t, daemon, provider, ws, "ask-cap")
	question, _ := waitHeldQuestion(t, store, owner)
	questionParked(t, daemon, store, owner, question.ID)
	if held := time.Since(started); held < time.Second {
		t.Fatalf("parked after %s, before its cap", held)
	}
}
