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
