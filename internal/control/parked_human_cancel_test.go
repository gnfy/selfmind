package control

import (
	"context"
	"strings"
	"testing"
	"time"
)

func parkedHumanCancelFixture(t *testing.T, kind string) (*Store, *IdentityContext, *Task, *Run, string) {
	t.Helper()
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "human-cancel-owner")
	var ref string
	if kind == LoopCheckpointWaitApproval {
		ref = pendingApproval(t, store, owner, task, run).ID
	} else {
		ref = pendingClarification(t, store, owner, task, run).ID
	}
	ctx := context.Background()
	if err := store.SaveLoopCheckpoint(ctx, awaitCheckpoint(owner, task, run, kind, ref, "waiting-call")); err != nil {
		t.Fatal(err)
	}
	if parked, err := store.ParkHumanWaitRun(ctx, owner.TenantID, owner.PersonID, run.ID, kind, ref, HumanWaitParkGrace); err != nil || !parked {
		t.Fatalf("park=%v %v", parked, err)
	}
	return store, owner, task, run, ref
}

func answerHumanCancelFixture(t *testing.T, store *Store, owner *IdentityContext, task *Task, kind, ref string) *QueuedTask {
	t.Helper()
	ctx := context.Background()
	q := QueuedTask{TaskID: task.ID, Content: "approved", IdempotencyKey: "human-answer:" + ref}
	var queued *QueuedTask
	var err error
	if kind == LoopCheckpointWaitApproval {
		_, queued, err = store.RespondParkedApprovalAndEnqueue(ctx, owner.TenantID, owner.PersonID, ref, "approved", "cli", ApprovalDecisionInput{DecisionID: "once"}, q)
	} else {
		_, queued, err = store.AnswerClarifyRequestWithResume(ctx, owner.TenantID, owner.PersonID, ref, "staging", "cli", q)
	}
	if err != nil || queued == nil {
		t.Fatalf("answer=%+v %v", queued, err)
	}
	return queued
}

func TestHumanCancellationRollsBackRequestRunAndEvents(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		for _, fault := range []string{"request", "event"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				store, owner, _, run, ref := parkedHumanCancelFixture(t, kind)
				query := `CREATE TRIGGER reject_human_stop BEFORE INSERT ON task_events WHEN NEW.type='run.cancelled'
				 BEGIN SELECT RAISE(ABORT,'injected cancellation failure'); END`
				if fault == "request" {
					table := "approval_requests"
					if kind == LoopCheckpointWaitClarification {
						table = "clarify_requests"
					}
					query = `CREATE TRIGGER reject_human_stop BEFORE UPDATE ON ` + table + ` WHEN NEW.status='expired'
					 BEGIN SELECT RAISE(ABORT,'injected cancellation failure'); END`
				}
				if _, err := store.db.Exec(query); err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				if stopped, err := store.CancelParkedHumanRun(ctx, owner.TenantID, owner.PersonID, run.ID); stopped || err == nil || !strings.Contains(err.Error(), "injected cancellation failure") {
					t.Fatalf("failed tx reported cancellation: %v %v", stopped, err)
				}
				current, _ := store.GetRun(ctx, owner.TenantID, run.ID)
				requested, _ := store.RunCancelRequested(ctx, owner.TenantID, run.ID)
				if current.Status != "waiting_user" || requested {
					t.Fatalf("failed cancellation changed Run: %+v requested=%v", current, requested)
				}
				var status string
				if kind == LoopCheckpointWaitApproval {
					ask, _ := store.GetApprovalRequest(ctx, owner.TenantID, ref)
					status = ask.Status
				} else {
					ask, _ := store.GetClarifyRequest(ctx, owner.TenantID, ref)
					status = ask.Status
				}
				var events int
				if err := store.db.QueryRow(`SELECT COUNT(*) FROM task_events WHERE run_id=? AND type IN ('approval.expired','run.outcome','run.cancelled')`, run.ID).Scan(&events); err != nil || events != 0 || status != "pending" {
					t.Fatalf("failed cancellation left a half-transition: status=%s events=%d err=%v", status, events, err)
				}
				ids, err := store.ParkedHumanRunIDs(ctx, owner.TenantID, owner.PersonID)
				if err != nil || len(ids) != 1 || ids[0] != run.ID {
					t.Fatalf("failed cancellation lost its wait: %+v %v", ids, err)
				}
			})
		}
	}
}

func TestHumanCancellationRevokesUnstartedAnsweredContinuation(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		t.Run(kind, func(t *testing.T) {
			store, owner, task, run, ref := parkedHumanCancelFixture(t, kind)
			ctx := context.Background()
			q := answerHumanCancelFixture(t, store, owner, task, kind, ref)
			token, claimed, err := store.ClaimQueued(ctx, owner.TenantID, q.ID, time.Minute)
			if err != nil || !claimed {
				t.Fatalf("claim=%v %v", claimed, err)
			}
			if stopped, err := store.CancelParkedHumanRun(ctx, owner.TenantID, owner.PersonID, run.ID); err != nil || !stopped {
				t.Fatalf("answered unstarted wait was not cancelled: %v %v", stopped, err)
			}
			queued, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
			if queued.Status != QueueStatusCancelled || queued.ClaimToken != "" {
				t.Fatalf("leased answer survived stop: %+v", queued)
			}
			if child, err := store.StartRunWithOptions(ctx, task, run.Channel, "too late", StartRunOptions{ResumesRunID: run.ID, QueueID: q.ID, QueueClaimToken: token}); err == nil || child != nil {
				t.Fatalf("stale answer created a child: %+v %v", child, err)
			}
			if kind == LoopCheckpointWaitApproval {
				ask, _ := store.GetApprovalRequest(ctx, owner.TenantID, ref)
				if ask.Status != "approved" || ask.AuthorizationState != "" {
					t.Fatalf("stop rewrote the decision or retained its authorization: %+v", ask)
				}
			} else {
				ask, _ := store.GetClarifyRequest(ctx, owner.TenantID, ref)
				if ask.Status != "answered" || ask.Answer != "staging" {
					t.Fatalf("stop rewrote the human answer: %+v", ask)
				}
			}
		})
	}
}

func TestHumanCancellationEventFailureKeepsAnsweredContinuation(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		t.Run(kind, func(t *testing.T) {
			store, owner, task, run, ref := parkedHumanCancelFixture(t, kind)
			ctx := context.Background()
			q := answerHumanCancelFixture(t, store, owner, task, kind, ref)
			if _, err := store.db.Exec(`CREATE TRIGGER reject_answer_stop BEFORE INSERT ON task_events WHEN NEW.type='run.cancelled'
			 BEGIN SELECT RAISE(ABORT,'cannot record answered stop'); END`); err != nil {
				t.Fatal(err)
			}
			if stopped, err := store.CancelParkedHumanRun(ctx, owner.TenantID, owner.PersonID, run.ID); stopped || err == nil {
				t.Fatalf("failed transaction closed an answer: %v %v", stopped, err)
			}
			queued, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
			parent, _ := store.GetRun(ctx, owner.TenantID, run.ID)
			if queued.Status != QueueStatusQueued || parent.Status != "waiting_user" {
				t.Fatalf("failed transaction lost answered work: queue=%+v parent=%+v", queued, parent)
			}
			if kind == LoopCheckpointWaitApproval {
				ask, _ := store.GetApprovalRequest(ctx, owner.TenantID, ref)
				if ask.Status != "approved" || ask.AuthorizationState != "available" {
					t.Fatalf("failed cancellation revoked the answer: %+v", ask)
				}
			}
		})
	}
}

func TestHumanCancellationNeverStopsForeignOrClaimedWork(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		t.Run(kind, func(t *testing.T) {
			store, owner, task, run, ref := parkedHumanCancelFixture(t, kind)
			ctx := context.Background()
			if stopped, err := store.CancelParkedHumanRun(ctx, "different-tenant", owner.PersonID, run.ID); err != nil || stopped {
				t.Fatalf("cross-tenant cancellation: %v %v", stopped, err)
			}
			if stopped, err := store.CancelParkedHumanRun(ctx, owner.TenantID, "different-person", run.ID); err != nil || stopped {
				t.Fatalf("cross-person cancellation: %v %v", stopped, err)
			}
			q := answerHumanCancelFixture(t, store, owner, task, kind, ref)
			token, claimed, err := store.ClaimQueued(ctx, owner.TenantID, q.ID, time.Minute)
			if err != nil || !claimed {
				t.Fatalf("claim=%v %v", claimed, err)
			}
			child, err := store.StartRunWithOptions(ctx, task, run.Channel, "claimed child", StartRunOptions{ResumesRunID: run.ID, QueueID: q.ID, QueueClaimToken: token})
			if err != nil {
				t.Fatal(err)
			}
			if stopped, err := store.CancelParkedHumanRun(ctx, owner.TenantID, owner.PersonID, run.ID); err != nil || stopped {
				t.Fatalf("old parent stopped a claimed child: %v %v", stopped, err)
			}
			current, _ := store.GetRun(ctx, owner.TenantID, child.ID)
			queued, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
			if current.Status != "running" || queued.RunID != child.ID || queued.Status != QueueStatusStarted {
				t.Fatalf("claimed work was changed: child=%+v queue=%+v", current, queued)
			}
		})
	}
}

func TestHumanCancellationDoesNotReinterpretAnUnboundHistoricalWait(t *testing.T) {
	store, owner, _, run, ref := parkedHumanCancelFixture(t, LoopCheckpointWaitApproval)
	ctx := context.Background()
	if _, err := store.db.Exec(`DELETE FROM loop_checkpoints WHERE run_id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if stopped, err := store.CancelParkedHumanRun(ctx, owner.TenantID, owner.PersonID, run.ID); err != nil || stopped {
		t.Fatalf("historical waiting_user acquired new cancellation authority: %v %v", stopped, err)
	}
	ask, _ := store.GetApprovalRequest(ctx, owner.TenantID, ref)
	if ask.Status != "pending" {
		t.Fatalf("unbound historical request changed: %+v", ask)
	}
}

func TestConcurrentHumanStopAndChildCreationHaveOneDurableWinner(t *testing.T) {
	for _, kind := range []string{LoopCheckpointWaitApproval, LoopCheckpointWaitClarification} {
		t.Run(kind, func(t *testing.T) {
			for range 6 {
				store, owner, task, run, ref := parkedHumanCancelFixture(t, kind)
				ctx := context.Background()
				q := answerHumanCancelFixture(t, store, owner, task, kind, ref)
				token, claimed, err := store.ClaimQueued(ctx, owner.TenantID, q.ID, time.Minute)
				if err != nil || !claimed {
					t.Fatalf("claim=%v %v", claimed, err)
				}
				start := make(chan struct{})
				stops := make(chan bool, 1)
				children := make(chan *Run, 1)
				go func() {
					<-start
					stopped, _ := store.CancelParkedHumanRun(ctx, owner.TenantID, owner.PersonID, run.ID)
					stops <- stopped
				}()
				go func() {
					<-start
					child, _ := store.StartRunWithOptions(ctx, task, run.Channel, "race stop", StartRunOptions{ResumesRunID: run.ID, QueueID: q.ID, QueueClaimToken: token})
					children <- child
				}()
				close(start)
				stopped, child := <-stops, <-children
				if stopped == (child != nil) {
					t.Fatalf("stop/child had no single winner: stopped=%v child=%+v", stopped, child)
				}
				parent, _ := store.GetRun(ctx, owner.TenantID, run.ID)
				queue, _ := store.GetQueued(ctx, owner.TenantID, q.ID)
				if stopped && (parent.Status != "cancelled" || queue.Status != QueueStatusCancelled) {
					t.Fatalf("cancellation winner was partial: parent=%+v queue=%+v", parent, queue)
				}
				if child != nil && (parent.Status != "waiting_user" || queue.RunID != child.ID || queue.Status != QueueStatusStarted) {
					t.Fatalf("child winner lost its claim: parent=%+v queue=%+v", parent, queue)
				}
			}
		})
	}
}
