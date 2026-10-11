package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func TestObsoleteApprovalContinuationDoesNotStarveLaterInput(t *testing.T) {
	provider := newSlowLLMProvider("Checked the independent request.")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	t.Cleanup(func() { daemon.beginDraining("test cleanup") })
	ctx := context.Background()
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "unfinished work", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.StartRun(ctx, task, "cli", "prepare an operation")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	// A person-authored continuation wins the parent's one-child claim while
	// the exact approval wakeup is still queued. That wakeup cannot be rebased.
	child, err := store.StartRunWithOptions(ctx, task, "cli", "a later correction", control.StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, child.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	old, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID,
		Platform: "cli", PlatformUserID: "local", Channel: "cli", TaskID: task.ID, ReplyToRunID: parent.ID,
		Class: control.QueueClassFinalization, IdempotencyKey: "approval-resume:old-action", Content: "The pending action was approved."})
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID,
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Class: control.QueueClassFinalization,
		Content: "inspect the independent report"})
	if err != nil {
		t.Fatal(err)
	}
	daemon.coordinator().drainQueue(owner)
	waitUntil(t, time.Second, func() bool {
		row, err := store.GetQueued(ctx, owner.TenantID, next.ID)
		return err == nil && row.RunID != "" && row.Status == control.QueueStatusDone && daemon.ActiveRunCount() == 0
	}, "obsolete approval continuation kept later input queued")
	row, err := store.GetQueued(ctx, owner.TenantID, old.ID)
	if err != nil || row.Status != control.QueueStatusFailed || row.RunID != "" || row.AttemptGeneration > 1 {
		t.Fatalf("obsolete wakeup must end without execution or repeated claims: %+v, %v", row, err)
	}
	unchanged, err := store.GetRun(ctx, owner.TenantID, child.ID)
	if err != nil || unchanged.Status != "waiting_user" || unchanged.ResumesRunID != parent.ID {
		t.Fatalf("queue retirement changed unfinished work: %+v, %v", unchanged, err)
	}
	if !hasEventOfType(t, store, task.ID, "queue.continuation_obsolete") {
		t.Fatal("the failed continuation left no durable explanation")
	}
}

func TestObsoleteContinuationRetainsDecisionUntilItsNoticeIsDurable(t *testing.T) {
	for _, fault := range []struct{ name, trigger string }{
		{"event", `BEFORE INSERT ON task_events WHEN NEW.type='queue.continuation_obsolete'`},
		{"outbox", `BEFORE INSERT ON outbound_messages`},
		{"settlement", `BEFORE UPDATE ON task_queue WHEN NEW.status='failed'`},
	} {
		t.Run(fault.name, func(t *testing.T) {
			s := newAdmissionProbeServer(t)
			ctx := context.Background()
			if _, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "wx-owner", "WeChat"); err != nil {
				t.Fatal(err)
			}
			task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Title: "unfinished operation", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			parent, err := s.store.StartRun(ctx, task, "cli", "prepare operation")
			if err != nil {
				t.Fatal(err)
			}
			approval, err := s.store.CreateApprovalRequest(ctx, control.ApprovalRequest{
				TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, TaskID: task.ID, RunID: parent.ID,
				ActionType: "tool_call", AuthorizationFingerprint: "resume:v1:exact-operation",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.store.ParkApprovalRequest(ctx, s.identity.TenantID, approval.ID, "wait ended"); err != nil {
				t.Fatal(err)
			}
			if err := s.store.FinishRun(ctx, s.identity.TenantID, parent.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			_, queued, err := s.store.RespondParkedApprovalAndEnqueue(ctx, s.identity.TenantID, s.identity.PersonID, approval.ID, "approved", "cli",
				control.ApprovalDecisionInput{DecisionID: "once"}, control.QueuedTask{
					PersonID: s.identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "cli",
					TaskID: task.ID, ReplyToRunID: parent.ID, Class: control.QueueClassFinalization,
					Content: "continue the approved operation", IdempotencyKey: "approval-resume:" + approval.ID,
				})
			if err != nil {
				t.Fatal(err)
			}
			child, err := s.store.StartRunWithOptions(ctx, task, "cli", "a correction", control.StartRunOptions{ResumesRunID: parent.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.store.FinishRun(ctx, s.identity.TenantID, child.ID, "waiting_user"); err != nil {
				t.Fatal(err)
			}
			s.execRaw(t, `CREATE TRIGGER refuse_obsolete_notice `+fault.trigger+` BEGIN SELECT RAISE(ABORT,'injected continuation failure'); END`)
			s.daemon.coordinator().drainQueue(s.identity)
			row, err := s.store.GetQueued(ctx, s.identity.TenantID, queued.ID)
			if err != nil || row.Status != control.QueueStatusQueued || row.AttemptGeneration != 0 || row.RunID != "" {
				t.Fatalf("notice failure lost or dispatched input: %+v, %v", row, err)
			}
			s.execRaw(t, `DROP TRIGGER refuse_obsolete_notice`)
			if fault.name == "settlement" {
				token, claimed, err := s.store.ClaimQueued(ctx, s.identity.TenantID, queued.ID, time.Minute)
				if err != nil || !claimed {
					t.Fatalf("claim: %v, %v", claimed, err)
				}
				if !s.daemon.coordinator().handleObsoleteQueueClaim(ctx, s.identity, api.MessageRequest{QueueID: queued.ID, QueueClaimToken: token}) {
					t.Fatal("a parent changed at atomic child creation was not retired")
				}
			}
			s.daemon.coordinator().drainQueue(s.identity)
			s.daemon.coordinator().drainQueue(s.identity)
			row, err = s.store.GetQueued(ctx, s.identity.TenantID, queued.ID)
			if err != nil || row.Status != control.QueueStatusFailed || row.RunID != "" {
				t.Fatalf("recovered notice did not retire the unbound wakeup: %+v, %v", row, err)
			}
			saved, err := s.store.GetApprovalRequest(ctx, s.identity.TenantID, approval.ID)
			if err != nil || saved.Status != "approved" || saved.AuthorizationState != "available" || saved.ClaimedByRunID != "" {
				t.Fatalf("retirement changed the person's exact decision: %+v, %v", saved, err)
			}
			events, err := s.store.ListTaskEvents(ctx, task.ID, 50)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, event := range events {
				if event.Type == "queue.continuation_obsolete" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("durable explanations=%d, want one", count)
			}
			pushes := func() int {
				count := 0
				for _, message := range s.sender.snapshot() {
					if strings.Contains(message.Content, "A queued continuation did not start:") {
						count++
					}
				}
				return count
			}
			waitUntil(t, time.Second, func() bool { return pushes() == 1 }, "the retained continuation notice was never delivered")
		})
	}
}

func TestObsoleteNoticeReachesAttachedCLIBeforeSettlement(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	if _, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "wx-owner", "WeChat"); err != nil {
		t.Fatal(err)
	}
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Title: "unfinished operation", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.store.StartRun(ctx, task, "cli", "prepare operation")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.FinishRun(ctx, s.identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	child, err := s.store.StartRunWithOptions(ctx, task, "cli", "a correction", control.StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.FinishRun(ctx, s.identity.TenantID, child.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	queued, err := s.store.EnqueueQueued(ctx, control.QueuedTask{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "cli", TaskID: task.ID, ReplyToRunID: parent.ID, Class: control.QueueClassFinalization, IdempotencyKey: "approval-resume:notice-review", Content: "continue the approved operation"})
	if err != nil {
		t.Fatal(err)
	}
	own, closeOwn := s.daemon.events().subscribe(s.identity.PersonID, "cli", "")
	defer closeOwn()
	other, closeOther := s.daemon.events().subscribe(s.identity.PersonID, "other-cli", "")
	defer closeOther()
	s.daemon.presenceTracker().Touch(s.identity.PersonID, "cli")
	s.execRaw(t, `CREATE TRIGGER refuse_obsolete_settlement BEFORE UPDATE ON task_queue WHEN NEW.status='failed' BEGIN SELECT RAISE(ABORT,'injected settlement failure'); END`)
	s.daemon.coordinator().drainQueue(s.identity)
	row, err := s.store.GetQueued(ctx, s.identity.TenantID, queued.ID)
	if err != nil || row.Status != control.QueueStatusQueued {
		t.Fatalf("unexpected row: %+v, %v", row, err)
	}
	drain := func(sub *runEventSubscriber) []api.RunEvent {
		var got []api.RunEvent
		for {
			select {
			case e := <-sub.ch:
				got = append(got, e)
			default:
				return got
			}
		}
	}
	ownEvents, otherEvents := drain(own), drain(other)
	waitUntil(t, time.Second, func() bool { return len(s.sender.snapshot()) == 1 }, "the durable notice lacked its push fallback")
	events, err := s.store.ListTaskEvents(ctx, task.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	durable := 0
	for _, e := range events {
		if e.Type == "queue.continuation_obsolete" {
			durable++
		}
	}
	if durable != 1 {
		t.Fatalf("notification record was duplicated: records=%d", durable)
	}
	for _, received := range [][]api.RunEvent{ownEvents, otherEvents} {
		if len(received) != 1 || received[0].Type != "background.notice" || received[0].Durability != api.EventDurable || received[0].EventID == "" || received[0].Cursor == 0 {
			t.Fatalf("attached CLI missed the durable notice before settlement: %+v", received)
		}
		if !strings.Contains(string(received[0].Payload), "/resume "+child.ID) {
			t.Fatalf("bounded notice lost its exact inspection action: %s", received[0].Payload)
		}
	}
	// Both a failed settlement retry and a later successful one are silent:
	// the already-committed notice is not replaced with a second live message.
	s.daemon.coordinator().drainQueue(s.identity)
	s.execRaw(t, `DROP TRIGGER refuse_obsolete_settlement`)
	s.daemon.coordinator().drainQueue(s.identity)
	row, err = s.store.GetQueued(ctx, s.identity.TenantID, queued.ID)
	if err != nil || row.Status != control.QueueStatusFailed || row.RunID != "" {
		t.Fatalf("retirement did not settle without executing: %+v, %v", row, err)
	}
	if len(drain(own)) != 0 || len(drain(other)) != 0 {
		t.Fatal("retry delivered the already-committed notice again")
	}
	if pushes := len(s.sender.snapshot()); pushes != 1 {
		t.Fatalf("retry duplicated the keyed push: %d", pushes)
	}
}

func TestObsoleteNoticeRetryKeepsPushForNewlyAttachedCLI(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	if _, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "wx-owner", "WeChat"); err != nil {
		t.Fatal(err)
	}
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Title: "unfinished operation", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.store.StartRun(ctx, task, "cli", "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.store.FinishRun(ctx, s.identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	child, err := s.store.StartRunWithOptions(ctx, task, "cli", "a correction", control.StartRunOptions{ResumesRunID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.store.FinishRun(ctx, s.identity.TenantID, child.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	q, err := s.store.EnqueueQueued(ctx, control.QueuedTask{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "cli", TaskID: task.ID, ReplyToRunID: parent.ID, Class: control.QueueClassFinalization, IdempotencyKey: "approval-resume:late-cli", Content: "continue approved operation"})
	if err != nil {
		t.Fatal(err)
	}
	// No CLI is attached. The event commits but push receipt fails.
	s.execRaw(t, `CREATE TRIGGER obsolete_outbox_fault BEFORE INSERT ON outbound_messages BEGIN SELECT RAISE(ABORT,'injected outbox failure'); END`)
	s.daemon.coordinator().drainQueue(s.identity)
	first, err := s.store.GetQueued(ctx, s.identity.TenantID, q.ID)
	if err != nil || first.Status != control.QueueStatusQueued {
		t.Fatalf("first failure did not retain input: %+v %v", first, err)
	}
	events, err := s.store.ListTaskEvents(ctx, task.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.Type == "queue.continuation_obsolete" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected prior durable notice: %d", count)
	}
	// A fresh CLI defaults to LatestPersonEventCursor, not historical replay.
	// Therefore subscribing now carries only new appends, like its live stream.
	own, closeOwn := s.daemon.events().subscribe(s.identity.PersonID, "cli", "")
	defer closeOwn()
	s.daemon.presenceTracker().Touch(s.identity.PersonID, "cli")
	s.execRaw(t, `DROP TRIGGER obsolete_outbox_fault`)
	s.daemon.coordinator().drainQueue(s.identity)
	after, err := s.store.GetQueued(ctx, s.identity.TenantID, q.ID)
	if err != nil || after.Status != control.QueueStatusFailed {
		t.Fatalf("retry not settled: %+v %v", after, err)
	}
	// The saved event is not newly published to this client, so its presence
	// cannot suppress the keyed fallback. Recovery must not execute any Run.
	select {
	case e := <-own.ch:
		t.Fatalf("idempotent event unexpectedly republished: %+v", e)
	default:
	}
	waitUntil(t, time.Second, func() bool { return len(s.sender.snapshot()) == 1 }, "the new CLI silenced the saved notice's push fallback")
	s.daemon.coordinator().drainQueue(s.identity)
	if after.RunID != "" || s.daemon.ActiveRunCount() != 0 || len(s.sender.snapshot()) != 1 {
		t.Fatal("notice retry executed work or duplicated the push")
	}
}
