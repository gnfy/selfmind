package cron

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestScheduler(t *testing.T) *Scheduler {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// mem is nil: Add/List/Remove and the executor path never touch it, and the
	// marker fallback is nil-guarded.
	s := NewScheduler(db, nil)
	if err := s.InitSchema(context.Background()); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	return s
}

// TestMigrationAndRoundTrip proves the proactive-delivery columns are added and
// that platform/deliver_to/web survive a write→read cycle.
func TestMigrationAndRoundTrip(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()

	id, err := s.AddJob(ctx, &CronJob{
		Name: "daily-summary", CronExpr: "0 8 * * *", Prompt: "汇总今天的工作进展",
		TenantID: "default", Channel: "weixin", Platform: "weixin", DeliverTo: "wxid_abc", Web: true, Enabled: false,
	})
	if err != nil {
		t.Fatalf("add job: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero job id")
	}

	jobs, err := s.ListJobs(ctx, "default")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
	j := jobs[0]
	if j.Platform != "weixin" || j.DeliverTo != "wxid_abc" || !j.Web {
		t.Fatalf("delivery fields did not round-trip: %+v", j)
	}
}

// TestMigrationIdempotentOnLegacyTable proves InitSchema upgrades a pre-existing
// table that lacks the new columns, without dropping data.
func TestMigrationIdempotentOnLegacyTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Create the OLD schema (no platform/deliver_to/web) and a legacy row.
	_, err = db.Exec(`CREATE TABLE cron_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, cron_expr TEXT, prompt TEXT,
		tenant_id TEXT, channel TEXT DEFAULT 'cli', enabled INTEGER DEFAULT 1,
		last_run INTEGER, next_run INTEGER, created_at INTEGER DEFAULT (strftime('%s','now')))`)
	if err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO cron_jobs (name, cron_expr, prompt, tenant_id, channel) VALUES ('old','0 3 * * *','x','default','cli')`); err != nil {
		t.Fatalf("legacy row: %v", err)
	}

	s := NewScheduler(db, nil)
	if err := s.InitSchema(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	jobs, err := s.ListJobs(context.Background(), "default")
	if err != nil {
		t.Fatalf("list after migrate: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Name != "old" {
		t.Fatalf("legacy row lost after migration: %+v", jobs)
	}
	if jobs[0].Platform != "" || jobs[0].Web {
		t.Fatalf("new columns should default empty/false: %+v", jobs[0])
	}
}

type fakeExecutor struct {
	called int
	last   CronJob
}

func (f *fakeExecutor) RunCronJob(_ context.Context, job CronJob) error {
	f.called++
	f.last = job
	return nil
}

// TestRunJobPrefersExecutor proves the executor runs the job (and the marker
// fallback is NOT used) when an executor is installed.
func TestRunJobPrefersExecutor(t *testing.T) {
	s := newTestScheduler(t)
	fx := &fakeExecutor{}
	s.SetExecutor(fx)

	s.runJob(context.Background(), &CronJob{ID: 1, Name: "j", Prompt: "do it", TenantID: "default", Channel: "weixin", Platform: "weixin", DeliverTo: "wxid_abc"})

	if fx.called != 1 {
		t.Fatalf("executor called %d times, want 1", fx.called)
	}
	if fx.last.DeliverTo != "wxid_abc" {
		t.Fatalf("executor got wrong job: %+v", fx.last)
	}
}

func TestOneTimeJobRecordsRunAndDisablesItself(t *testing.T) {
	s := newTestScheduler(t)
	fx := &fakeExecutor{}
	s.SetExecutor(fx)
	ctx := context.Background()
	job := &CronJob{
		Name: "one-shot", CronExpr: "1 2 3 4 *", Prompt: "remind me",
		TenantID: "default", Channel: "wxid_abc", Platform: "weixin", DeliverTo: "wxid_abc",
		Once: true, Enabled: true,
	}
	id, err := s.AddJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != id || id == 0 {
		t.Fatalf("job id not propagated: id=%d job=%+v", id, job)
	}

	s.runJob(ctx, job)
	jobs, err := s.ListJobs(ctx, "default")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	if jobs[0].Enabled || jobs[0].LastRun == nil || !jobs[0].Once {
		t.Fatalf("one-time job was not completed durably: %+v", jobs[0])
	}
	if _, ok := s.entries[id]; ok {
		t.Fatalf("one-time job %d remains scheduled", id)
	}
}

func TestCronToolNormalizesProactiveRecipient(t *testing.T) {
	s := newTestScheduler(t)
	tool := NewCronTool(s)
	_, err := tool.Execute(context.Background(), map[string]interface{}{
		"action": "add", "name": "notify", "cron": "1 2 3 4 *", "prompt": "hello",
		"tenantID": "default", "channel": "weixin", "platform": "weixin",
		"deliver_to": "wxid_abc", "once": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ListJobs(context.Background(), "default")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	if jobs[0].Channel != "wxid_abc" || !jobs[0].Once {
		t.Fatalf("proactive job not normalized: %+v", jobs[0])
	}
}

// TestRunJobFallbackWithoutExecutor proves a memless, executor-less scheduler
// does not panic (it degrades to a no-op marker path).
func TestRunJobFallbackWithoutExecutor(t *testing.T) {
	s := newTestScheduler(t)
	s.runJob(context.Background(), &CronJob{ID: 2, Name: "j", Prompt: "p", TenantID: "default", Channel: "cli"})
}

// TestEntryIDMappingNotStale proves enable→disable→remove uses the entry-id map
// rather than assuming cron.EntryID == SQLite id.
func TestEntryIDMappingNotStale(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()
	// Two enabled jobs so SQLite ids and cron entry ids can diverge.
	id1, err := s.AddJob(ctx, &CronJob{Name: "a", CronExpr: "0 8 * * *", Prompt: "p", TenantID: "default", Channel: "cli", Enabled: true})
	if err != nil {
		t.Fatalf("add a: %v", err)
	}
	id2, err := s.AddJob(ctx, &CronJob{Name: "b", CronExpr: "0 9 * * *", Prompt: "p", TenantID: "default", Channel: "cli", Enabled: true})
	if err != nil {
		t.Fatalf("add b: %v", err)
	}
	if _, ok := s.entries[id1]; !ok {
		t.Fatalf("job %d not scheduled", id1)
	}
	if _, ok := s.entries[id2]; !ok {
		t.Fatalf("job %d not scheduled", id2)
	}
	if err := s.RemoveJob(ctx, id1); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := s.entries[id1]; ok {
		t.Fatalf("entry for removed job %d still present", id1)
	}
	if _, ok := s.entries[id2]; !ok {
		t.Fatalf("removing job %d wrongly affected job %d", id1, id2)
	}
}

// TestEnsureJobIdempotent proves built-in jobs (skill-pruner, canary) don't
// duplicate across restarts: same name+tenant updates in place.
func TestEnsureJobIdempotent(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.EnsureJob(ctx, &CronJob{Name: "canary", CronExpr: "0 * * * *", Prompt: "canary:", TenantID: "default", Channel: "cli", Enabled: true}); err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	jobs, err := s.ListJobs(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, j := range jobs {
		if j.Name == "canary" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("canary registered %d times, want 1 (no duplicates across restarts)", n)
	}
}

func TestSetTimezone(t *testing.T) {
	s := newTestScheduler(t)
	if err := s.SetTimezone("Asia/Shanghai"); err != nil {
		t.Fatalf("valid tz: %v", err)
	}
	if err := s.SetTimezone(""); err != nil {
		t.Fatalf("empty tz should default to local: %v", err)
	}
	if err := s.SetTimezone("Not/AZone"); err == nil {
		t.Fatal("invalid tz should error")
	}
}

// TestTaskIDBindingRoundTrip: SetTaskID persists the learned label binding
// (execution-quality W6), ListJobs reads it back, and clearing works.
func TestTaskIDBindingRoundTrip(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()
	id, err := s.AddJob(ctx, &CronJob{
		Name: "daily-report", CronExpr: "0 9 * * *", Prompt: "日报",
		TenantID: "default", Channel: "cli", Enabled: false,
	})
	if err != nil {
		t.Fatalf("add job: %v", err)
	}
	if err := s.SetTaskID(ctx, id, "task_abc123"); err != nil {
		t.Fatalf("set task id: %v", err)
	}
	jobs, err := s.ListJobs(ctx, "default")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list jobs: %v %d", err, len(jobs))
	}
	if jobs[0].TaskID != "task_abc123" {
		t.Fatalf("task binding did not round-trip: %+v", jobs[0])
	}
	// Clearing (archived label) resets to the pre-binding behavior.
	if err := s.SetTaskID(ctx, id, ""); err != nil {
		t.Fatalf("clear task id: %v", err)
	}
	jobs, _ = s.ListJobs(ctx, "default")
	if jobs[0].TaskID != "" {
		t.Fatalf("clear must empty the binding: %+v", jobs[0])
	}
}
