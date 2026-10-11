package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
)

func TestQueuedStartupNoticeRetriesIndependentlyOfExecutionCapacity(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	if _, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "notice-owner", "WeChat"); err != nil {
		t.Fatal(err)
	}
	row, err := s.store.EnqueueQueued(ctx, control.QueuedTask{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "origin-session", Content: "inspect report"})
	if err != nil {
		t.Fatal(err)
	}
	s.execRaw(t, `CREATE TRIGGER fail_start_notice BEFORE INSERT ON outbound_messages BEGIN SELECT RAISE(ABORT,'notice outbox fault'); END`)
	s.execRaw(t, `CREATE TRIGGER fail_run_creation BEFORE INSERT ON runs BEGIN SELECT RAISE(ABORT,'Run could not be created'); END`)
	s.daemon.coordinator().drainQueue(s.identity)
	waitUntil(t, time.Second, func() bool { return s.daemon.ActiveRunCount() == 0 }, "failed startup did not release slot")
	saved, err := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
	if err != nil || saved.Status != control.QueueStatusQueued {
		t.Fatalf("not retained: %+v %v", saved, err)
	}
	// A different, legitimate active Run occupies the work slot. Recovering
	// a notification must not require executing the queued input again.
	s.execRaw(t, `DROP TRIGGER fail_run_creation`)
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Title: "independent active work", Channel: "other-session"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.store.StartRun(ctx, task, "other-session", "independent task")
	if err != nil {
		t.Fatal(err)
	}
	active := &activeRun{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, RunID: run.ID, TaskID: task.ID, Platform: "cli", Channel: "other-session"}
	if !s.daemon.coordinator().beginActive(s.identity.PersonID, active) {
		t.Fatal("could not register independent active work")
	}
	defer s.daemon.coordinator().endActiveRun(s.identity.PersonID, active)
	s.execRaw(t, `DROP TRIGGER fail_start_notice`)
	s.daemon.runExternalWatchPass(ctx)
	s.daemon.runExternalWatchPass(ctx)
	events, err := s.store.ListPersonEventsAfter(ctx, s.identity.TenantID, s.identity.PersonID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	durable := 0
	for _, event := range events {
		if strings.Contains(string(event.Payload), "could not") {
			durable++
		}
	}
	counts, err := s.store.CountOutboundByStatusSince(ctx, s.identity.TenantID, s.identity.PersonID, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	pushes := 0
	for _, n := range counts {
		pushes += n
	}
	t.Logf("retained_status=%s durable_notices=%d notice_outbox_rows=%d", saved.Status, durable, pushes)
	if pushes != 1 {
		t.Fatal("a missed live notice and one outbox-write fault lost the startup reason permanently; periodic scheduler has no notice retry")
	}
	after, err := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
	if err != nil || after.AttemptGeneration != saved.AttemptGeneration || after.RunID != "" {
		t.Fatalf("notice retry executed the retained input: %+v, %v", after, err)
	}
}

func TestFailedQueuedStartupDoesNotStarveIndependentSources(t *testing.T) {
	for _, capacity := range []int{1, 2} {
		for _, returnWriteFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("capacity_%d_return_fault_%t", capacity, returnWriteFails), func(t *testing.T) {
				testFailedQueueSource(t, capacity, returnWriteFails)
			})
		}
	}
}

func testFailedQueueSource(t *testing.T, capacity int, returnWriteFails bool) {
	t.Helper()
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	if err := s.daemon.ConfigureWorkRunCapacity(capacity, capacity); err != nil {
		t.Fatal(err)
	}
	bad, err := s.store.EnqueueQueued(ctx, control.QueuedTask{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "broken-session", Content: "broken input"})
	if err != nil {
		t.Fatal(err)
	}
	later, err := s.store.EnqueueQueued(ctx, control.QueuedTask{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		Platform: bad.Platform, PlatformUserID: bad.PlatformUserID, Channel: bad.Channel, Content: "later input from the failed source"})
	if err != nil {
		t.Fatal(err)
	}
	if returnWriteFails {
		s.execRaw(t, fmt.Sprintf(`CREATE TRIGGER fail_source_return BEFORE UPDATE OF status ON task_queue
			WHEN OLD.id='%s' AND OLD.status='started' AND NEW.status='queued' BEGIN SELECT RAISE(ABORT,'source return failed'); END`, bad.ID))
	}
	good, err := s.store.EnqueueQueued(ctx, control.QueuedTask{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Platform: "cli", PlatformUserID: "local", Channel: "healthy-session", Content: "inspect independent report"})
	if err != nil {
		t.Fatal(err)
	}
	// Only the first input's Run insertion is unavailable; other Run creation,
	// model requests and tools are healthy.
	s.execRaw(t, `CREATE TRIGGER fail_one_input BEFORE INSERT ON runs WHEN NEW.input_summary='broken input' BEGIN SELECT RAISE(ABORT,'this input startup failed'); END`)
	for i := 0; i < 3; i++ {
		s.daemon.runExternalWatchPass(ctx)
		waitUntil(t, time.Second, func() bool { return s.daemon.ActiveRunCount() == 0 }, "startup held its active slot")
	}
	badSaved, _ := s.store.GetQueued(ctx, s.identity.TenantID, bad.ID)
	goodSaved, _ := s.store.GetQueued(ctx, s.identity.TenantID, good.ID)
	t.Logf("failed_input_status=%s attempts=%d healthy_input_status=%s attempts=%d run=%q", badSaved.Status, badSaved.AttemptGeneration, goodSaved.Status, goodSaved.AttemptGeneration, goodSaved.RunID)
	if goodSaved.RunID == "" || goodSaved.Status != control.QueueStatusDone || goodSaved.AttemptGeneration != 1 {
		t.Fatal("healthy work from another session starved behind one retained startup failure on every periodic tick")
	}
	laterSaved, err := s.store.GetQueued(ctx, s.identity.TenantID, later.ID)
	if err != nil || laterSaved.Status != control.QueueStatusQueued || laterSaved.AttemptGeneration != 0 {
		t.Fatalf("later input overtook the unstarted head of its source: %+v, %v", laterSaved, err)
	}
}

func TestQueuedHumanReplySurvivesParentClaimRaceAndReturnWriteFailure(t *testing.T) {
	s := newAdmissionProbeServer(t)
	// This case observes input retention and exact lineage; the fixture's
	// synthetic chmod approval is unrelated and would park the recovered Run.
	s.provider.skipApprovals = true
	ctx := context.Background()
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		Title: "unfinished work", Channel: "request-session"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.store.StartRun(ctx, task, task.LastChannel, "original work")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.FinishRun(ctx, s.identity.TenantID, parent.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	row, err := s.store.EnqueueQueued(ctx, control.QueuedTask{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		TaskID: task.ID, ReplyToRunID: parent.ID, Platform: "cli", PlatformUserID: "local", Channel: task.LastChannel,
		Content: "continue with my new correction"})
	if err != nil {
		t.Fatal(err)
	}
	s.execRaw(t, `CREATE TRIGGER refuse_reply_return BEFORE UPDATE OF status ON task_queue
		WHEN OLD.status='started' AND NEW.status='queued' BEGIN SELECT RAISE(ABORT,'injected reply-return fault'); END`)
	reads := 0
	var winner *control.Run
	s.daemon.coordinator().approvalModeRead = func(ctx context.Context, _, _ string) (string, int64, error) {
		reads++
		// The first read precedes queue claim; the second freezes admission
		// after exact-parent resolution and before atomic child creation.
		if reads == 2 {
			winner, err = s.store.StartRunWithOptions(ctx, task, task.LastChannel, "competing continuation", control.StartRunOptions{ResumesRunID: parent.ID})
			if err != nil {
				return "", 0, err
			}
			if err := s.store.FinishRun(ctx, s.identity.TenantID, winner.ID, "waiting_user"); err != nil {
				return "", 0, err
			}
		}
		return "smart", 0, nil
	}
	s.daemon.coordinator().drainQueue(s.identity)
	if winner == nil || err != nil {
		t.Fatalf("parent-claim race did not occur: %+v, %v", winner, err)
	}
	saved, err := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
	if err != nil || saved.Status != control.QueueStatusStarted || saved.RunID != "" || saved.Content != row.Content || saved.AttemptGeneration != 1 {
		t.Fatalf("unstarted human correction was consumed: %+v, %v", saved, err)
	}
	s.daemon.coordinator().approvalModeRead = nil
	s.execRaw(t, `DROP TRIGGER refuse_reply_return`)
	s.daemon.runExternalWatchPass(ctx)
	waitUntil(t, 3*time.Second, func() bool {
		saved, err = s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
		return err == nil && saved.Status == control.QueueStatusDone && saved.RunID != "" && s.daemon.ActiveRunCount() == 0
	}, "the retained correction was not recovered on its exact lineage")
	recovered, err := s.store.GetRun(ctx, s.identity.TenantID, saved.RunID)
	if err != nil || recovered.ResumesRunID != winner.ID || saved.AttemptGeneration != 2 {
		t.Fatalf("the correction lost its continuation or was duplicated: %+v, %v", recovered, err)
	}
}

func TestQueuedAmbiguousContinuationRequiresSavedChoiceBeforeSettlement(t *testing.T) {
	for _, fault := range []string{"", "choice", "notice", "handoff", "outbox"} {
		t.Run("write_fault_"+fault, func(t *testing.T) {
			writeFails := fault != ""
			s := newAdmissionProbeServer(t)
			ctx := context.Background()
			task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
				Title: "two unfinished lines", Channel: "origin-session"})
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"unfinished first line", "unfinished second line"} {
				run, err := s.store.StartRun(ctx, task, task.LastChannel, text)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.store.FinishRun(ctx, s.identity.TenantID, run.ID, "interrupted"); err != nil {
					t.Fatal(err)
				}
			}
			row, err := s.store.EnqueueQueued(ctx, control.QueuedTask{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
				Platform: "cli", PlatformUserID: "local", Channel: task.LastChannel, TaskID: task.ID,
				Content: "continue and include my new requirement"})
			if err != nil {
				t.Fatal(err)
			}
			owner, closeOwner := s.daemon.events().subscribe(s.identity.PersonID, row.Channel, "")
			defer closeOwner()
			other, closeOther := s.daemon.events().subscribe(s.identity.PersonID, "other-session", "")
			defer closeOther()
			if fault == "choice" {
				s.execRaw(t, `CREATE TRIGGER fail_candidate_choice BEFORE INSERT ON pending_turn_choices
					BEGIN SELECT RAISE(ABORT,'choice write unavailable'); END`)
			} else if fault == "notice" {
				s.execRaw(t, `CREATE TRIGGER fail_candidate_choice BEFORE INSERT ON task_events
					WHEN NEW.type='continuation.candidates' BEGIN SELECT RAISE(ABORT,'choice notice unavailable'); END`)
			} else if fault == "handoff" {
				s.execRaw(t, `CREATE TRIGGER fail_candidate_choice BEFORE UPDATE OF status ON task_queue
					WHEN OLD.status='started' AND NEW.status='done' BEGIN SELECT RAISE(ABORT,'choice handoff unavailable'); END`)
			} else if fault == "outbox" {
				if _, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "choice-owner", "WeChat"); err != nil {
					t.Fatal(err)
				}
				s.execRaw(t, `CREATE TRIGGER fail_choice_outbox BEFORE INSERT ON outbound_messages
					BEGIN SELECT RAISE(ABORT,'choice outbox unavailable'); END`)
			}
			s.daemon.coordinator().drainQueue(s.identity)
			endpoint := control.TurnChoiceEndpoint{AccountID: s.identity.AccountID, Channel: row.Channel}
			if writeFails {
				saved, err := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
				if err != nil || saved.Status != control.QueueStatusQueued || saved.RunID != "" || saved.Content != row.Content || saved.AttemptGeneration != 1 {
					t.Fatalf("unsaved choice consumed the original input: %+v, %v", saved, err)
				}
				if _, err := s.store.PeekBareTurnChoice(ctx, s.identity.TenantID, s.identity.PersonID, endpoint, time.Now(), turnChoiceBareWindow); !errors.Is(err, control.ErrTurnChoiceNotFound) {
					t.Fatalf("failed choice insert left an answerable record: %v", err)
				}
				if fault == "outbox" {
					s.execRaw(t, `DROP TRIGGER fail_choice_outbox`)
				} else {
					s.execRaw(t, `DROP TRIGGER fail_candidate_choice`)
				}
				if fault == "outbox" || fault == "handoff" {
					// Drop process-local retry state and run the real boot drain.
					// The original durable input must create one complete handoff.
					s.daemon = &Server{Control: s.store, Gateway: s.daemon.Gateway, Delivery: s.daemon.Delivery, DefaultTenantID: "default", ApprovalJudge: s.daemon.ApprovalJudge}
					s.daemon.DrainQueuedAtBoot(ctx)
				}
				s.daemon.runExternalWatchPass(ctx)
			}
			waitUntil(t, time.Second, func() bool { return s.daemon.ActiveRunCount() == 0 }, "choice handoff held execution capacity")
			saved, err := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
			generation := 1
			if writeFails {
				generation = 2
			}
			if err != nil || saved.Status != control.QueueStatusDone || saved.RunID != "" || saved.AttemptGeneration != generation {
				t.Fatalf("saved choice did not accept the original input exactly once: %+v, %v", saved, err)
			}
			choice, err := s.store.PeekBareTurnChoice(ctx, s.identity.TenantID, s.identity.PersonID, endpoint, time.Now(), turnChoiceBareWindow)
			if err != nil || !strings.Contains(choice.RequestJSON, row.Content) || len(choice.Options) != 3 {
				t.Fatalf("handoff has no complete durable input and candidate list: %+v, %v", choice, err)
			}
			visible := 0
			for len(owner.ch) > 0 {
				event := <-owner.ch
				if event.Type != "continuation.candidates" {
					continue
				}
				var payload struct {
					ChoiceID string `json:"choice_id"`
					Question string `json:"question"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.ChoiceID != choice.ID ||
					!strings.Contains(payload.Question, "1.") || !strings.Contains(payload.Question, "3. This is new work") {
					t.Fatalf("choice handoff was not actionable on its source: %+v", event)
				}
				visible++
			}
			if visible != 1 {
				t.Fatalf("source received %d complete choice notices, want one", visible)
			}
			if len(other.ch) != 0 {
				t.Fatal("choice question leaked to another session")
			}
			events, err := s.store.ListPersonEventsAfter(ctx, s.identity.TenantID, s.identity.PersonID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			replayed := 0
			for _, event := range events {
				if event.Type == "continuation.candidates" {
					if got, ok := owner.view(durableRunEvent(event)); !ok || got.Channel != row.Channel || !strings.Contains(string(got.Payload), choice.ID) {
						t.Fatal("choice question cannot replay on its original source")
					}
					if _, ok := other.view(durableRunEvent(event)); ok {
						t.Fatal("choice replay leaked to another source")
					}
					replayed++
				}
			}
			if replayed != 1 {
				t.Fatalf("choice notice committed %d times, want one", replayed)
			}
			if fault == "outbox" {
				s.daemon.runExternalWatchPass(ctx)
				s.daemon.runExternalWatchPass(ctx)
				counts, err := s.store.CountOutboundByStatusSince(ctx, s.identity.TenantID, s.identity.PersonID, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				pushes := 0
				for _, count := range counts {
					pushes += count
				}
				if pushes != 1 {
					t.Fatalf("choice notice was lost or enqueued repeatedly: %v", counts)
				}
				after, err := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
				if err != nil || after.AttemptGeneration != generation || after.RunID != "" {
					t.Fatalf("notification retry reexecuted input: %+v, %v", after, err)
				}
			}
			s.provider.mu.Lock()
			calls := len(s.provider.probes)
			s.provider.mu.Unlock()
			if calls != 0 {
				t.Fatal("ambiguous continuation dispatched a model or guessed a parent")
			}
		})
	}
}
