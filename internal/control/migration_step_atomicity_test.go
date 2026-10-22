package control

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// withStepFault returns the ordered chain with one step followed by fault.
func withStepFault(version int, fault func(context.Context, migrationDB) error) []schemaMigration {
	chain := append([]schemaMigration(nil), orderedMigrations...)
	for i := range chain {
		if chain[i].Version != version {
			continue
		}
		apply := chain[i].Apply
		chain[i].Apply = func(ctx context.Context, db migrationDB) error {
			if err := apply(ctx, db); err != nil {
				return err
			}
			return fault(ctx, db)
		}
	}
	return chain
}

// openWithChain opens dir the way OpenStore does, with one connection, but
// migrates it with the given chain.
func openWithChain(t *testing.T, dir string, chain []schemaMigration) error {
	t.Helper()
	path := filepath.Join(dir, "control.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	store := &Store{db: db, dataDir: dir, events: newEventAppendBus()}
	return store.prepareAndMigrateSchema(context.Background(), dir, path, true, quickCheckDB, chain)
}

type stepAftermath struct {
	version, admissionColumns, uncertainClaims int
}

func readStepAftermath(t *testing.T, dir string) stepAftermath {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out stepAftermath
	if err := db.QueryRow(`SELECT (SELECT MAX(version) FROM schema_migrations),
		(SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = 'admission_json'),
		(SELECT COUNT(*) FROM external_effect_claims WHERE state = 'uncertain')`).Scan(
		&out.version, &out.admissionColumns, &out.uncertainClaims); err != nil {
		t.Fatal(err)
	}
	return out
}

// A step's schema change, its invariant check, and its ledger row commit
// together. A step whose result fails verification leaves nothing behind, so
// a later open runs the step, and its check, again instead of trusting it.
func TestFailedStepVerificationLeavesNoTraceAndBlocksTheNextOpen(t *testing.T) {
	for name, fault := range map[string]string{
		"fabricated admission snapshots": `UPDATE runs SET admission_json = '{"version":1}'`,
		"settled historical effect":      `UPDATE external_effect_claims SET state = 'observed', observation_ref = 'step' WHERE state = 'uncertain'`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			loadRowFixture(t, dir, releasedRowFixtures[2])
			before := readStepAftermath(t, dir)
			if before.version != 25 || before.uncertainClaims == 0 {
				t.Fatalf("fixture is not the v25 shape with an uncertain claim: %+v", before)
			}
			faulty := withStepFault(26, func(ctx context.Context, db migrationDB) error {
				_, err := db.ExecContext(ctx, fault)
				return err
			})
			for attempt := 1; attempt <= 2; attempt++ {
				if err := openWithChain(t, dir, faulty); err == nil {
					t.Fatalf("attempt %d accepted a step that failed verification", attempt)
				}
				if after := readStepAftermath(t, dir); after != before {
					t.Fatalf("attempt %d left the failed step behind: before=%+v after=%+v", attempt, before, after)
				}
			}
			store, err := OpenStore(dir)
			if err != nil {
				t.Fatalf("a sound chain after the refused step: %v", err)
			}
			defer store.Close()
			var fabricated int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE admission_json <> ''`).Scan(&fabricated); err != nil || fabricated != 0 {
				t.Fatalf("fabricated snapshots=%d err=%v", fabricated, err)
			}
			if after := readStepAftermath(t, dir); after.version != CurrentControlSchemaVersion || after.uncertainClaims != before.uncertainClaims {
				t.Fatalf("sound upgrade = %+v", after)
			}
		})
	}
}

// A step that fails partway keeps none of its earlier statements.
func TestFailedStepKeepsNoPartialSchema(t *testing.T) {
	dir := t.TempDir()
	loadRowFixture(t, dir, releasedRowFixtures[2])
	before := readStepAftermath(t, dir)
	broken := withStepFault(26, func(context.Context, migrationDB) error { return errors.New("disk full") })
	if err := openWithChain(t, dir, broken); err == nil {
		t.Fatal("a failing step was accepted")
	}
	if after := readStepAftermath(t, dir); after != before {
		t.Fatalf("a failing step left part of its schema: before=%+v after=%+v", before, after)
	}
}
