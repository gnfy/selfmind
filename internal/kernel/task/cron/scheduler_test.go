package cron

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"selfmind/internal/kernel"

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

// fakeAuthority binds every job to one fixed endpoint and owns the jobs that
// deliver to that endpoint, standing in for the gateway's identity store.
type fakeAuthority struct {
	endpoint JobEndpoint
	owner    string
	bound    []JobRequest
}

func (a *fakeAuthority) Bind(_ context.Context, scope kernel.ToolInvocationScope, request JobRequest) (JobEndpoint, error) {
	a.bound = append(a.bound, request)
	if scope.PersonID != a.owner {
		return JobEndpoint{}, errors.New("not your endpoint")
	}
	return a.endpoint, nil
}

func (a *fakeAuthority) Standing(_ context.Context, scope kernel.ToolInvocationScope, job CronJob) (JobStanding, error) {
	standing := JobStanding{Owned: scope.PersonID == a.owner && job.DeliverTo == a.endpoint.DeliverTo}
	if standing.Owned && job.Channel != a.endpoint.Channel {
		standing.Blocked = "it would post to chat " + job.Channel
	}
	return standing, nil
}

func runAs(person string, args map[string]interface{}) map[string]interface{} {
	args["_invocation_scope"] = kernel.ToolInvocationScope{ControlTenantID: "default", PersonID: person, RunID: "run-" + person}
	return args
}

// The tool stores the endpoint its authority binds, never the fields the model
// sent, and acts only inside the calling Run's tenant.
func TestCronToolStoresTheBoundEndpoint(t *testing.T) {
	s := newTestScheduler(t)
	authority := &fakeAuthority{owner: "person-a",
		endpoint: JobEndpoint{TenantID: "default", Platform: "weixin", DeliverTo: "wxid_abc", Channel: "wxid_abc"}}
	tool := &ToolAdapter{CronTool: NewCronTool(s, authority)}
	if _, err := tool.Execute(runAs("person-a", map[string]interface{}{
		"action": "add", "name": "notify", "cron": "1 2 3 4 *", "prompt": "hello",
		"channel": "weixin", "platform": "weixin", "deliver_to": "wxid_abc", "once": true,
	})); err != nil {
		t.Fatal(err)
	}
	if len(authority.bound) != 1 || authority.bound[0] != (JobRequest{Platform: "weixin", DeliverTo: "wxid_abc", Channel: "weixin", Prompt: "hello"}) {
		t.Fatalf("authority saw %+v", authority.bound)
	}
	jobs, err := s.ListJobs(context.Background(), "default")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
	if jobs[0].Channel != "wxid_abc" || jobs[0].Platform != "weixin" || jobs[0].DeliverTo != "wxid_abc" || !jobs[0].Once {
		t.Fatalf("job not stored with its bound endpoint: %+v", jobs[0])
	}

	for name, args := range map[string]map[string]interface{}{
		"no run scope":   {"action": "add", "name": "x", "cron": "1 2 3 4 *", "prompt": "hello"},
		"other tenant":   runAs("person-a", map[string]interface{}{"action": "add", "name": "x", "cron": "1 2 3 4 *", "prompt": "hello", "tenantID": "other"}),
		"other person":   runAs("person-b", map[string]interface{}{"action": "add", "name": "x", "cron": "1 2 3 4 *", "prompt": "hello"}),
		"list elsewhere": runAs("person-a", map[string]interface{}{"action": "list", "tenantID": "other"}),
	} {
		if _, err := tool.Execute(args); err == nil {
			t.Fatalf("%s: the cron tool accepted the call", name)
		}
	}
	if _, err := (&ToolAdapter{CronTool: NewCronTool(s, nil)}).Execute(runAs("person-a", map[string]interface{}{"action": "list"})); err == nil {
		t.Fatal("a cron tool without an authority must refuse")
	}
	if jobs, _ := s.ListJobs(context.Background(), "default"); len(jobs) != 1 {
		t.Fatalf("refused calls stored jobs: %+v", jobs)
	}
	if jobs, _ := s.ListJobs(context.Background(), "other"); len(jobs) != 0 {
		t.Fatalf("a job was stored in another tenant: %+v", jobs)
	}
}

// list, remove, and enable reach only the calling person's jobs. Another
// person's job reads as not found, whether or not it exists.
func TestCronToolReachesOnlyOwnJobs(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()
	own, err := s.AddJob(ctx, &CronJob{Name: "mine", CronExpr: "0 8 * * *", Prompt: "p", TenantID: "default",
		Platform: "weixin", DeliverTo: "wxid_abc", Channel: "wxid_abc", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := s.AddJob(ctx, &CronJob{Name: "theirs", CronExpr: "0 9 * * *", Prompt: "private plan", TenantID: "default",
		Platform: "telegram", DeliverTo: "900", Channel: "900", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tool := &ToolAdapter{CronTool: NewCronTool(s, &fakeAuthority{owner: "person-a",
		endpoint: JobEndpoint{TenantID: "default", Platform: "weixin", DeliverTo: "wxid_abc", Channel: "wxid_abc"}})}
	listed, err := tool.Execute(runAs("person-a", map[string]interface{}{"action": "list"}))
	if err != nil || !strings.Contains(listed, "mine") || strings.Contains(listed, "theirs") {
		t.Fatalf("list = %q, %v", listed, err)
	}
	for _, action := range []map[string]interface{}{
		{"action": "remove", "job_id": float64(theirs)},
		{"action": "enable", "job_id": theirs, "enabled": false},
	} {
		if _, err := tool.Execute(runAs("person-a", action)); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("%v on another person's job: %v", action["action"], err)
		}
	}
	if _, err := tool.Execute(runAs("person-a", map[string]interface{}{"action": "enable", "job_id": int(own), "enabled": false})); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(runAs("person-a", map[string]interface{}{"action": "remove", "job_id": float64(own)})); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ListJobs(ctx, "default")
	if err != nil || len(jobs) != 1 || jobs[0].ID != theirs || !jobs[0].Enabled {
		t.Fatalf("another person's job changed: %+v %v", jobs, err)
	}
}

// A job of the person's own that does not run is listed with the reason, so
// the person can remove it or add it again instead of finding it silent.
func TestCronToolListsWhyAnOwnJobDoesNotRun(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()
	if _, err := s.AddJob(ctx, &CronJob{Name: "daily", CronExpr: "0 8 * * *", Prompt: "p", TenantID: "default",
		Platform: "weixin", DeliverTo: "wxid_abc", Channel: "wxid_abc", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	stuck, err := s.AddJob(ctx, &CronJob{Name: "team digest", CronExpr: "0 9 * * *", Prompt: "p", TenantID: "default",
		Platform: "weixin", DeliverTo: "wxid_abc", Channel: "team@chatroom", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tool := &ToolAdapter{CronTool: NewCronTool(s, &fakeAuthority{owner: "person-a",
		endpoint: JobEndpoint{TenantID: "default", Platform: "weixin", DeliverTo: "wxid_abc", Channel: "wxid_abc"}})}
	listed, err := tool.Execute(runAs("person-a", map[string]interface{}{"action": "list"}))
	if err != nil {
		t.Fatal(err)
	}
	var daily, digest string
	for _, line := range strings.Split(listed, "\n") {
		switch {
		case strings.Contains(line, "daily"):
			daily = line
		case strings.Contains(line, "team digest"):
			digest = line
		}
	}
	if strings.Contains(daily, "does not run") || !strings.Contains(digest, "does not run: it would post to chat team@chatroom") ||
		!strings.Contains(listed, "remove it") {
		t.Fatalf("list = %q", listed)
	}
	if _, err := tool.Execute(runAs("person-a", map[string]interface{}{"action": "remove", "job_id": float64(stuck)})); err != nil {
		t.Fatalf("the person cannot remove their own job that does not run: %v", err)
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

// TestEnsureJobIdempotent proves built-in jobs such as the canary don't
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

func TestEnsureJobRejectsRetiredSkillPrunerSystemKey(t *testing.T) {
	s := newTestScheduler(t)
	_, err := s.EnsureJob(context.Background(), &CronJob{
		Name: "skill-pruner-default", CronExpr: "0 3 * * *", Prompt: "skill_prune:default",
		TenantID: "default", Channel: "cli", Enabled: true, SystemKey: "skill-pruner:default",
	})
	if err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("retired Skill metric pruner was accepted: %v", err)
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

// TestSystemJobGovernanceMigrationRetiresLegacySkillPruners pins the final
// migration from frozen memory.db counters to durable activation outcomes.
// Exact built-in rows disappear while unrelated user jobs survive.
func TestSystemJobGovernanceMigrationRetiresLegacySkillPruners(t *testing.T) {
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
	if systemPruners != 0 {
		t.Fatalf("obsolete system pruner rows = %d, want 0", systemPruners)
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
	// The partial unique index still protects current daemon-owned jobs.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO cron_jobs
		(name, cron_expr, prompt, tenant_id, channel, enabled, system_key)
		VALUES ('system-one', '0 3 * * *', 'x', 'default', 'cli', 1, 'system:test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO cron_jobs
		(name, cron_expr, prompt, tenant_id, channel, enabled, system_key)
		VALUES ('system-two', '0 4 * * *', 'y', 'default', 'cli', 1, 'system:test')`); err == nil {
		t.Fatal("duplicate system_key insert must fail")
	}
	// Migration is idempotent on a clean table.
	if err := s.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
}
