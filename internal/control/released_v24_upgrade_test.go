package control

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestReleasedV24CancellationUpgradePreservesHistoryAndAuthority(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ddl, err := os.ReadFile(filepath.Join("testdata", "control-v24-schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(string(ddl))
	exec(`INSERT INTO schema_migrations(version,name,applied_at) VALUES(1,'legacy-baseline',1)`)
	for _, m := range orderedMigrations {
		if m.Version <= 24 {
			exec(`INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,1)`, m.Version, m.Name)
		}
	}
	exec(`INSERT INTO threads(id,tenant_id,person_id,kind,visibility,title,summary,created_at,updated_at,last_activity_at) VALUES('old-thread','default','old-person','work','listed','Observe','',1,1,1)`)
	exec(`INSERT INTO runs(id,thread_id,tenant_id,person_id,channel,input_summary,status,started_at,recovery_contract_version) VALUES('old-run','old-thread','default','old-person','cli','Old scope','blocked',1,2)`)
	exec(`INSERT INTO run_plan_versions(run_id,tenant_id,version,explanation,content_hash,created_at) VALUES('old-run','default',1,'Old assessment','old-hash',1)`)
	exec(`INSERT INTO run_plan_steps(run_id,tenant_id,plan_version,step_id,sequence,step_text,status,cancellation_disposition,cancellation_reason,created_at) VALUES('old-run','default',1,'old-step',1,'Observe current state','cancelled','not_required','Legacy Main assessment',1)`)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	status := store.SchemaStatus()
	if status.Version != 25 || status.MigrationBackup == "" {
		t.Fatalf("unverified migration: %+v", status)
	}
	old, err := store.GetRun(ctx, "default", "old-run")
	if err != nil || old.RecoveryContractVersion != 2 || old.Status != "blocked" {
		t.Fatalf("migration rewrote Run authority: %+v err=%v", old, err)
	}
	plan, err := store.LatestRunPlan(ctx, "default", "old-run")
	if err != nil || plan.Version != 1 || plan.ContentHash != "old-hash" || plan.Steps[0].Status != "cancelled" || plan.Steps[0].ReplacementStepID != "" || plan.Steps[0].ScopeChangeQuote != "" {
		t.Fatalf("migration invented evidence: %+v err=%v", plan, err)
	}
	if err = store.ValidateRunCompletion(ctx, "default", "old-run"); err != nil {
		t.Fatal("a historical contract was silently tightened:", err)
	}
	task, err := store.GetTask(ctx, "default", "old-thread")
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.StartRunWithOptions(ctx, task, "cli", "continue", StartRunOptions{ResumesRunID: "old-run"})
	if err != nil {
		t.Fatal(err)
	}
	if child.RecoveryContractVersion != CurrentRunRecoveryContractVersion {
		t.Fatalf("new run retained historical contract: %+v", child)
	}
	childPlan, err := store.LatestRunPlan(ctx, "default", child.ID)
	if err != nil || childPlan.Steps[0].Status != "pending" || childPlan.Steps[0].CancellationDisposition != "unfinished" {
		t.Fatalf("resume erased ungrounded work: %+v err=%v", childPlan, err)
	}
	if err = store.ValidateRunCompletion(ctx, "default", child.ID); err == nil {
		t.Fatal("new run completed with ungrounded inherited cancellation")
	}
	backup, err := sql.Open("sqlite", status.MigrationBackup)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var integrity string
	if err = backup.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("backup integrity=%q err=%v", integrity, err)
	}
}
