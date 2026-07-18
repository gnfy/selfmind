package cliapp

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/control"
)

// openSeededLegacyDB creates <dataDir>/default/memory.db with a minimal
// schema matching the provider's column set for the five migrated tables
// (see internal/kernel/memory/sqlite_provider.go) and returns the open db.
func openSeededLegacyDB(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	legacyDir := filepath.Join(dataDir, "default")
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(legacyDir, "memory.db"))
	if err != nil {
		t.Fatalf("open legacy memory.db: %v", err)
	}
	db.SetMaxOpenConns(1)
	ddl := []string{
		`CREATE TABLE facts (
			id TEXT PRIMARY KEY, target TEXT, content TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			source TEXT DEFAULT '', scope TEXT DEFAULT '', confidence REAL DEFAULT 0,
			created_from_run TEXT DEFAULT '', last_verified_at DATETIME)`,
		`CREATE TABLE canonical_memories (
			id TEXT PRIMARY KEY, target TEXT NOT NULL, scope TEXT DEFAULT '',
			category TEXT DEFAULT '', content TEXT NOT NULL, normalized_hash TEXT NOT NULL,
			status TEXT DEFAULT 'active', pinned INTEGER DEFAULT 0, user_confirmed INTEGER DEFAULT 0,
			confidence REAL DEFAULT 0, evidence_count INTEGER DEFAULT 0, occurrences INTEGER DEFAULT 0,
			last_verified_at INTEGER, last_accessed_at INTEGER, valid_from INTEGER, valid_until INTEGER,
			superseded_by TEXT DEFAULT '', revision INTEGER DEFAULT 1, created_at INTEGER, updated_at INTEGER)`,
		`CREATE TABLE memory_observations (
			id TEXT PRIMARY KEY, run_id TEXT DEFAULT '', analyzer_version INTEGER DEFAULT 0,
			workspace_id TEXT DEFAULT '', target TEXT NOT NULL, scope TEXT DEFAULT '',
			source TEXT DEFAULT '', content TEXT NOT NULL, normalized_hash TEXT NOT NULL,
			confidence_prior REAL DEFAULT 0, status TEXT DEFAULT 'accepted', created_at INTEGER)`,
		`CREATE TABLE memory_evidence (
			memory_id TEXT NOT NULL, observation_id TEXT NOT NULL, relation TEXT NOT NULL,
			created_at INTEGER, PRIMARY KEY(memory_id, observation_id, relation))`,
		`CREATE TABLE memory_events (
			id TEXT PRIMARY KEY, actor TEXT NOT NULL, action TEXT NOT NULL,
			memory_id TEXT DEFAULT '', observation_id TEXT DEFAULT '',
			confidence REAL DEFAULT 0, snapshot TEXT DEFAULT '', detail TEXT DEFAULT '', created_at INTEGER)`,
		`CREATE INDEX idx_observations_hash ON memory_observations(target, scope, normalized_hash)`,
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed ddl: %v", err)
		}
	}
	return db
}

// seedControlRun creates one identity, task, and run in the control store and
// returns the person and run ids the migration must resolve.
func seedControlRun(t *testing.T, ctx context.Context, store *control.Store, platformUserID string) (personID, runID string) {
	t.Helper()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", platformUserID, "User "+platformUserID)
	if err != nil {
		t.Fatalf("identity %s: %v", platformUserID, err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "seed task",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatalf("task %s: %v", platformUserID, err)
	}
	run, err := store.StartRun(ctx, task, "cli", "seed run")
	if err != nil {
		t.Fatalf("run %s: %v", platformUserID, err)
	}
	return identity.PersonID, run.ID
}

func seedRows(t *testing.T, db *sql.DB, seed []struct {
	stmt string
	args []interface{}
}) {
	t.Helper()
	for _, s := range seed {
		if _, err := db.Exec(s.stmt, s.args...); err != nil {
			t.Fatalf("seed row: %v", err)
		}
	}
}

// newMigrationFixture builds a data dir with a real control.db (one identity,
// one run) and a legacy default/memory.db seeded with rows that are
// resolvable, unresolvable, and run-less. It returns the data dir, the person
// id the resolvable rows must move to, and the real run id.
func newMigrationFixture(t *testing.T) (dataDir, personID, runID string) {
	t.Helper()
	ctx := context.Background()
	dataDir = t.TempDir()

	store, err := control.OpenStore(dataDir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	personID, runID = seedControlRun(t, ctx, store, "local")

	db := openSeededLegacyDB(t, dataDir)
	defer db.Close()
	seedRows(t, db, []struct {
		stmt string
		args []interface{}
	}{
		// One resolvable fact, one referencing a run that no longer exists,
		// one legacy fact with no run reference at all.
		{`INSERT INTO facts (id, target, content, created_from_run) VALUES ('fact-run', 'user', 'movable fact', ?)`,
			[]interface{}{runID}},
		{`INSERT INTO facts (id, target, content, created_from_run) VALUES ('fact-unknown', 'user', 'orphan run fact', 'run-missing')`, nil},
		{`INSERT INTO facts (id, target, content, created_from_run) VALUES ('fact-norun', 'user', 'pre-attribution fact', '')`, nil},
		// canon-1 resolves through evidence -> obs-1 -> run; canon-2 has no
		// evidence and must stay.
		{`INSERT INTO canonical_memories (id, target, content, normalized_hash) VALUES ('canon-1', 'user', 'movable canonical', 'h1')`, nil},
		{`INSERT INTO canonical_memories (id, target, content, normalized_hash) VALUES ('canon-2', 'user', 'stranded canonical', 'h2')`, nil},
		{`INSERT INTO memory_observations (id, run_id, target, content, normalized_hash) VALUES ('obs-1', ?, 'user', 'evidence obs', 'h1')`,
			[]interface{}{runID}},
		{`INSERT INTO memory_observations (id, run_id, target, content, normalized_hash) VALUES ('obs-2', ?, 'user', 'orphan obs', 'h3')`,
			[]interface{}{runID}},
		{`INSERT INTO memory_observations (id, run_id, target, content, normalized_hash) VALUES ('obs-3', '', 'user', 'legacy obs', 'h4')`, nil},
		{`INSERT INTO memory_evidence (memory_id, observation_id, relation) VALUES ('canon-1', 'obs-1', 'supports')`, nil},
		{`INSERT INTO memory_events (id, actor, action, memory_id) VALUES ('evt-1', 'judge', 'add', 'canon-1')`, nil},
		{`INSERT INTO memory_events (id, actor, action, memory_id) VALUES ('evt-2', 'judge', 'add', 'canon-2')`, nil},
	})
	return dataDir, personID, runID
}

func countRows(t *testing.T, dbPath, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s in %s: %v", table, dbPath, err)
	}
	return n
}

func assertReportCounts(t *testing.T, report *memoryMigrationReport, personID string) {
	t.Helper()
	c, ok := report.Persons[personID]
	if !ok {
		t.Fatalf("report has no entry for person %s: %+v", personID, report.Persons)
	}
	if c.Facts != 1 || c.Canonicals != 1 || c.Observations != 2 || c.Evidence != 1 || c.Events != 1 {
		t.Fatalf("person counts = %+v, want facts=1 canonicals=1 observations=2 evidence=1 events=1", c)
	}
	if report.UnresolvedFacts != 1 || report.UnresolvedCanonicals != 1 || report.UnresolvedObservations != 0 {
		t.Fatalf("unresolved = facts=%d canonicals=%d observations=%d, want 1/1/0",
			report.UnresolvedFacts, report.UnresolvedCanonicals, report.UnresolvedObservations)
	}
	if report.FactsWithoutRun != 1 {
		t.Fatalf("facts without run = %d, want 1", report.FactsWithoutRun)
	}
}

// TestMemoryMigrationDryRun is the safety contract: without --apply the
// report shows exactly what would move but no db is created or modified.
func TestMemoryMigrationDryRun(t *testing.T) {
	ctx := context.Background()
	dataDir, personID, _ := newMigrationFixture(t)

	report, err := runMemoryMigration(ctx, memoryMigrationOptions{dataDir: dataDir, apply: false})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if report.Applied {
		t.Fatal("dry-run report marked as applied")
	}
	assertReportCounts(t, report, personID)

	if _, err := os.Stat(filepath.Join(dataDir, personID, "memory.db")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not create the person db (stat err = %v)", err)
	}
	sourcePath := filepath.Join(dataDir, "default", "memory.db")
	if n := countRows(t, sourcePath, "facts"); n != 3 {
		t.Fatalf("dry-run changed source facts: %d, want 3", n)
	}
	if n := countRows(t, sourcePath, "canonical_memories"); n != 2 {
		t.Fatalf("dry-run changed source canonicals: %d, want 2", n)
	}
}

// TestMemoryMigrationApply moves resolvable rows to the person partition,
// leaves unresolvable rows in the legacy partition, and is idempotent: a
// second apply reports zero moved rows and changes nothing.
func TestMemoryMigrationApply(t *testing.T) {
	ctx := context.Background()
	dataDir, personID, _ := newMigrationFixture(t)

	report, err := runMemoryMigration(ctx, memoryMigrationOptions{dataDir: dataDir, apply: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !report.Applied {
		t.Fatal("apply report not marked as applied")
	}
	assertReportCounts(t, report, personID)

	personPath := filepath.Join(dataDir, personID, "memory.db")
	wantPerson := map[string]int{
		"facts":               1,
		"canonical_memories":  1,
		"memory_observations": 2,
		"memory_evidence":     1,
		"memory_events":       1,
	}
	for table, want := range wantPerson {
		if got := countRows(t, personPath, table); got != want {
			t.Fatalf("person db %s = %d rows, want %d", table, got, want)
		}
	}

	sourcePath := filepath.Join(dataDir, "default", "memory.db")
	wantSource := map[string]int{
		"facts":               2, // fact-unknown + fact-norun stay
		"canonical_memories":  1, // canon-2 stays
		"memory_observations": 1, // obs-3 (no run) stays
		"memory_evidence":     0,
		"memory_events":       1, // evt-2 follows the stranded canon-2
	}
	for table, want := range wantSource {
		if got := countRows(t, sourcePath, table); got != want {
			t.Fatalf("legacy db %s = %d rows, want %d", table, got, want)
		}
	}

	// Second apply: nothing left to move, and nothing changes.
	again, err := runMemoryMigration(ctx, memoryMigrationOptions{dataDir: dataDir, apply: true})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if c := again.Persons[personID]; !c.empty() {
		t.Fatalf("second apply moved rows: %+v", c)
	}
	for table, want := range wantPerson {
		if got := countRows(t, personPath, table); got != want {
			t.Fatalf("person db %s changed on rerun: %d, want %d", table, got, want)
		}
	}
	for table, want := range wantSource {
		if got := countRows(t, sourcePath, table); got != want {
			t.Fatalf("legacy db %s changed on rerun: %d, want %d", table, got, want)
		}
	}
}

// TestMemoryMigrationSplitEvidenceStays is the cross-database consistency
// contract: a canonical whose evidence-linked observations resolve to two
// different persons, or partly to no person at all, must keep its ENTIRE
// component (canonical, evidence rows, events, and all linked observations)
// in the legacy partition. Nothing may move, or a relation would dangle
// across two databases.
func TestMemoryMigrationSplitEvidenceStays(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	store, err := control.OpenStore(dataDir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	personA, runA := seedControlRun(t, ctx, store, "local-a")
	personB, runB := seedControlRun(t, ctx, store, "local-b")
	if personA == personB {
		t.Fatalf("fixture must produce two persons, got one: %s", personA)
	}

	db := openSeededLegacyDB(t, dataDir)
	defer db.Close()
	seedRows(t, db, []struct {
		stmt string
		args []interface{}
	}{
		// canon-split: evidence spans runs of two different persons.
		{`INSERT INTO canonical_memories (id, target, content, normalized_hash) VALUES ('canon-split', 'user', 'split canonical', 'h1')`, nil},
		{`INSERT INTO memory_observations (id, run_id, target, content, normalized_hash) VALUES ('obs-a', ?, 'user', 'obs person a', 'ha')`,
			[]interface{}{runA}},
		{`INSERT INTO memory_observations (id, run_id, target, content, normalized_hash) VALUES ('obs-b', ?, 'user', 'obs person b', 'hb')`,
			[]interface{}{runB}},
		{`INSERT INTO memory_evidence (memory_id, observation_id, relation) VALUES ('canon-split', 'obs-a', 'supports')`, nil},
		{`INSERT INTO memory_evidence (memory_id, observation_id, relation) VALUES ('canon-split', 'obs-b', 'supports')`, nil},
		{`INSERT INTO memory_events (id, actor, action, memory_id) VALUES ('evt-split', 'judge', 'add', 'canon-split')`, nil},
		// canon-partial: one observation resolves, the other references a
		// run that no longer exists.
		{`INSERT INTO canonical_memories (id, target, content, normalized_hash) VALUES ('canon-partial', 'user', 'partial canonical', 'h2')`, nil},
		{`INSERT INTO memory_observations (id, run_id, target, content, normalized_hash) VALUES ('obs-c', ?, 'user', 'obs resolvable', 'hc')`,
			[]interface{}{runA}},
		{`INSERT INTO memory_observations (id, run_id, target, content, normalized_hash) VALUES ('obs-d', 'run-missing', 'user', 'obs unresolvable', 'hd')`, nil},
		{`INSERT INTO memory_evidence (memory_id, observation_id, relation) VALUES ('canon-partial', 'obs-c', 'supports')`, nil},
		{`INSERT INTO memory_evidence (memory_id, observation_id, relation) VALUES ('canon-partial', 'obs-d', 'supports')`, nil},
		{`INSERT INTO memory_events (id, actor, action, memory_id) VALUES ('evt-partial', 'judge', 'add', 'canon-partial')`, nil},
	})

	report, err := runMemoryMigration(ctx, memoryMigrationOptions{dataDir: dataDir, apply: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for person, c := range report.Persons {
		if !c.empty() {
			t.Fatalf("split-evidence component moved rows to %s: %+v", person, c)
		}
	}
	if report.UnresolvedCanonicals != 2 {
		t.Fatalf("unresolved canonicals = %d, want 2", report.UnresolvedCanonicals)
	}
	// obs-a, obs-b, obs-c, obs-d all reference a run but are stuck with
	// their unresolvable components.
	if report.UnresolvedObservations != 4 {
		t.Fatalf("unresolved observations = %d, want 4", report.UnresolvedObservations)
	}

	sourcePath := filepath.Join(dataDir, "default", "memory.db")
	wantSource := map[string]int{
		"canonical_memories":  2,
		"memory_observations": 4,
		"memory_evidence":     4,
		"memory_events":       2,
	}
	for table, want := range wantSource {
		if got := countRows(t, sourcePath, table); got != want {
			t.Fatalf("legacy db %s = %d rows, want %d (component must stay whole)", table, got, want)
		}
	}
	for _, person := range []string{personA, personB} {
		if _, err := os.Stat(filepath.Join(dataDir, person, "memory.db")); !os.IsNotExist(err) {
			t.Fatalf("no person db may be created for %s (stat err = %v)", person, err)
		}
	}
}
