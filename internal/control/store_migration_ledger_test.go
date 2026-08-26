package control

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
)

// schemaLedger returns the applied migration ledger, oldest first.
func schemaLedger(t *testing.T, s *Store) []struct {
	Version int
	Name    string
} {
	t.Helper()
	rows, err := s.db.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct {
		Version int
		Name    string
	}
	for rows.Next() {
		var entry struct {
			Version int
			Name    string
		}
		if err := rows.Scan(&entry.Version, &entry.Name); err != nil {
			t.Fatal(err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVersionFiveMigrationAddsSkillRepairHealthColumnsToVersionFourShape(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE skill_versions (
			control_tenant_id TEXT NOT NULL, skill_key TEXT NOT NULL, version_hash TEXT NOT NULL,
			state TEXT NOT NULL, PRIMARY KEY(control_tenant_id, skill_key, version_hash));
		CREATE TABLE skill_failure_guards (
			control_tenant_id TEXT NOT NULL, skill_key TEXT NOT NULL, version_hash TEXT NOT NULL,
			failure_signature TEXT NOT NULL,
			PRIMARY KEY(control_tenant_id, skill_key, version_hash, failure_signature));`); err != nil {
		t.Fatal(err)
	}
	var migration *schemaMigration
	for index := range orderedMigrations {
		if orderedMigrations[index].Version == 5 {
			migration = &orderedMigrations[index]
		}
	}
	if migration == nil {
		t.Fatal("v5 migration is missing")
	}
	if err := migration.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for table, columns := range map[string][]string{
		"skill_versions":       {"dependency_fingerprint", "verification_environment_fingerprint", "last_verified_at"},
		"skill_failure_guards": {"environment_fingerprint"},
	} {
		for _, column := range columns {
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&count); err != nil || count != 1 {
				t.Fatalf("%s.%s count=%d err=%v", table, column, count, err)
			}
		}
	}
	var snapshots int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='skill_candidate_evidence_snapshots'`).Scan(&snapshots); err != nil || snapshots != 1 {
		t.Fatalf("skill_candidate_evidence_snapshots count=%d err=%v", snapshots, err)
	}
}

// TestOrderedMigrationsCoverCurrentSchemaVersion pins the declaration itself:
// the steps stay sorted, carry unique versions, and the highest one is the
// version this binary claims to support. Without this, adding a table to
// InitSchema and bumping the constant compiles and passes while existing
// databases silently never receive the change.
func TestOrderedMigrationsCoverCurrentSchemaVersion(t *testing.T) {
	if len(orderedMigrations) == 0 {
		t.Fatal("orderedMigrations is empty; the baseline alone cannot reach a bumped version")
	}
	seen := map[int]bool{}
	sorted := true
	for i, migration := range orderedMigrations {
		if migration.Version <= schemaBaselineVersion {
			t.Errorf("migration %q has version %d, which is not above the baseline %d",
				migration.Name, migration.Version, schemaBaselineVersion)
		}
		if migration.Apply == nil {
			t.Errorf("migration %d (%q) has no Apply func", migration.Version, migration.Name)
		}
		if seen[migration.Version] {
			t.Errorf("duplicate migration version %d", migration.Version)
		}
		seen[migration.Version] = true
		if i > 0 && orderedMigrations[i-1].Version > migration.Version {
			sorted = false
		}
	}
	if !sorted {
		t.Error("orderedMigrations must be sorted by ascending Version")
	}
	highest := orderedMigrations[len(orderedMigrations)-1].Version
	if highest != CurrentControlSchemaVersion {
		t.Fatalf("highest ordered migration is %d but CurrentControlSchemaVersion is %d; every bump needs its own step",
			highest, CurrentControlSchemaVersion)
	}
}

// TestFreshControlStoreRecordsEveryAppliedVersion pins provenance for a brand
// new database. Recording only the final version left schema_migrations unable
// to describe what was applied, and left a future non-additive step with no
// slot to land in.
func TestFreshControlStoreRecordsEveryAppliedVersion(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ledger := schemaLedger(t, store)
	var versions []int
	for _, entry := range ledger {
		versions = append(versions, entry.Version)
	}
	want := []int{schemaBaselineVersion}
	for _, migration := range orderedMigrations {
		want = append(want, migration.Version)
	}
	sort.Ints(want)
	if len(versions) != len(want) {
		t.Fatalf("ledger versions=%v, want %v", versions, want)
	}
	for i := range want {
		if versions[i] != want[i] {
			t.Fatalf("ledger versions=%v, want %v", versions, want)
		}
	}
	if ledger[0].Version != schemaBaselineVersion || ledger[0].Name != "legacy-baseline" {
		t.Errorf("baseline row=%+v, want version %d named legacy-baseline", ledger[0], schemaBaselineVersion)
	}
	for i, migration := range orderedMigrations {
		entry := ledger[i+1]
		if entry.Version != migration.Version || entry.Name != migration.Name {
			t.Errorf("ledger row %d = %+v, want {%d %s}", i+1, entry, migration.Version, migration.Name)
		}
	}
}

// TestBaselineSchemaExcludesOrderedMigrationObjects proves the v2 object really
// moved out of InitSchema. If it stays in the baseline blob, an existing
// database at v1 is already "correct" by accident and the ordered step is
// untested dead code.
func TestBaselineSchemaExcludesOrderedMigrationObjects(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Drop the migration-owned object, then re-run ONLY the baseline. The object
	// must stay absent until the ordered step runs.
	if _, err := store.db.Exec(`DROP TABLE memory_governance_schedule`); err != nil {
		t.Fatal(err)
	}
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memory_governance_schedule'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("InitSchema still creates memory_governance_schedule; it belongs to the v2 ordered migration only")
	}

	for _, migration := range orderedMigrations {
		if migration.Version != 2 {
			continue
		}
		if err := migration.Apply(context.Background(), store.db); err != nil {
			t.Fatalf("apply v2: %v", err)
		}
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memory_governance_schedule'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 1 {
		t.Fatal("the v2 ordered migration did not create memory_governance_schedule")
	}
}

// TestLegacyUnversionedStoreRecordsBaselineThenSteps covers the real upgrade
// shape: an installed database with tables but no ledger. It must record the
// baseline as version 1 rather than labelling the newest version as the
// baseline.
func TestLegacyUnversionedStoreRecordsBaselineThenSteps(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE memory_governance_schedule`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("migrate unversioned store: %v", err)
	}
	defer migrated.Close()
	if migrated.SchemaStatus().Version != CurrentControlSchemaVersion {
		t.Fatalf("schema status=%+v", migrated.SchemaStatus())
	}
	ledger := schemaLedger(t, migrated)
	if len(ledger) < 2 {
		t.Fatalf("ledger=%+v, want a baseline row plus every ordered step", ledger)
	}
	if ledger[0].Version != schemaBaselineVersion || ledger[0].Name != "legacy-baseline" {
		t.Errorf("baseline row=%+v", ledger[0])
	}
	last := ledger[len(ledger)-1]
	if last.Version != CurrentControlSchemaVersion || last.Name == "legacy-baseline" {
		t.Errorf("final row=%+v; the newest version must carry its own migration name", last)
	}
}

// TestExistingBaselineRowIsNotRelabelled pins that an upgrade preserves the
// history a previous release wrote.
func TestExistingBaselineRowIsNotRelabelled(t *testing.T) {
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
	if _, err := store.db.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES (1, 'shipped-v1', 100)`); err != nil {
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
	ledger := schemaLedger(t, migrated)
	if len(ledger) != 1+len(orderedMigrations) {
		t.Fatalf("ledger=%+v", ledger)
	}
	if ledger[0].Name != "shipped-v1" {
		t.Errorf("existing baseline row was relabelled to %q", ledger[0].Name)
	}
	if ledger[len(ledger)-1].Version != CurrentControlSchemaVersion {
		t.Errorf("final row=%+v", ledger[len(ledger)-1])
	}
}
