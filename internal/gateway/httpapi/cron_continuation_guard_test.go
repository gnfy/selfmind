package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel/task/cron"
)

// The schedule check belongs to a scheduled fire that has not started. Work a
// schedule already started is underway, and everything the daemon queues to
// continue it — a provider wait, an answered approval or question, a watcher's
// result, a recovery — copies the schedule's snapshot and carries the person's
// decisions. Deleting or switching off the schedule afterwards must neither
// cancel those rows nor discard an answer the person already gave.

// scheduledParent is the snapshot of a Run a schedule started.
func scheduledParent(request string) control.AdmissionSnapshot {
	admitted := webAdmission("smart", request)
	admitted.Origin = control.AdmissionOrigin{Kind: runOriginCron, ID: "7"}
	admitted.StandingGrantsAuthorizedAt = 1790000000
	return admitted
}

func continuationRowsOfScheduledWork(t *testing.T, s admissionProbeServer, rows map[string]control.QueuedTask) {
	t.Helper()
	ctx := context.Background()
	const request = "tidy the nightly notes"
	parked := func(admission control.AdmissionSnapshot) (*control.Task, *control.Run) {
		task, run := s.parkedRunForHuman(t, admission, request)
		return task, run
	}
	row := func(key string) control.QueuedTask {
		got, err := s.store.GetQueuedByIdempotencyKey(ctx, s.identity.TenantID, key)
		if err != nil || got == nil {
			t.Fatalf("queue row %s: %+v %v", key, got, err)
		}
		if got.Admission.Origin.Kind != runOriginCron {
			t.Fatalf("fixture: %s must copy the schedule's snapshot: %+v", key, got.Admission)
		}
		return *got
	}

	// An answered approval.
	task, run := parked(scheduledParent(request))
	approval, err := s.store.CreateApprovalRequest(ctx, control.ApprovalRequest{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		TaskID: task.ID, RunID: run.ID, ActionType: "tool_call", AuthorizationFingerprint: "resume:v1:probe"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.store.ParkApprovalRequest(ctx, s.identity.TenantID, approval.ID, "waiter gone"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.FinishRun(ctx, s.identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.daemon.respondApprovalByToken(ctx, s.identity, approval.ID, "approved", "cli", control.ApprovalDecisionInput{DecisionID: "once"}); err != nil {
		t.Fatal(err)
	}
	rows["approval resume"] = row("approval-resume:" + approval.ID)

	// An answered question.
	task, run = parked(scheduledParent(request))
	clarify, err := s.store.CreateClarifyRequest(ctx, control.ClarifyRequest{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Question: "Which notes?", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.FinishRun(ctx, s.identity.TenantID, run.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.daemon.answerClarifyTarget(ctx, s.identity, clarify, "the nightly ones", "cli"); err != nil {
		t.Fatal(err)
	}
	rows["clarify resume"] = row("clarify-resume:" + clarify.ID)

	// A watcher's result.
	task, run = parked(scheduledParent(request))
	if err := s.store.FinishRun(ctx, s.identity.TenantID, run.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	watch, err := s.store.CreateExternalWatch(ctx, control.ExternalWatch{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Channel: "cli", Description: "the export is published", CWD: t.TempDir(),
		Command: "true", SuccessPattern: "SUCCESS", TimeoutAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if finished, err := s.store.FinishExternalWatch(ctx, watch.TenantID, watch.ID, control.ExternalWatchSucceeded, "SUCCESS\n", ""); err != nil || !finished {
		t.Fatalf("finish watch = %v %v", finished, err)
	}
	watch, _ = s.store.GetExternalWatch(ctx, s.identity.TenantID, watch.ID)
	if err := s.daemon.enqueueExternalWatchFinalization(ctx, *watch, s.identity, "export published"); err != nil {
		t.Fatal(err)
	}
	rows["watcher continuation"] = row(externalWatchFinalizationKey(*watch))

	// A recovery of an interrupted Run.
	recovery := newAdmissionProbeServer(t)
	_, key := recovery.interruptedForRecovery(t, scheduledParent(request))
	recovered, err := recovery.store.GetQueuedByIdempotencyKey(ctx, recovery.identity.TenantID, key)
	if err != nil || recovered == nil {
		t.Fatalf("recovery row: %+v %v", recovered, err)
	}
	rows["recovery"] = *recovered

	// An external resource wake.
	task, run = parked(scheduledParent(request))
	if err := s.store.FinishRun(ctx, s.identity.TenantID, run.ID, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	if err := s.daemon.enqueueExternalResourceWake(ctx, control.ExternalResourceWait{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		RunID: run.ID, EffectID: "effect-cron"}, "waiting_external"); err != nil {
		t.Fatal(err)
	}
	rows["external resource"] = row("external-resource:" + run.ID + ":effect-cron")
	_ = task
}

func TestContinuationsOfScheduledWorkAreNotDroppedWithTheirSchedule(t *testing.T) {
	s := newAdmissionProbeServer(t)
	rows := map[string]control.QueuedTask{}
	continuationRowsOfScheduledWork(t, s, rows)
	for name, row := range rows {
		for _, state := range []struct {
			name string
			job  func(f *fakeCronJobs)
		}{
			{"schedule deleted", func(f *fakeCronJobs) { f.remove(7) }},
			{"schedule switched off", func(f *fakeCronJobs) {
				f.set(cron.CronJob{ID: 7, Enabled: false})
			}},
		} {
			t.Run(name+"/"+state.name, func(t *testing.T) {
				jobs := &fakeCronJobs{jobs: map[int64]cron.CronJob{7: {ID: 7, TenantID: s.identity.TenantID, Enabled: true}}}
				state.job(jobs)
				s.daemon.CronJobs = jobs
				verdict, why, err := s.daemon.queuedCronFire(context.Background(), row)
				if verdict != cronFireRuns || err != nil {
					t.Fatalf("a %s row of scheduled work was %v (%q) when %s: it is underway and must run", name, verdict, why, state.name)
				}
			})
		}
	}

	// The constraint that must change the result: a fresh fire that has not
	// started is still dropped with its schedule.
	fresh := newAdmissionProbeServer(t)
	freshRow, freshJobs, _ := queuedFire(t, fresh, cron.CronJob{ID: 7, Enabled: true})
	freshJobs.remove(7)
	if verdict, _, _ := fresh.daemon.queuedCronFire(context.Background(), freshRow); verdict != cronFireDropped {
		t.Fatalf("a fresh scheduled fire whose schedule was deleted must still be dropped, got %v", verdict)
	}
}

// The reviewer's probe, end to end: a scheduled Run that parks on the provider
// continues after its schedule is deleted, and the person is not told it never
// started.
func TestScheduledRunParkedOnTheProviderResumesAfterItsScheduleIsDeleted(t *testing.T) {
	s := newAdmissionProbeServer(t, 1)
	s.provider.skipApprovals = true
	ctx := context.Background()
	if _, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "wx-owner", "WeChat"); err != nil {
		t.Fatal(err)
	}
	job := cron.CronJob{ID: 5, TenantID: s.identity.TenantID, CreatorPersonID: s.identity.PersonID, Platform: "cli", DeliverTo: "local", Channel: "session-cron",
		Prompt: "tidy the nightly notes", Enabled: true}
	jobs := &fakeCronJobs{jobs: map[int64]cron.CronJob{5: job}}
	s.daemon.CronJobs = jobs
	if err := NewCronExecutor(s.daemon, nil).RunCronJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	digests, err := s.store.ListRecentRunsForPerson(ctx, s.identity.TenantID, s.identity.PersonID, 10)
	if err != nil || len(digests) != 1 {
		t.Fatalf("scheduled runs = %+v %v", digests, err)
	}
	parentID := digests[0].RunID

	jobs.remove(5)
	child := s.drainDue(t, "provider-wait:"+parentID)
	if child == nil || child.ResumesRunID != parentID {
		t.Fatalf("the continuation of the parked scheduled Run did not run: %+v", child)
	}
	// Historical attempts retain their parked outcome. The exact child, not
	// rewriting its parent, proves the work resumed and finished.
	if child.Status != "done" {
		t.Fatalf("the scheduled continuation did not finish: %+v", child)
	}
	for _, msg := range s.sender.snapshot() {
		if strings.Contains(msg.Content, "did not start") {
			t.Fatalf("the person was told a Run that already started never started: %q", msg.Content)
		}
	}
}
