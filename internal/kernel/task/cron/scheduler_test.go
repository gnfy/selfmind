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

func TestEnsureJobRepairsHistoricalDuplicates(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO cron_jobs
			(name, cron_expr, prompt, tenant_id, channel, enabled)
			VALUES ('skill-pruner-default', '0 3 * * *', 'skill_prune:default', 'default', 'cli', 1)`); err != nil {
			t.Fatalf("seed duplicate %d: %v", i, err)
		}
	}
	// The repair runs during the next daemon boot. Keeping it separate from
	// EnsureJob avoids teaching runtime registration to adopt arbitrary
	// same-named user rows.
	if err := s.InitSchema(ctx); err != nil {
		t.Fatalf("repair historical duplicates: %v", err)
	}
	keepID, err := s.EnsureJob(ctx, &CronJob{
		Name: "skill-pruner-default", CronExpr: "30 3 * * *", Prompt: "skill_prune:default",
		TenantID: "default", Channel: "cli", Enabled: true, SystemKey: "skill-pruner:default",
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ListJobs(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	var matches []CronJob
	for _, job := range jobs {
		if job.Name == "skill-pruner-default" {
			matches = append(matches, job)
		}
	}
	if len(matches) != 1 || matches[0].ID != keepID || matches[0].CronExpr != "30 3 * * *" {
		t.Fatalf("deduplicated jobs=%+v keep=%d", matches, keepID)
	}
	if len(s.entries) != 1 {
		t.Fatalf("scheduled entries=%d, want 1 after repair", len(s.entries))
	}
}

func TestAddJobRejectsReservedSystemName(t *testing.T) {
	s := newTestScheduler(t)
	_, err := s.AddJob(context.Background(), &CronJob{
		Name: "skill-pruner-person_abc", CronExpr: "0 3 * * *", Prompt: "custom cleanup",
		TenantID: "default", Channel: "cli", Enabled: true,
	})
	if err == nil {
		t.Fatal("user jobs must not claim the reserved skill-pruner-* namespace")
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

// TestSystemJobGovernanceMigration pins the 2026-07 repair: per-data-directory
// pruner rows collapse to exactly one control-tenant pruner, historic
// duplicates fold, the partial unique index blocks future system duplicates,
// and a historical user row with a coincidental reserved-prefix name but a
// non-system shape survives the migration intact.
func TestSystemJobGovernanceMigration(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()
	// Simulate the polluted state: per-tenant pruners + duplicated default rows.
	seed := func(name, tenant string) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO cron_jobs
			(name, cron_expr, prompt, tenant_id, channel, enabled) VALUES (?, '0 3 * * *', 'skill_prune:x', ?, 'cli', 1)`,
			name, tenant); err != nil {
			t.Fatal(err)
		}
	}
	seed("skill-pruner-default", "default")
	seed("skill-pruner-default", "default")
	seed("skill-pruner-default", "default")
	seed("skill-pruner-eval-chat_basic_001-123456789012345", "eval-chat_basic_001-123456789012345")
	seed("skill-pruner-person_abc", "person_abc")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO cron_jobs
		(name, cron_expr, prompt, tenant_id, channel, enabled)
		VALUES ('skill-pruner-user-report', '15 9 * * 1', 'send my weekly report', 'default', 'weixin', 1)`); err != nil {
		t.Fatal(err)
	}
	// A user's own job with a coincidental duplicate name must survive intact.
	seed("my-daily-report", "default")
	seed("my-daily-report", "default")

	if err := s.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var systemPruners, userJobs, preservedReserved int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs WHERE system_key = 'skill-pruner:default'`).Scan(&systemPruners); err != nil {
		t.Fatal(err)
	}
	if systemPruners != 1 {
		t.Fatalf("system pruner rows = %d, want exactly 1", systemPruners)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs
		WHERE name = 'skill-pruner-user-report' AND cron_expr = '15 9 * * 1'
		  AND prompt = 'send my weekly report' AND channel = 'weixin'
		  AND COALESCE(system_key, '') = ''`).Scan(&preservedReserved); err != nil {
		t.Fatal(err)
	}
	if preservedReserved != 1 {
		t.Fatalf("historical user job under reserved prefix was changed or deleted")
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs WHERE name = 'my-daily-report'`).Scan(&userJobs); err != nil {
		t.Fatal(err)
	}
	if userJobs != 2 {
		t.Fatalf("user duplicate-name jobs = %d, want 2 (governance must not constrain user jobs)", userJobs)
	}
	var key string
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(system_key,'') FROM cron_jobs WHERE name = 'skill-pruner-default'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "skill-pruner:default" {
		t.Fatalf("surviving pruner system_key = %q", key)
	}

	// The partial unique index blocks a second row for the same system key...
	if _, err := s.db.ExecContext(ctx, `INSERT INTO cron_jobs
		(name, cron_expr, prompt, tenant_id, channel, enabled, system_key)
		VALUES ('another-pruner', '0 3 * * *', 'x', 'default', 'cli', 1, 'skill-pruner:default')`); err == nil {
		t.Fatal("duplicate system_key insert must fail")
	}
	// ...while EnsureJob converges on the surviving row by system key even
	// across a rename.
	id, err := s.EnsureJob(ctx, &CronJob{
		Name: "skill-pruner-default", CronExpr: "0 3 * * *", Prompt: "skill_prune:default",
		TenantID: "default", Channel: "cli", Enabled: true, SystemKey: "skill-pruner:default",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cron_jobs WHERE system_key = 'skill-pruner:default'`).Scan(&systemPruners); err != nil {
		t.Fatal(err)
	}
	if systemPruners != 1 || id == 0 {
		t.Fatalf("EnsureJob by system key must reuse the surviving row: rows=%d id=%d", systemPruners, id)
	}
	// Migration is idempotent on a clean table.
	if err := s.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
}
