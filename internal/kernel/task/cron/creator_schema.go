package cron

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Version zero is the released, unversioned cron store. Version one records
// each new job's creating person; historical rows retain an empty creator.
const cronSchemaVersion = 1

func (s *Scheduler) prepareCreatorSchema(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > cronSchemaVersion {
		return fmt.Errorf("cron database schema %d is newer than supported schema %d", version, cronSchemaVersion)
	}
	if version == cronSchemaVersion {
		return nil
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='cron_jobs'").Scan(&exists); err != nil || exists == 0 {
		return err
	}
	// A fresh database has nothing to back up. Existing file-backed stores are
	// snapshotted and checked before even the old additive migrations run.
	rows, err := s.db.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return err
	}
	var path string
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			rows.Close()
			return err
		}
		if name == "main" {
			path = file
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || path == "" {
		return err // In-memory stores have no durable source to back up.
	}
	if err := backupCreatorMigration(ctx, s.db, path, version); err != nil {
		return fmt.Errorf("cron migration backup failed; database was not migrated: %w", err)
	}
	return nil
}

func backupCreatorMigration(ctx context.Context, db *sql.DB, source string, version int) error {
	dir := filepath.Join(filepath.Dir(source), "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("cron-v%d-to-v%d-%s.db", version, cronSchemaVersion, time.Now().UTC().Format("20060102T150405.000000000Z")))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	quoted := strings.ReplaceAll(path, "'", "''")
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		return err
	}
	backup, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer backup.Close()
	var verdict string
	if err := backup.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&verdict); err != nil {
		return err
	}
	if verdict != "ok" {
		return fmt.Errorf("cron backup integrity check: %s", verdict)
	}
	return nil
}
