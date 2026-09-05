package controltest_test

import (
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
)

// TestSeededSchemaMatchesAFreshDatabase is the reason the shortcut is allowed at
// all. Seeding from a template is only sound while the seeded database is
// indistinguishable from one the real creation path built: a suite that runs
// fast against a schema production never has is worse than a slow one. Every
// object AND the migration ledger must match, so a future migration that the
// template somehow misses fails here instead of hiding behind green tests.
func TestSeededSchemaMatchesAFreshDatabase(t *testing.T) {
	freshDir := t.TempDir()
	fresh, err := control.OpenStore(freshDir)
	if err != nil {
		t.Fatalf("create a store the real way: %v", err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}

	seededDir := t.TempDir()
	controltest.SeedDir(t, seededDir)

	freshObjects := schemaObjects(t, freshDir)
	seededObjects := schemaObjects(t, seededDir)
	if len(freshObjects) == 0 {
		t.Fatal("fresh database reported no schema objects; the comparison would be vacuous")
	}
	if diff := describeDiff(freshObjects, seededObjects); diff != "" {
		t.Fatalf("seeded schema differs from a freshly created one:\n%s", diff)
	}

	freshLedger := migrationLedger(t, freshDir)
	seededLedger := migrationLedger(t, seededDir)
	if len(freshLedger) == 0 {
		t.Fatal("fresh database recorded no migrations; the comparison would be vacuous")
	}
	if diff := describeDiff(freshLedger, seededLedger); diff != "" {
		t.Fatalf("seeded migration ledger differs:\n%s", diff)
	}

	// And the version the binary reports must be the current one, not whatever
	// the template happened to be built at.
	seeded := controltest.NewStoreInDir(t, t.TempDir())
	if got := seeded.SchemaStatus().Version; got != control.CurrentControlSchemaVersion {
		t.Fatalf("seeded store reports schema v%d, want v%d", got, control.CurrentControlSchemaVersion)
	}
}

// TestSeededStoresAreIsolated: sharing one database would be faster still and
// completely wrong. Each store must start empty and never see another's rows.
func TestSeededStoresAreIsolated(t *testing.T) {
	ctx := t.Context()
	a := controltest.NewStore(t)
	b := controltest.NewStore(t)

	if _, err := a.CreateTask(ctx, control.TaskCreate{
		TenantID: "default", PersonID: "p1", Title: "only in a", Channel: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	tasks, err := b.ListTasks(ctx, "default", "p1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("a seeded store must start empty; it saw %d task(s) from another store", len(tasks))
	}
}

func schemaObjects(t *testing.T, dir string) []string {
	t.Helper()
	db := openRaw(t, dir)
	rows, err := db.Query(`SELECT type, name, COALESCE(sql, '') FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, name, ddl string
		if err := rows.Scan(&kind, &name, &ddl); err != nil {
			t.Fatal(err)
		}
		out = append(out, kind+" "+name+" :: "+normalizeDDL(ddl))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func migrationLedger(t *testing.T, dir string) []string {
	t.Helper()
	db := openRaw(t, dir)
	rows, err := db.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
		_ = version
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func openRaw(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatalf("open %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// normalizeDDL collapses whitespace so formatting differences never read as a
// schema difference.
func normalizeDDL(ddl string) string {
	return strings.Join(strings.Fields(ddl), " ")
}

func describeDiff(want, got []string) string {
	if strings.Join(want, "\n") == strings.Join(got, "\n") {
		return ""
	}
	inWant := map[string]bool{}
	for _, item := range want {
		inWant[item] = true
	}
	inGot := map[string]bool{}
	for _, item := range got {
		inGot[item] = true
	}
	var sb strings.Builder
	for _, item := range want {
		if !inGot[item] {
			sb.WriteString("  missing from seeded: " + item + "\n")
		}
	}
	for _, item := range got {
		if !inWant[item] {
			sb.WriteString("  only in seeded:      " + item + "\n")
		}
	}
	return sb.String()
}
