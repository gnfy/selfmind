package control

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// The v6 step must create the attribution table on a v5-shaped database rather
// than only on a fresh one: an existing install never re-runs InitSchema.
func TestVersionSixMigrationAddsSkillAttributionToVersionFiveShape(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE run_skill_activations (
			control_tenant_id TEXT NOT NULL, run_id TEXT NOT NULL, work_unit_id TEXT NOT NULL,
			skill_key TEXT NOT NULL, skill_name TEXT NOT NULL, state TEXT NOT NULL);`); err != nil {
		t.Fatal(err)
	}
	var migration *schemaMigration
	for index := range orderedMigrations {
		if orderedMigrations[index].Version == 6 {
			migration = &orderedMigrations[index]
		}
	}
	if migration == nil {
		t.Fatal("v6 migration is missing")
	}
	if err := migration.Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='skill_attributions'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("skill_attributions count=%d err=%v", tables, err)
	}
	// Historical rows stay inert: the step adds no column to an existing table
	// and rewrites nothing.
	var activations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM run_skill_activations`).Scan(&activations); err != nil || activations != 0 {
		t.Fatalf("activations disturbed: count=%d err=%v", activations, err)
	}
	if err := migration.Apply(context.Background(), db); err != nil {
		t.Fatalf("v6 step is not idempotent: %v", err)
	}
}

func attributionFixture(t *testing.T) (*Store, context.Context) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, context.Background()
}

// De-duplication is per work unit and keyed by package path, so a work unit that
// reads two Skills records two rows and one that reads the same Skill twice
// records one.
func TestSkillAttributionDeduplicatesPerWorkUnit(t *testing.T) {
	store, ctx := attributionFixture(t)
	base := SkillAttribution{
		ControlTenantID: "default", PersonID: "person-1",
		RunID: "run-1", WorkUnitID: "unit-1", ObservedAt: time.Unix(1000, 0),
	}
	first := base
	first.SkillKey, first.SkillName, first.PackagePath = "key-a", "alpha", "/pkgs/alpha"
	second := base
	second.SkillKey, second.SkillName, second.PackagePath = "key-b", "beta", "/pkgs/beta"

	for _, record := range []SkillAttribution{first, second} {
		inserted, err := store.RecordSkillAttribution(ctx, record)
		if err != nil || !inserted {
			t.Fatalf("first observation of %s: inserted=%v err=%v", record.SkillName, inserted, err)
		}
	}
	repeat := first
	repeat.ObservedAt = time.Unix(2000, 0)
	inserted, err := store.RecordSkillAttribution(ctx, repeat)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("a repeated observation in the same work unit must not add a row")
	}

	// A later work unit is a separate observation.
	nextUnit := first
	nextUnit.WorkUnitID = "unit-2"
	if inserted, err := store.RecordSkillAttribution(ctx, nextUnit); err != nil || !inserted {
		t.Fatalf("next work unit: inserted=%v err=%v", inserted, err)
	}

	summaries, err := store.SkillAttributionSummaries(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, summary := range summaries {
		counts[summary.SkillKey] = summary.Attributions
	}
	if counts["key-a"] != 2 || counts["key-b"] != 1 {
		t.Fatalf("unexpected counts: %v", counts)
	}
}

// Attribution requires work-unit identity, because the work unit is the
// granularity outcome, verification, and activation already use.
func TestSkillAttributionRequiresWorkUnitIdentity(t *testing.T) {
	store, ctx := attributionFixture(t)
	if _, err := store.RecordSkillAttribution(ctx, SkillAttribution{
		ControlTenantID: "default", RunID: "run-1", PackagePath: "/pkgs/alpha",
	}); err == nil {
		t.Fatal("missing work unit was accepted")
	}
	if _, err := store.RecordSkillAttribution(ctx, SkillAttribution{
		ControlTenantID: "default", RunID: "run-1", WorkUnitID: "unit-1",
	}); err == nil {
		t.Fatal("missing package path was accepted")
	}
}

// A Skill used only implicitly has no activation row, which is exactly what the
// implicit column exists to show; and attribution never inflates Calls.
func TestSkillStatsReportImplicitUseSeparately(t *testing.T) {
	store, ctx := attributionFixture(t)
	if _, err := store.RecordSkillAttribution(ctx, SkillAttribution{
		ControlTenantID: "default", PersonID: "person-1", RunID: "run-1", WorkUnitID: "unit-1",
		SkillKey: "key-a", SkillName: "alpha", PackagePath: "/pkgs/alpha",
		ObservedAt: time.Unix(1700000000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	stats, err := store.SkillUsageStats(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected the implicitly used Skill to appear: %+v", stats)
	}
	if stats[0].SkillName != "alpha" || stats[0].Attributions != 1 {
		t.Fatalf("unexpected projection: %+v", stats[0])
	}
	if stats[0].Calls != 0 {
		t.Fatalf("attribution must not inflate activation calls: %+v", stats[0])
	}
	if !stats[0].LastUsedAt.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("recency not carried: %+v", stats[0])
	}
}

// Attribution alone never becomes curator or repair evidence. It freezes no
// version, package hash, or resource manifest, and the thresholds are defined
// over snapshots attached to an exact version.
func TestSkillAttributionIsNotCuratorEvidence(t *testing.T) {
	store, ctx := attributionFixture(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "attribution-user", "Attribution User")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "implicit use", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "implicit use")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordSkillAttribution(ctx, SkillAttribution{
		ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: run.ID, WorkUnitID: run.WorkUnitID,
		SkillKey: "key-a", SkillName: "alpha", PackagePath: "/pkgs/alpha",
	}); err != nil {
		t.Fatal(err)
	}
	digests, err := store.ReadySkillEvidenceDigestsForRun(ctx, identity.TenantID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(digests) != 0 {
		t.Fatalf("attribution produced curator evidence: %+v", digests)
	}
	summaries, err := store.SkillAttributionSummaries(ctx, identity.TenantID)
	if err != nil || len(summaries) != 1 {
		t.Fatalf("attribution should still be visible for stats: %d %v", len(summaries), err)
	}
}

// The activated Skill of a work unit is known, so attribution can be suppressed
// for it while another Skill read in the same work unit still counts.
func TestWorkUnitActivatedSkillKeysDrivesSuppression(t *testing.T) {
	store, ctx := attributionFixture(t)
	keys, err := store.WorkUnitActivatedSkillKeys(ctx, "default", "run-1", "unit-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("no activation was recorded: %v", keys)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO run_skill_activations(
		id, identity_tenant_id, control_tenant_id, person_id, run_id, sequence, work_unit_id,
		primary_task_id, skill_key, skill_name, version_hash, activation_source, state, selected_at)
		VALUES('a1','default','default','person-1','run-1',1,'unit-1','task-1','key-a','alpha','v1','model','completed',1000)`); err != nil {
		t.Fatal(err)
	}
	keys, err = store.WorkUnitActivatedSkillKeys(ctx, "default", "run-1", "unit-1")
	if err != nil {
		t.Fatal(err)
	}
	if !keys["key-a"] {
		t.Fatalf("activated Skill not reported: %v", keys)
	}
	if keys["key-b"] {
		t.Fatalf("unrelated Skill reported as activated: %v", keys)
	}
}
