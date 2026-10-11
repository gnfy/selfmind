package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/task/cron"
)

// fakeCronJobs is the schedule store as the drain sees it.
type fakeCronJobs struct {
	mu    sync.Mutex
	jobs  map[int64]cron.CronJob
	err   error
	calls int
}

func (f *fakeCronJobs) GetJob(_ context.Context, tenantID string, id int64) (*cron.CronJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	job, ok := f.jobs[id]
	if !ok || job.TenantID != tenantID {
		return nil, cron.ErrJobNotFound
	}
	return &job, nil
}

// set changes a stored job's state; it keeps the tenant the job was stored with.
func (f *fakeCronJobs) set(job cron.CronJob) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if prev, ok := f.jobs[job.ID]; ok {
		job.TenantID = prev.TenantID
	}
	f.jobs[job.ID] = job
}

func (f *fakeCronJobs) remove(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.jobs, id)
}

func (f *fakeCronJobs) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeCronJobs) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// queuedFire queues a scheduled fire behind busy work and returns its row, the
// schedule store the drain will ask, and a drain that frees the person's slot
// first.
func queuedFire(t *testing.T, s admissionProbeServer, job cron.CronJob) (control.QueuedTask, *fakeCronJobs, func()) {
	t.Helper()
	ctx := context.Background()
	// A stricter mode would ask a person about the probe's dangerous command.
	s.provider.skipApprovals = true
	if _, err := s.store.BindAccount(ctx, s.identity.TenantID, s.identity.PersonID, "weixin", "wx-owner", "WeChat"); err != nil {
		t.Fatal(err)
	}
	coord := s.daemon.coordinator()
	if !coord.beginActive(s.identity.PersonID, &activeRun{TaskID: "other-task"}) {
		t.Fatal("failed to occupy the person's slot")
	}
	job.TenantID, job.Platform, job.DeliverTo, job.Channel = s.identity.TenantID, "cli", "local", "session-cron"
	if job.Prompt == "" {
		job.Prompt = "tidy the nightly notes"
	}
	if err := NewCronExecutor(s.daemon, nil).RunCronJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	rows, err := s.store.ListQueued(ctx, s.identity.TenantID, s.identity.PersonID, control.QueueStatusQueued)
	if err != nil || len(rows) != 1 || rows[0].Admission.Origin.Kind != runOriginCron {
		t.Fatalf("fixture: expected one queued scheduled fire: %+v %v", rows, err)
	}
	store := &fakeCronJobs{jobs: map[int64]cron.CronJob{job.ID: job}}
	s.daemon.CronJobs = store
	return rows[0], store, func() {
		coord.endActive(s.identity.PersonID)
		coord.drainQueue(s.identity)
	}
}

func (s admissionProbeServer) queueRow(t *testing.T, id string) *control.QueuedTask {
	t.Helper()
	row, err := s.store.GetQueued(context.Background(), s.identity.TenantID, id)
	if err != nil || row == nil {
		t.Fatalf("queue row %s: %+v %v", id, row, err)
	}
	return row
}

func (s admissionProbeServer) droppedNotices() int {
	count := 0
	for _, msg := range s.sender.snapshot() {
		if strings.Contains(msg.Content, "did not start") && strings.Contains(msg.Content, "scheduled run") {
			count++
		}
	}
	return count
}

func (s admissionProbeServer) modelCalls() int {
	s.provider.mu.Lock()
	defer s.provider.mu.Unlock()
	return len(s.provider.probes)
}

// A scheduled fire that waited in the queue while its schedule was deleted or
// switched off does not run: it was authorized by a schedule that no longer
// exists to authorize it. The person is told once and the row is cancelled.
func TestQueuedScheduledFireIsDroppedWhenItsScheduleIsGoneOrOff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(f *fakeCronJobs, job cron.CronJob)
		wantWhy string
	}{
		{"deleted", func(f *fakeCronJobs, job cron.CronJob) { f.remove(job.ID) }, "was deleted"},
		{"switched off, recurring", func(f *fakeCronJobs, job cron.CronJob) { job.Enabled = false; f.set(job) }, "was switched off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdmissionProbeServer(t)
			job := cron.CronJob{ID: 42, Enabled: true, Web: true, AuthorizedAt: time.Unix(1790000000, 0)}
			row, jobs, drain := queuedFire(t, s, job)
			tc.change(jobs, job)
			drain()

			waitUntil(t, 5*time.Second, func() bool { return s.droppedNotices() == 1 }, "the person was not told the fire was dropped")
			if got := s.queueRow(t, row.ID); got.Status != control.QueueStatusCancelled || got.RunID != "" {
				t.Fatalf("a fire whose schedule is gone must be cancelled, never run: %+v", got)
			}
			if runs, _ := s.recordedWork(t); runs != 0 || s.modelCalls() != 0 {
				t.Fatalf("a dropped fire ran: %d Runs, %d model calls", runs, s.modelCalls())
			}
			var message string
			for _, msg := range s.sender.snapshot() {
				if strings.Contains(msg.Content, "scheduled run") {
					message = msg.Content
				}
			}
			if !strings.Contains(message, tc.wantWhy) || !strings.Contains(message, "Create the schedule again") {
				t.Fatalf("the notice must say what happened and what to do: %q", message)
			}
			// Later drains neither revive the row nor tell the person again.
			s.daemon.coordinator().drainQueue(s.identity)
			time.Sleep(100 * time.Millisecond)
			if got := s.queueRow(t, row.ID); got.Status != control.QueueStatusCancelled || s.droppedNotices() != 1 {
				t.Fatalf("after later drains: %+v notices=%d", got, s.droppedNotices())
			}
		})
	}
}

// The constraints that must change the result.
func TestQueuedScheduledFireStillRunsWhileItsScheduleStands(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(f *fakeCronJobs, job cron.CronJob)
	}{
		{"enabled job", func(*fakeCronJobs, cron.CronJob) {}},
		// A one-time job is switched off right after its first attempt, so the
		// fire waiting in the queue is exactly the work it was created for.
		{"one-time job, switched off after its attempt", func(f *fakeCronJobs, job cron.CronJob) {
			job.Once, job.Enabled = true, false
			f.set(job)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdmissionProbeServer(t)
			job := cron.CronJob{ID: 43, Enabled: true}
			row, jobs, drain := queuedFire(t, s, job)
			tc.change(jobs, job)
			drain()
			waitUntil(t, 5*time.Second, func() bool {
				q := s.queueRow(t, row.ID)
				return q.Status == control.QueueStatusDone && q.RunID != ""
			}, "the queued scheduled fire did not run")
			waitUntil(t, 5*time.Second, func() bool { return s.daemon.coordinator().activeCount(s.identity.PersonID) == 0 }, "the fire did not finish")
			if jobs.lookups() == 0 {
				t.Fatal("the drain never asked about the schedule")
			}
			if s.droppedNotices() != 0 {
				t.Fatal("a fire that ran was reported as dropped")
			}
		})
	}
}

// A lookup that fails says nothing about the schedule: the row is not claimed,
// not started and not dropped, and it starts once the lookup works. Nothing
// spins while it fails.
func TestQueuedScheduledFireWaitsForALookupThatFails(t *testing.T) {
	s := newAdmissionProbeServer(t)
	row, jobs, drain := queuedFire(t, s, cron.CronJob{ID: 44, Enabled: true})
	jobs.fail(errors.New("injected schedule store failure"))
	drain()
	time.Sleep(300 * time.Millisecond)
	got := s.queueRow(t, row.ID)
	if got.Status != control.QueueStatusQueued || got.RunID != "" || got.AttemptGeneration != 0 {
		t.Fatalf("a fire whose schedule cannot be checked must stay queued and unclaimed: %+v", got)
	}
	if s.droppedNotices() != 0 || s.modelCalls() != 0 {
		t.Fatalf("a failed lookup dropped or ran the fire: notices=%d calls=%d", s.droppedNotices(), s.modelCalls())
	}
	if n := jobs.lookups(); n > 50 {
		t.Fatalf("the drain asked %d times in 300 ms: it spins on a failing lookup", n)
	}

	jobs.fail(nil)
	s.daemon.coordinator().drainQueue(s.identity)
	waitUntil(t, 5*time.Second, func() bool {
		q := s.queueRow(t, row.ID)
		return q.Status == control.QueueStatusDone && q.RunID != ""
	}, "the fire did not run once the lookup worked")
}

// Only rows a schedule wrote are asked about: a person's own queued turn is
// never looked up, and a scheduled row from before snapshots (none to say which
// job it was) runs as it always did.
func TestQueuedRowsThatNoScheduleWroteAreNotAskedAbout(t *testing.T) {
	t.Run("the person's own turn", func(t *testing.T) {
		s := newAdmissionProbeServer(t)
		s.provider.skipApprovals = true
		ctx := context.Background()
		jobs := &fakeCronJobs{jobs: map[int64]cron.CronJob{}, err: errors.New("injected schedule store failure")}
		s.daemon.CronJobs = jobs
		coord := s.daemon.coordinator()
		if !coord.beginActive(s.identity.PersonID, &activeRun{TaskID: "other-task"}) {
			t.Fatal("failed to occupy the person's slot")
		}
		queued, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-own", Content: "summarize the design notes"})
		if code != 200 || queued.Turn == nil || queued.Turn.QueueID == "" {
			t.Fatalf("busy turn was not queued: %d %+v", code, queued)
		}
		coord.endActive(s.identity.PersonID)
		coord.drainQueue(s.identity)
		waitUntil(t, 5*time.Second, func() bool {
			q := s.queueRow(t, queued.Turn.QueueID)
			return q.Status == control.QueueStatusDone && q.RunID != ""
		}, "the person's own queued turn did not run")
		if jobs.lookups() != 0 {
			t.Fatalf("the drain asked the schedule store %d times about a turn no schedule wrote", jobs.lookups())
		}
	})

	t.Run("a scheduled row with no snapshot", func(t *testing.T) {
		s := newAdmissionProbeServer(t)
		row, jobs, drain := queuedFire(t, s, cron.CronJob{ID: 45, Enabled: true})
		jobs.remove(45)
		s.execRaw(t, `UPDATE task_queue SET admission_json = '' WHERE id = ?`, row.ID)
		drain()
		waitUntil(t, 5*time.Second, func() bool {
			q := s.queueRow(t, row.ID)
			return q.Status == control.QueueStatusDone && q.RunID != ""
		}, "the legacy scheduled row did not run")
		if jobs.lookups() != 0 || s.droppedNotices() != 0 {
			t.Fatalf("a row with no snapshot was checked or dropped: lookups=%d notices=%d", jobs.lookups(), s.droppedNotices())
		}
	})

	t.Run("no scheduler configured", func(t *testing.T) {
		s := newAdmissionProbeServer(t)
		row, _, drain := queuedFire(t, s, cron.CronJob{ID: 46, Enabled: true})
		s.daemon.CronJobs = nil
		s.daemon.Schedules = nil
		drain()
		waitUntil(t, 5*time.Second, func() bool {
			q := s.queueRow(t, row.ID)
			return q.Status == control.QueueStatusDone && q.RunID != ""
		}, "a scheduled row ran only with a scheduler to ask")
	})
}

// The daemon's own wiring: the scheduler the Server already holds answers the
// drain's question through the real store, with no separate lookup installed.
func TestQueuedScheduledFireIsCheckedAgainstTheRealScheduler(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	sched := cron.NewScheduler(db, nil)
	if err := sched.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := sched.AddJob(ctx, &cron.CronJob{Name: "nightly", CronExpr: "0 2 * * *", Prompt: "tidy the nightly notes",
		TenantID: s.identity.TenantID, Channel: "cli", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	row, _, drain := queuedFire(t, s, cron.CronJob{ID: id, Enabled: true})
	s.daemon.CronJobs = nil
	s.daemon.Schedules = sched
	if err := sched.RemoveJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	drain()
	waitUntil(t, 5*time.Second, func() bool { return s.queueRow(t, row.ID).Status == control.QueueStatusCancelled }, "the fire of a deleted schedule was not dropped")
	if runs, _ := s.recordedWork(t); runs != 0 || s.modelCalls() != 0 {
		t.Fatalf("a dropped fire ran: %d Runs, %d model calls", runs, s.modelCalls())
	}
}

// A dropped fire that belongs to a thread is recorded there once, and a drain
// that cannot cancel the row yet tells the person nothing twice when it tries
// again.
func TestDroppedScheduledFireIsRecordedOnceOnItsThreadAcrossRetries(t *testing.T) {
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	row, jobs, drain := queuedFire(t, s, cron.CronJob{ID: 47, Enabled: true})
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Title: "nightly notes", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	s.execRaw(t, `UPDATE task_queue SET thread_id = ? WHERE id = ?`, task.ID, row.ID)
	jobs.remove(47)

	// The store refuses to cancel the row, so the first drain tells the person
	// and leaves it queued.
	s.execRaw(t, `CREATE TRIGGER fail_cron_cancel BEFORE UPDATE ON task_queue WHEN NEW.status = 'cancelled' BEGIN SELECT RAISE(ABORT,'injected cancel failure'); END`)
	drain()
	waitUntil(t, 5*time.Second, func() bool { return s.droppedNotices() == 1 }, "the person was not told")
	if got := s.queueRow(t, row.ID); got.Status != control.QueueStatusQueued {
		t.Fatalf("the injected failure did not leave the row queued: %+v", got)
	}
	s.execRaw(t, `DROP TRIGGER fail_cron_cancel`)
	s.daemon.coordinator().drainQueue(s.identity)
	waitUntil(t, 5*time.Second, func() bool { return s.queueRow(t, row.ID).Status == control.QueueStatusCancelled }, "the retry did not cancel the row")
	events, err := s.store.ListTaskEvents(ctx, task.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	recorded := 0
	for _, event := range events {
		if event.Type == "queue.cron_fire_dropped" {
			recorded++
		}
	}
	time.Sleep(100 * time.Millisecond)
	if recorded != 1 || s.droppedNotices() != 1 {
		t.Fatalf("the notice must be recorded once across retries: events=%d pushes=%d", recorded, s.droppedNotices())
	}
}
