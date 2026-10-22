package control

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// heldWriteLock is another connection holding the database's write lock, the
// way a running older daemon does while a new binary opens the same store.
type heldWriteLock struct {
	db   *sql.DB
	conn *sql.Conn
	once sync.Once
}

func holdWriteLock(t *testing.T, path string) *heldWriteLock {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`PRAGMA busy_timeout=5000`,
		`BEGIN IMMEDIATE`,
		`INSERT INTO persons (id, tenant_id, display_name, created_at, updated_at)
			VALUES ('person_concurrent_writer', 'default', 'Concurrent writer', 1, 1)`,
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	held := &heldWriteLock{db: db, conn: conn}
	t.Cleanup(func() { held.release(t) })
	return held
}

func (h *heldWriteLock) release(t *testing.T) {
	h.once.Do(func() {
		if _, err := h.conn.ExecContext(context.Background(), `COMMIT`); err != nil {
			t.Errorf("concurrent writer commit: %v", err)
		}
		_ = h.conn.Close()
		_ = h.db.Close()
	})
}

// walFixture loads the v25 row fixture in WAL mode, the journal an operator's
// store already uses.
func walFixture(t *testing.T) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	loadRowFixture(t, dir, releasedRowFixtures[2])
	path = filepath.Join(dir, "control.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode=%q err=%v", mode, err)
	}
	return dir, path
}

// releaseAfter frees the writer's lock after delay and reports when it did.
func releaseAfter(t *testing.T, writer *heldWriteLock, delay time.Duration) <-chan time.Time {
	released := make(chan time.Time, 1)
	timer := time.AfterFunc(delay, func() {
		writer.release(t)
		released <- time.Now()
	})
	t.Cleanup(func() { timer.Stop() })
	return released
}

// A step takes the write lock before its first read, so it waits for a writer
// that releases within the busy timeout. A step that began with a read could
// not take the lock while another connection held it and failed at once, so
// a new binary's first open failed while the old daemon wrote.
func TestStepWaitsForAConcurrentWriterWithinTheBusyTimeout(t *testing.T) {
	ctx := context.Background()
	_, path := walFixture(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		t.Fatal(err)
	}
	before, err := captureMigrationInvariants(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	released := releaseAfter(t, holdWriteLock(t, path), 300*time.Millisecond)
	if err := applyMigrationStep(ctx, db, version26Migration(t), before); err != nil {
		t.Fatalf("step under a writer that releases within the busy timeout: %v", err)
	}
	finished := time.Now()
	if releasedAt := <-released; finished.Before(releasedAt) {
		t.Fatal("the step committed while another connection held the write lock")
	}
	var version, writes int
	if err := db.QueryRow(`SELECT (SELECT MAX(version) FROM schema_migrations),
		(SELECT COUNT(*) FROM persons WHERE id = 'person_concurrent_writer')`).Scan(&version, &writes); err != nil || version != 26 || writes != 1 {
		t.Fatalf("after the wait: version %d, writer rows %d, err=%v", version, writes, err)
	}
}

// The whole upgrade waits for a concurrent writer as well and keeps its row.
func TestUpgradeWaitsForAConcurrentWriter(t *testing.T) {
	dir, path := walFixture(t)
	released := releaseAfter(t, holdWriteLock(t, path), 300*time.Millisecond)

	store, err := OpenStore(dir)
	opened := time.Now()
	if err != nil {
		t.Fatalf("upgrade under a writer that releases within the busy timeout: %v", err)
	}
	defer store.Close()
	if status := store.SchemaStatus(); status.Version != CurrentControlSchemaVersion {
		t.Fatalf("schema status = %+v", status)
	}
	if releasedAt := <-released; opened.Before(releasedAt) {
		t.Fatal("the upgrade finished while another connection held the write lock")
	}
	var writes int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM persons WHERE id = 'person_concurrent_writer'`).Scan(&writes); err != nil || writes != 1 {
		t.Fatalf("the concurrent writer's row = %d err=%v", writes, err)
	}
}

// A writer that outlasts the busy timeout fails the step cleanly: nothing is
// recorded or changed, and the step runs once the lock is free.
func TestStepPastTheBusyTimeoutLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	_, path := walFixture(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=150`); err != nil {
		t.Fatal(err)
	}
	before, err := captureMigrationInvariants(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	step := version26Migration(t)
	writer := holdWriteLock(t, path)
	if err := applyMigrationStep(ctx, db, step, before); err == nil {
		t.Fatal("a step ran while another connection held the write lock past the busy timeout")
	}
	var version, columns int
	readAftermath := func() {
		t.Helper()
		if err := db.QueryRow(`SELECT (SELECT MAX(version) FROM schema_migrations),
			(SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = 'admission_json')`).Scan(&version, &columns); err != nil {
			t.Fatal(err)
		}
	}
	readAftermath()
	if version != 25 || columns != 0 {
		t.Fatalf("a refused step left version %d and %d admission column(s)", version, columns)
	}
	writer.release(t)
	if err := applyMigrationStep(ctx, db, step, before); err != nil {
		t.Fatalf("step after the writer released: %v", err)
	}
	readAftermath()
	if version != 26 || columns != 1 {
		t.Fatalf("step after release left version %d and %d admission column(s)", version, columns)
	}
}
