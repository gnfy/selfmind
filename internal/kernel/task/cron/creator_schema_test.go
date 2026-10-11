package cron

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreatorSchemaRejectsNewerDatabaseBeforeWrites(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := NewScheduler(db, nil).InitSchema(context.Background()); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("unsupported newer cron schema accepted: %v", err)
	}
	var tables int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='cron_jobs'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("newer database was changed: tables=%d err=%v", tables, err)
	}
}

func TestCreatorMigrationBackupFailureLeavesLegacyDatabaseUntouched(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "cron.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE cron_jobs (id INTEGER PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	// A file at the backup directory is a deterministic write failure, even
	// when the test is run by a user who can bypass ordinary permission bits.
	if err := os.WriteFile(filepath.Join(dir, "backups"), []byte("do not replace"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewScheduler(db, nil).InitSchema(context.Background()); err == nil || !strings.Contains(err.Error(), "backup failed") {
		t.Fatalf("migration proceeded without a backup: %v", err)
	}
	var version, creatorColumns int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 0 {
		t.Fatalf("failed migration advanced its version: %d, %v", version, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('cron_jobs') WHERE name='creator_person_id'").Scan(&creatorColumns); err != nil || creatorColumns != 0 {
		t.Fatalf("failed backup changed legacy columns: %d, %v", creatorColumns, err)
	}
}

func TestStoredScheduleCreatorCannotBeRewrittenByRegistration(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()
	cutoff := time.Unix(1790000000, 0)
	job := &CronJob{Name: "nightly report", CronExpr: "0 9 * * *", Prompt: "summarize the export", TenantID: "default", Channel: "cli",
		CreatorPersonID: "person-original", AuthorizedAt: cutoff, Web: true, Once: true}
	id, err := s.AddJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, "default", id)
	if err != nil || got.CreatorPersonID != "person-original" || !got.AuthorizedAt.Equal(cutoff) || !got.Web || !got.Once {
		t.Fatalf("stored authority did not round-trip: %+v, %v", got, err)
	}
	got.CreatorPersonID = "person-other"
	if _, err := s.EnsureJob(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, err := s.GetJob(ctx, "default", id)
	if err != nil || again.CreatorPersonID != "person-original" || !again.AuthorizedAt.Equal(cutoff) {
		t.Fatalf("registration transferred stored authority: %+v, %v", again, err)
	}
	// A registration over an old row must not grant it a new creator either.
	legacy := &CronJob{Name: "older report", CronExpr: "0 9 * * *", Prompt: "older report", TenantID: "default", Channel: "cli"}
	legacyID, err := s.AddJob(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacy.CreatorPersonID = "person-original"
	if _, err := s.EnsureJob(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	old, err := s.GetJob(ctx, "default", legacyID)
	if err != nil || old.CreatorPersonID != "" {
		t.Fatalf("registration authorized a historical row: %+v, %v", old, err)
	}
}

func TestCreatorMigrationBacksUpReleasedRowsWithoutAuthorizingThem(t *testing.T) {
	for _, extra := range []string{"", ", platform TEXT NOT NULL DEFAULT '', deliver_to TEXT NOT NULL DEFAULT '', web INTEGER NOT NULL DEFAULT 0, authorized_at INTEGER NOT NULL DEFAULT 0, once INTEGER NOT NULL DEFAULT 0, task_id TEXT NOT NULL DEFAULT '', system_key TEXT NOT NULL DEFAULT ''"} {
		name := "original cron store"
		if extra != "" {
			name = "released delivery and authorization store"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := sql.Open("sqlite", filepath.Join(dir, "cron.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.Exec(`CREATE TABLE cron_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, cron_expr TEXT NOT NULL, prompt TEXT NOT NULL, tenant_id TEXT NOT NULL, channel TEXT NOT NULL DEFAULT 'cli', enabled INTEGER NOT NULL DEFAULT 1, last_run INTEGER, next_run INTEGER, created_at INTEGER NOT NULL DEFAULT 1790000000` + extra + `)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO cron_jobs (name, cron_expr, prompt, tenant_id) VALUES ('private notes', '0 9 * * *', 'my private notes', 'default')`); err != nil {
				t.Fatal(err)
			}
			s := NewScheduler(db, nil)
			if err := s.InitSchema(context.Background()); err != nil {
				t.Fatal(err)
			}
			jobs, err := s.ListJobs(context.Background(), "default")
			if err != nil || len(jobs) != 1 || jobs[0].Prompt != "my private notes" || jobs[0].CreatorPersonID != "" {
				t.Fatalf("historical row lost or granted authority: %+v, %v", jobs, err)
			}
			backups, err := filepath.Glob(filepath.Join(dir, "backups", "cron-v0-to-v1-*.db"))
			if err != nil || len(backups) != 1 {
				t.Fatalf("pre-migration backup missing: %v, %v", backups, err)
			}
			info, err := os.Stat(backups[0])
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("backup permissions: %v, %v", info, err)
			}
			backup, err := sql.Open("sqlite", backups[0])
			if err != nil {
				t.Fatal(err)
			}
			defer backup.Close()
			var prompt string
			if err := backup.QueryRow("SELECT prompt FROM cron_jobs").Scan(&prompt); err != nil || prompt != "my private notes" {
				t.Fatalf("backup is not the original database: %q, %v", prompt, err)
			}
			var creator string
			if err := backup.QueryRow("SELECT creator_person_id FROM cron_jobs").Scan(&creator); err == nil {
				t.Fatal("backup was taken after the creator migration")
			}
			if err := s.InitSchema(context.Background()); err != nil {
				t.Fatal(err)
			}
			again, _ := filepath.Glob(filepath.Join(dir, "backups", "cron-v0-to-v1-*.db"))
			if len(again) != 1 {
				t.Fatalf("already migrated store backed up again: %v", again)
			}
		})
	}
}
