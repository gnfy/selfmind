package control

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentControlSchemaSkipsFullIntegrityCheck(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	checks := 0
	err = store.prepareAndMigrateSchema(
		context.Background(), dir, filepath.Join(dir, "control.db"), true,
		func(context.Context, *sql.DB) error {
			checks++
			return errors.New("unexpected full integrity check")
		},
	)
	if err != nil {
		t.Fatalf("reopen current schema: %v", err)
	}
	if checks != 0 {
		t.Fatalf("current schema integrity checks=%d, want 0", checks)
	}
}

func TestVersionOneControlStoreMigratesMemoryGovernanceSchedule(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE memory_governance_schedule`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES (1, 'legacy-baseline', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("migrate v1 store: %v", err)
	}
	defer migrated.Close()
	if migrated.SchemaStatus().Version != CurrentControlSchemaVersion {
		t.Fatalf("schema status=%+v", migrated.SchemaStatus())
	}
	var tables int
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memory_governance_schedule'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("schedule tables=%d err=%v", tables, err)
	}
	if strings.TrimSpace(migrated.SchemaStatus().MigrationBackup) == "" {
		t.Fatal("versioned migration did not create a backup")
	}
}

func TestLegacyControlSchemaKeepsMigrationBoundaryIntegrityChecks(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}

	checks := 0
	err = store.prepareAndMigrateSchema(
		context.Background(), dir, filepath.Join(dir, "control.db"), true,
		func(ctx context.Context, db *sql.DB) error {
			checks++
			return quickCheckDB(ctx, db)
		},
	)
	if err != nil {
		t.Fatalf("migrate legacy schema: %v", err)
	}
	if checks != 2 {
		t.Fatalf("migration boundary integrity checks=%d, want 2", checks)
	}
}

func TestReleasedBeta15ControlStoreFixtureMigratesAndReopens(t *testing.T) {
	dir := t.TempDir()
	fixture, err := os.Open(filepath.Join("testdata", "control-v0-beta.15.db.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	compressed, err := gzip.NewReader(fixture)
	if err != nil {
		t.Fatal(err)
	}
	database, err := os.OpenFile(filepath.Join(dir, "control.db"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(database, compressed); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	for name, query := range map[string]string{
		"accounts":  `SELECT COUNT(*) FROM accounts`,
		"tasks":     `SELECT COUNT(*) FROM tasks`,
		"approvals": `SELECT COUNT(*) FROM approval_requests`,
	} {
		var rows int
		if err := legacy.QueryRow(query).Scan(&rows); err != nil || rows != 0 {
			_ = legacy.Close()
			t.Fatalf("released fixture %s rows=%d err=%v", name, rows, err)
		}
	}
	var versionTables int
	if err := legacy.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&versionTables); err != nil || versionTables != 0 {
		_ = legacy.Close()
		t.Fatalf("released fixture schema_migrations=%d err=%v", versionTables, err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("migrate released beta.15 fixture: %v", err)
	}
	status := store.SchemaStatus()
	if status.Version != CurrentControlSchemaVersion || strings.TrimSpace(status.MigrationBackup) == "" {
		t.Fatalf("schema status=%+v", status)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := countMigrationBackups(t, dir); got != 1 {
		t.Fatalf("migration backups=%d, want 1", got)
	}

	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen migrated beta.15 fixture: %v", err)
	}
	defer reopened.Close()
	if got := countMigrationBackups(t, dir); got != 1 {
		t.Fatalf("current schema created another backup: got %d", got)
	}
}

func TestLegacyControlStoreMigrationBacksUpAndDoesNotReplayHistoricalApproval(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "legacy-owner", "Legacy Owner")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "legacy task", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "legacy work")
	if err != nil {
		t.Fatal(err)
	}
	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		ActionType: "tool_call", AuthorizationFingerprint: "resume:v1:legacy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, approval.ID, "approved", "cli", ApprovalDecisionInput{DecisionID: "once"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkInterruptedRuns(ctx, 0); err != nil {
		t.Fatal(err)
	}
	// Reproduce a database created before crash-safe approval recovery and before
	// explicit schema versions. Its terminal decision must remain inert.
	if _, err := store.db.ExecContext(ctx, `UPDATE approval_requests SET decision_recorded_at = NULL WHERE id = ?`, approval.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	status := migrated.SchemaStatus()
	if status.Version != CurrentControlSchemaVersion || strings.TrimSpace(status.MigrationBackup) == "" {
		t.Fatalf("schema status=%+v", status)
	}
	if info, err := os.Stat(status.MigrationBackup); err != nil || info.Size() == 0 {
		t.Fatalf("migration backup=%q info=%v err=%v", status.MigrationBackup, info, err)
	}
	if rows, err := migrated.ListRecoverableApprovalDecisions(ctx, 10); err != nil || len(rows) != 0 {
		t.Fatalf("historical approval became recoverable: rows=%+v err=%v", rows, err)
	}
	queued, err := migrated.ListQueued(ctx, identity.TenantID, identity.PersonID, QueueStatusQueued)
	if err != nil || len(queued) != 0 {
		t.Fatalf("migration created queue work: rows=%+v err=%v", queued, err)
	}

	backupCount := countMigrationBackups(t, dir)
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := countMigrationBackups(t, dir); got != backupCount {
		t.Fatalf("current schema created another backup: before=%d after=%d", backupCount, got)
	}
}

func TestNewerControlSchemaIsRejectedBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, 'future', 1)`, CurrentControlSchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(dir); err == nil || !strings.Contains(err.Error(), "newer than this SelfMind binary") {
		t.Fatalf("future schema error=%v", err)
	}
}

func TestMigrationBackupIsReadableSQLiteSnapshot(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	backupPath := migrated.SchemaStatus().MigrationBackup
	_ = migrated.Close()
	backup, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var approvals int
	if err := backup.QueryRow(`SELECT COUNT(*) FROM approval_requests`).Scan(&approvals); err != nil {
		t.Fatalf("read backup: %v", err)
	}
}

func TestRestoreControlDatabasePreservesFailedCopyAndRestoresBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveOrCreateAccount(ctx, DefaultTenantID, "cli", "restore-owner", "Restore Owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "before backup", Channel: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	backupPath := migrated.SchemaStatus().MigrationBackup
	if _, err := migrated.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "after backup", Channel: "cli"}); err != nil {
		t.Fatal(err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}

	failedPath, err := RestoreControlDatabase(ctx, dir, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := nonEmptyRegularFile(failedPath); err != nil || !ok {
		t.Fatalf("failed copy=%q ok=%v err=%v", failedPath, ok, err)
	}
	restored, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	tasks, err := restored.ListTasks(ctx, identity.TenantID, identity.PersonID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.Title == "after backup" {
			t.Fatalf("post-backup task survived restore: %+v", task)
		}
	}
}

func countMigrationBackups(t *testing.T, dir string) int {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "backups", "control-v*-to-v*.db"))
	if err != nil {
		t.Fatal(err)
	}
	return len(paths)
}

// TestVersionTenFixtureMigratesRowsToThreadedWorkHistory drives the v10->v11
// step over a row-bearing database built from the schema-only DDL of a real
// version-10 control.db. It pins what an installed upgrade must preserve: every
// row, the forward parent edge, the legacy reverse edge, pinning, the
// visibility and kind mapping, referential integrity, the pre-migration backup,
// and an idempotent second open.
func TestVersionTenFixtureMigratesRowsToThreadedWorkHistory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ddl, err := os.ReadFile(filepath.Join("testdata", "control-v10-schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(string(ddl)), "INSERT") {
		t.Fatal("the v10 fixture must be schema only")
	}
	legacy, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := legacy.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(string(ddl))
	exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, 'legacy-baseline', 1)`, schemaBaselineVersion)
	for _, migration := range orderedMigrations {
		if migration.Version > 10 {
			continue
		}
		exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`, migration.Version, migration.Name, migration.Version)
	}
	exec(`INSERT INTO tasks(id, tenant_id, person_id, workspace_id, title, status, kind, visibility, pinned, current_summary, created_at, updated_at) VALUES
		('task_hidden', 'default', 'person_a', 'ws_1', 'hidden inbox', 'done', 'inbox', 'hidden', 0, 'inbox summary', 1, 2),
		('task_pinned', 'default', 'person_a', 'ws_1', 'pinned work', 'waiting_user', 'work', 'visible', 1, 'pinned summary', 3, 4),
		('task_recurring', 'default', 'person_a', 'ws_1', 'nightly job', 'in_progress', 'recurring', 'visible', 0, NULL, 5, 6),
		('task_archived', 'default', 'person_b', NULL, 'old work', 'done', 'work', 'archived', 0, '', 7, 8)`)
	exec(`INSERT INTO task_runs(id, task_id, tenant_id, person_id, workspace_id, channel, input_summary, status, started_at, finished_at, resumed_by_run_id, parent_run_id) VALUES
		('run_hidden', 'task_hidden', 'default', 'person_a', 'ws_1', 'cli', 'hello', 'done', 10, 11, '', ''),
		('run_parent', 'task_pinned', 'default', 'person_a', 'ws_1', 'cli', 'plan release', 'waiting_user', 20, 21, '', ''),
		('run_child', 'task_pinned', 'default', 'person_a', 'ws_1', 'weixin', 'confirm', 'done', 30, 31, '', 'run_parent'),
		('run_legacy', 'task_recurring', 'default', 'person_a', 'ws_1', 'cron', 'nightly', 'interrupted', 40, NULL, 'run_hidden', '')`)
	exec(`INSERT INTO task_events(id, task_id, run_id, type, visibility, created_at) VALUES
		('evt_1', 'task_hidden', 'run_hidden', 'run.finished', 'task', 11),
		('evt_2', 'task_pinned', 'run_parent', 'run.finished', 'task', 21),
		('evt_3', 'task_pinned', 'run_child', 'run.finished', 'task', 31),
		('evt_4', 'task_recurring', 'run_legacy', 'run.interrupted', 'task', 41)`)
	exec(`INSERT INTO task_handoffs(id, task_id, run_id, summary, created_at) VALUES ('handoff_run_run_parent', 'task_pinned', 'run_parent', 'waiting for confirmation', 21)`)
	exec(`INSERT INTO task_artifacts(id, task_id, run_id, kind, name, uri, created_at) VALUES ('art_1', 'task_pinned', 'run_parent', 'file', 'plan.md', 'file:///plan.md', 21)`)
	exec(`INSERT INTO approval_requests(id, tenant_id, person_id, task_id, run_id, action_type, status, created_at, updated_at) VALUES ('apr_1', 'default', 'person_a', 'task_pinned', 'run_parent', 'tool_call', 'pending', 22, 22)`)
	exec(`INSERT INTO clarify_requests(id, tenant_id, person_id, task_id, run_id, question, status, created_at, updated_at) VALUES ('clr_1', 'default', 'person_a', 'task_recurring', 'run_legacy', 'which environment?', 'pending', 42, 42)`)
	exec(`INSERT INTO task_queue(id, tenant_id, person_id, channel, platform, content, task_id, status, created_at) VALUES ('queue_1', 'default', 'person_a', 'cli', 'cli', 'queued follow-up', 'task_pinned', 'queued', 50)`)
	exec(`INSERT INTO current_task(tenant_id, person_id, task_id, updated_at) VALUES ('default', 'person_a', 'task_pinned', 51)`)
	exec(`INSERT INTO task_references(id, tenant_id, person_id, task_id, class, raw_value, normalized_value, created_at, updated_at) VALUES ('ref_1', 'default', 'person_a', 'task_pinned', 'name', 'release', 'release', 52, 52)`)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("migrate v10 fixture: %v", err)
	}
	status := store.SchemaStatus()
	if status.Version != CurrentControlSchemaVersion || strings.TrimSpace(status.MigrationBackup) == "" {
		t.Fatalf("schema status=%+v", status)
	}
	if info, err := os.Stat(status.MigrationBackup); err != nil || info.Size() == 0 {
		t.Fatalf("backup=%q info=%v err=%v", status.MigrationBackup, info, err)
	}
	if modern, err := hasThreadedWorkHistorySchema(ctx, store.db); err != nil || !modern {
		t.Fatalf("threaded schema=%v err=%v", modern, err)
	}
	assertFixtureCounts := func(db *sql.DB) {
		t.Helper()
		for table, want := range map[string]int{
			"threads": 4, "runs": 4, "task_events": 4, "task_handoffs": 1, "task_artifacts": 1,
			"approval_requests": 1, "clarify_requests": 1, "task_queue": 1, "task_references": 1,
		} {
			var got int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&got); err != nil || got != want {
				t.Fatalf("%s rows=%d err=%v, want %d", table, got, err, want)
			}
		}
	}
	assertFixtureCounts(store.db)
	for _, retired := range []string{"tasks", "task_runs", "current_task"} {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, retired).Scan(&count); err != nil || count != 0 {
			t.Fatalf("retired table %s count=%d err=%v", retired, count, err)
		}
	}
	var resumeEdges int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE resumes_run_id <> ''`).Scan(&resumeEdges); err != nil || resumeEdges != 1 {
		t.Fatalf("parent edges=%d err=%v, want the single seeded edge", resumeEdges, err)
	}
	var resumesRunID, resumedBy string
	if err := store.db.QueryRowContext(ctx, `SELECT resumes_run_id FROM runs WHERE id = 'run_child'`).Scan(&resumesRunID); err != nil || resumesRunID != "run_parent" {
		t.Fatalf("child resumes=%q err=%v", resumesRunID, err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT resumed_by_run_id FROM runs WHERE id = 'run_legacy'`).Scan(&resumedBy); err != nil || resumedBy != "run_hidden" {
		t.Fatalf("legacy reverse edge=%q err=%v", resumedBy, err)
	}
	for id, want := range map[string]struct {
		kind, visibility string
		pinned           int
	}{
		"task_hidden":    {ThreadKindInteraction, ThreadVisibilityUnlisted, 0},
		"task_pinned":    {ThreadKindWork, ThreadVisibilityListed, 1},
		"task_recurring": {ThreadKindRecurring, ThreadVisibilityListed, 0},
		"task_archived":  {ThreadKindWork, ThreadVisibilityArchived, 0},
	} {
		var kind, visibility string
		var pinned int
		if err := store.db.QueryRowContext(ctx, `SELECT kind, visibility, pinned FROM threads WHERE id = ?`, id).Scan(&kind, &visibility, &pinned); err != nil {
			t.Fatalf("thread %s: %v", id, err)
		}
		if kind != want.kind || visibility != want.visibility || pinned != want.pinned {
			t.Fatalf("thread %s kind=%q visibility=%q pinned=%d, want %+v", id, kind, visibility, pinned, want)
		}
	}
	var summary string
	if err := store.db.QueryRowContext(ctx, `SELECT summary FROM threads WHERE id = 'task_hidden'`).Scan(&summary); err != nil || summary != "inbox summary" {
		t.Fatalf("summary=%q err=%v", summary, err)
	}
	for _, table := range []string{"runs", "task_events", "task_handoffs", "task_artifacts", "approval_requests", "clarify_requests", "task_queue", "task_references"} {
		var orphans int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE COALESCE(thread_id, '') <> '' AND thread_id NOT IN (SELECT id FROM threads)`).Scan(&orphans); err != nil || orphans != 0 {
			t.Fatalf("%s orphans=%d err=%v", table, orphans, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := countMigrationBackups(t, dir); got != 1 {
		t.Fatalf("migration backups=%d, want 1", got)
	}

	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("reopen migrated fixture: %v", err)
	}
	defer reopened.Close()
	if reopened.SchemaStatus().Version != CurrentControlSchemaVersion || strings.TrimSpace(reopened.SchemaStatus().MigrationBackup) != "" {
		t.Fatalf("reopen status=%+v", reopened.SchemaStatus())
	}
	if got := countMigrationBackups(t, dir); got != 1 {
		t.Fatalf("idempotent reopen created another backup: %d", got)
	}
	assertFixtureCounts(reopened.db)
}

// TestMigrationInvariantsRejectOrphansAndParentEdgeChanges pins the safety net
// itself: a step that strands subordinate rows or changes the continuation edge
// count must fail, while dangling history that predates the upgrade is
// tolerated so an old database is never refused over it.
func TestMigrationInvariantsRejectOrphansAndResumeEdgeChanges(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE tasks (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, person_id TEXT NOT NULL);
		CREATE TABLE task_runs (id TEXT PRIMARY KEY, task_id TEXT NOT NULL, status TEXT NOT NULL, parent_run_id TEXT NOT NULL DEFAULT '');
		CREATE TABLE task_events (id TEXT PRIMARY KEY, task_id TEXT NOT NULL);
		CREATE TABLE approval_requests (id TEXT PRIMARY KEY, task_id TEXT, status TEXT NOT NULL, authorization_state TEXT, decision_recorded_at INTEGER);
		INSERT INTO tasks VALUES ('task_a', 'default', 'person_a'), ('task_b', 'default', 'person_a');
		INSERT INTO task_runs VALUES ('run_a', 'task_a', 'done', ''), ('run_b', 'task_b', 'done', 'run_a');
		INSERT INTO task_events VALUES ('evt_a', 'task_a'), ('evt_b', 'task_b');`); err != nil {
		t.Fatal(err)
	}
	before, err := captureMigrationInvariants(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyMigrationInvariants(ctx, db, before); err != nil {
		t.Fatalf("unchanged database failed verification: %v", err)
	}

	if _, err := db.ExecContext(ctx, `UPDATE task_events SET task_id = 'task_missing' WHERE id = 'evt_b'`); err != nil {
		t.Fatal(err)
	}
	if err := verifyMigrationInvariants(ctx, db, before); err == nil || !strings.Contains(err.Error(), "referencing a missing thread") || !strings.Contains(err.Error(), "task_events") {
		t.Fatalf("orphaned event error=%v", err)
	}
	// Dangling history that already existed before the step is not the step's
	// fault: a snapshot taken with the orphan present verifies cleanly.
	tolerated, err := captureMigrationInvariants(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyMigrationInvariants(ctx, db, tolerated); err != nil {
		t.Fatalf("pre-existing orphan was not tolerated: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE task_events SET task_id = 'task_b' WHERE id = 'evt_b'`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `UPDATE task_runs SET parent_run_id = '' WHERE id = 'run_b'`); err != nil {
		t.Fatal(err)
	}
	if err := verifyMigrationInvariants(ctx, db, before); err == nil || !strings.Contains(err.Error(), "run_resume_edges") {
		t.Fatalf("dropped resume edge error=%v", err)
	}
}
