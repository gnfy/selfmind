package control

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedReleasedV11Database writes the released v11 schema plus two runs — one
// carrying the forward continuation edge, one without — and records the ledger
// rows named by ledgerThrough. ledgerThrough of 0 means "ledger lost", the
// shape-adoption case.
func seedReleasedV11Database(t *testing.T, ledgerThrough int) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ddl, err := os.ReadFile(filepath.Join("testdata", "control-v11-schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(string(ddl)), "INSERT") {
		t.Fatal("the v11 fixture must be schema only")
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, execErr := db.ExecContext(ctx, query, args...); execErr != nil {
			t.Fatalf("%s: %v", query, execErr)
		}
	}
	exec(string(ddl))
	if ledgerThrough > 0 {
		exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, 'legacy-baseline', 1)`, schemaBaselineVersion)
		for _, migration := range orderedMigrations {
			if migration.Version > ledgerThrough {
				break
			}
			exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`, migration.Version, migration.Name, migration.Version)
		}
	}
	exec(`INSERT INTO threads(id, tenant_id, person_id, kind, visibility, title, summary, pinned, created_at, updated_at, last_activity_at)
		VALUES ('thread_a', 'default', 'person_a', 'work', 'listed', 'release', '', 0, 1, 2, 2)`)
	exec(`INSERT INTO runs(id, thread_id, tenant_id, person_id, channel, input_summary, status, started_at, parent_run_id)
		VALUES ('run_target', 'thread_a', 'default', 'person_a', 'cli', 'first attempt', 'waiting_user', 1, '')`)
	exec(`INSERT INTO runs(id, thread_id, tenant_id, person_id, channel, input_summary, status, started_at, parent_run_id)
		VALUES ('run_resumer', 'thread_a', 'default', 'person_a', 'cli', 'continued', 'done', 2, 'run_target')`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertResumeEdgeRenamed(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	if status := store.SchemaStatus(); status.Version != CurrentControlSchemaVersion {
		t.Fatalf("version=%d want %d", status.Version, CurrentControlSchemaVersion)
	}
	var edge string
	if err := store.db.QueryRowContext(ctx,
		`SELECT COALESCE(resumes_run_id, '') FROM runs WHERE id = 'run_resumer'`).Scan(&edge); err != nil {
		t.Fatalf("read renamed edge: %v", err)
	}
	if edge != "run_target" {
		t.Fatalf("resume edge=%q want run_target", edge)
	}
	var total int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&total); err != nil || total != 2 {
		t.Fatalf("runs=%d err=%v, want both seeded rows", total, err)
	}
	for _, gone := range []struct{ kind, name string }{
		{"index", "idx_task_runs_parent_once"},
	} {
		var count int
		if err := store.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type=? AND name=?`, gone.kind, gone.name).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s %s survived: count=%d err=%v", gone.kind, gone.name, count, err)
		}
	}
	var newIndex int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_runs_resumes_once'`).Scan(&newIndex); err != nil || newIndex != 1 {
		t.Fatalf("renamed unique index missing: count=%d err=%v", newIndex, err)
	}
	var oldColumn int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name='parent_run_id'`).Scan(&oldColumn); err != nil || oldColumn != 0 {
		t.Fatalf("old column survived: count=%d err=%v", oldColumn, err)
	}
}

// TestReleasedV11FixtureUpgradesResumeEdge is the released-upgrade gate: v11 is
// the last publicly shipped schema, so a real v11 install must reach the
// current version with its continuation edges intact.
func TestReleasedV11FixtureUpgradesResumeEdge(t *testing.T) {
	store, err := OpenStore(seedReleasedV11Database(t, threadedWorkHistorySchemaVersion))
	if err != nil {
		t.Fatalf("open/migrate released v11: %v", err)
	}
	defer store.Close()
	assertResumeEdgeRenamed(t, store)
	if strings.TrimSpace(store.SchemaStatus().MigrationBackup) == "" {
		t.Fatal("a released upgrade must leave a pre-migration backup")
	}
}

// TestModernShapeAdoptionStillRunsLaterMigrations pins the defect that a fresh
// database can never expose: shape adoption exists so a v11 database with a
// lost ledger skips the legacy additive InitSchema. When it claimed the WHOLE
// ledger it also stamped every later step as applied without running it, so a
// released v11 install silently kept the pre-v12 column while reporting the
// current version. Adoption may only claim the shape it recognizes.
func TestModernShapeAdoptionStillRunsLaterMigrations(t *testing.T) {
	store, err := OpenStore(seedReleasedV11Database(t, 0))
	if err != nil {
		t.Fatalf("open/migrate ledgerless v11: %v", err)
	}
	defer store.Close()
	assertResumeEdgeRenamed(t, store)
	for _, retired := range []string{"tasks", "task_runs", "current_task"} {
		var count int
		if err := store.db.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, retired).Scan(&count); err != nil || count != 0 {
			t.Fatalf("adoption ran legacy InitSchema and recreated %s: count=%d err=%v", retired, count, err)
		}
	}
}

// TestArchivedThreadUpgradesToRunDismissal pins v13: Attention no longer reads
// the Thread row, so archiving must survive as the bulk dismissal it
// effectively was. Without the conversion, upgrading would resurface work the
// person had already put away.
func TestArchivedThreadUpgradesToRunDismissal(t *testing.T) {
	ctx := context.Background()
	dir := seedReleasedV11Database(t, threadedWorkHistorySchemaVersion)
	legacy, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, execErr := legacy.ExecContext(ctx, query, args...); execErr != nil {
			t.Fatalf("%s: %v", query, execErr)
		}
	}
	exec(`INSERT INTO threads(id, tenant_id, person_id, kind, visibility, title, summary, pinned, created_at, updated_at, last_activity_at)
		VALUES ('thread_archived', 'default', 'person_a', 'work', 'archived', 'put away', '', 0, 1, 2, 2)`)
	exec(`INSERT INTO runs(id, thread_id, tenant_id, person_id, channel, input_summary, status, started_at, parent_run_id)
		VALUES ('run_archived', 'thread_archived', 'default', 'person_a', 'cli', 'parked but archived', 'waiting_user', 3, '')`)
	exec(`INSERT INTO threads(id, tenant_id, person_id, kind, visibility, title, summary, pinned, created_at, updated_at, last_activity_at)
		VALUES ('thread_open', 'default', 'person_a', 'work', 'listed', 'still open', '', 0, 1, 2, 2)`)
	exec(`INSERT INTO runs(id, thread_id, tenant_id, person_id, channel, input_summary, status, started_at, parent_run_id)
		VALUES ('run_open', 'thread_open', 'default', 'person_a', 'cli', 'parked and visible', 'waiting_user', 4, '')`)
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("open/migrate: %v", err)
	}
	defer store.Close()

	var dismissedAt int64
	var dismissedBy string
	if err := store.db.QueryRowContext(ctx,
		`SELECT COALESCE(attention_dismissed_at, 0), COALESCE(attention_dismissed_by, '') FROM runs WHERE id = 'run_archived'`).
		Scan(&dismissedAt, &dismissedBy); err != nil {
		t.Fatal(err)
	}
	if dismissedAt == 0 || dismissedBy != "retention" {
		t.Fatalf("archived thread did not become a run dismissal: at=%d by=%q", dismissedAt, dismissedBy)
	}
	var openDismissed int64
	if err := store.db.QueryRowContext(ctx,
		`SELECT COALESCE(attention_dismissed_at, 0) FROM runs WHERE id = 'run_open'`).Scan(&openDismissed); err != nil {
		t.Fatal(err)
	}
	if openDismissed != 0 {
		t.Fatalf("a listed thread's run must not be dismissed: at=%d", openDismissed)
	}

	items, _, err := NewWorkTimeline(store).AttentionPage(ctx, "default", "person_a", "", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.RunID == "run_archived" {
			t.Fatal("an archived thread's run resurfaced in Attention")
		}
	}
	found := false
	for _, item := range items {
		if item.RunID == "run_open" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the visible parked run must still be Attention: %+v", items)
	}
}
