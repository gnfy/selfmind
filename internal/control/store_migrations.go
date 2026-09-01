package control

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CurrentControlSchemaVersion is the durable control.db compatibility
// boundary. Adding or changing durable schema requires an ordered migration and
// a version bump; silently extending InitSchema is not a release-safe upgrade.
const CurrentControlSchemaVersion = 9

// schemaBaselineVersion is the version recorded for the historical additive
// schema created by InitSchema. Every durable change after it is an entry in
// orderedMigrations, so schema_migrations always describes what was applied.
const schemaBaselineVersion = 1

// schemaMigration is one ordered, idempotent step above the baseline. Steps run
// lowest version first and each records its own ledger row, so a non-additive
// future change (column type, backfill, constraint) has a real slot instead of
// being smuggled into InitSchema, where existing databases would silently skip
// it.
type schemaMigration struct {
	Version int
	Name    string
	Apply   func(context.Context, *sql.DB) error
}

// orderedMigrations must stay sorted by Version, and the highest Version must
// equal CurrentControlSchemaVersion (pinned by test).
var orderedMigrations = []schemaMigration{
	{
		Version: 2,
		Name:    "memory-governance-schedule",
		Apply: func(ctx context.Context, db *sql.DB) error {
			_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS memory_governance_schedule (
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	last_attempt_at INTEGER NOT NULL DEFAULT 0,
	last_success_at INTEGER NOT NULL DEFAULT 0,
	next_due_at INTEGER NOT NULL DEFAULT 0,
	consecutive_failures INTEGER NOT NULL DEFAULT 0,
	last_outcome TEXT NOT NULL DEFAULT '',
	last_deferred_reason TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, person_id)
);
CREATE INDEX IF NOT EXISTS idx_memory_governance_due
	ON memory_governance_schedule(next_due_at, tenant_id, person_id);`)
			return err
		},
	},
	{
		Version: 3,
		Name:    "run-execution-roots",
		Apply: func(ctx context.Context, db *sql.DB) error {
			if err := ensureMigrationColumn(ctx, db, "task_queue", "execution_roots_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
				return err
			}
			return ensureMigrationColumn(ctx, db, "task_runs", "execution_roots_json", "TEXT NOT NULL DEFAULT '[]'")
		},
	},
	{
		Version: 4,
		Name:    "skill-presentation-contract",
		Apply: func(ctx context.Context, db *sql.DB) error {
			columns := []struct {
				table, name, definition string
			}{
				{"skill_versions", "package_hash", "TEXT NOT NULL DEFAULT ''"},
				{"skill_versions", "resource_manifest_json", "TEXT NOT NULL DEFAULT '[]'"},
				{"run_skill_activations", "package_hash", "TEXT NOT NULL DEFAULT ''"},
				{"run_skill_activations", "delivery_contract_version", "INTEGER NOT NULL DEFAULT 0"},
				{"run_skill_activations", "delivery_mode", "TEXT NOT NULL DEFAULT ''"},
				{"run_skill_activations", "delivered_main", "TEXT NOT NULL DEFAULT ''"},
				{"run_skill_activations", "delivered_main_hash", "TEXT NOT NULL DEFAULT ''"},
				{"run_skill_activations", "delivered_main_bytes", "INTEGER NOT NULL DEFAULT 0"},
				{"run_skill_activations", "resource_manifest_json", "TEXT NOT NULL DEFAULT '[]'"},
			}
			for _, column := range columns {
				if err := ensureMigrationColumn(ctx, db, column.table, column.name, column.definition); err != nil {
					return err
				}
			}
			_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS skill_candidate_refs (
	candidate_ref TEXT PRIMARY KEY,
	identity_tenant_id TEXT NOT NULL,
	control_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	work_unit_id TEXT NOT NULL,
	skill_key TEXT NOT NULL,
	skill_name TEXT NOT NULL,
	version_hash TEXT NOT NULL,
	package_hash TEXT NOT NULL,
	description_hash TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT 'issued',
	drift_count INTEGER NOT NULL DEFAULT 0,
	issued_at INTEGER NOT NULL,
	last_used_at INTEGER NOT NULL DEFAULT 0,
	UNIQUE(identity_tenant_id, run_id, work_unit_id, skill_key, package_hash, description_hash)
);
CREATE INDEX IF NOT EXISTS idx_skill_candidate_refs_work_unit
	ON skill_candidate_refs(identity_tenant_id, run_id, work_unit_id, issued_at);
CREATE TABLE IF NOT EXISTS skill_package_resources (
	control_tenant_id TEXT NOT NULL,
	skill_key TEXT NOT NULL,
	package_hash TEXT NOT NULL,
	resource_path TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	content_body TEXT NOT NULL,
	content_bytes INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY(control_tenant_id, skill_key, package_hash, resource_path)
);`)
			return err
		},
	},
	{
		Version: 5,
		Name:    "skill-repair-health",
		Apply: func(ctx context.Context, db *sql.DB) error {
			columns := []struct {
				table, name, definition string
			}{
				{"skill_failure_guards", "environment_fingerprint", "TEXT NOT NULL DEFAULT ''"},
				{"skill_versions", "dependency_fingerprint", "TEXT NOT NULL DEFAULT ''"},
				{"skill_versions", "verification_environment_fingerprint", "TEXT NOT NULL DEFAULT ''"},
				{"skill_versions", "last_verified_at", "INTEGER NOT NULL DEFAULT 0"},
			}
			for _, column := range columns {
				if err := ensureMigrationColumn(ctx, db, column.table, column.name, column.definition); err != nil {
					return err
				}
			}
			_, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_skill_versions_review_due
				ON skill_versions(control_tenant_id, state, last_verified_at);
			CREATE TABLE IF NOT EXISTS skill_candidate_evidence_snapshots (
				control_tenant_id TEXT NOT NULL,
				skill_key TEXT NOT NULL,
				version_hash TEXT NOT NULL,
				evidence_set_hash TEXT NOT NULL,
				observation_ids_json TEXT NOT NULL DEFAULT '[]',
				evidence_json TEXT NOT NULL DEFAULT '{}',
				created_at INTEGER NOT NULL,
				PRIMARY KEY(control_tenant_id, skill_key, version_hash, evidence_set_hash)
			);
			CREATE INDEX IF NOT EXISTS idx_skill_candidate_evidence_set
				ON skill_candidate_evidence_snapshots(control_tenant_id, evidence_set_hash);`)
			return err
		},
	},
	{
		Version: 6,
		Name:    "skill-attribution",
		Apply: func(ctx context.Context, db *sql.DB) error {
			// Attribution records implicit Skill use. It is deliberately its own
			// table rather than a column on run_skill_activations: activation
			// evidence feeds curator cohorts and repair thresholds, attribution
			// never does, and separate storage makes that boundary structural
			// instead of a field convention. The primary key is the per-work-unit
			// de-duplication rule, so a repeated observation is a no-op insert.
			_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS skill_attributions (
	control_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL,
	work_unit_id TEXT NOT NULL,
	skill_key TEXT NOT NULL,
	skill_name TEXT NOT NULL,
	package_path TEXT NOT NULL,
	package_name TEXT NOT NULL DEFAULT '',
	scope TEXT NOT NULL DEFAULT '',
	provenance TEXT NOT NULL DEFAULT '',
	tool_name TEXT NOT NULL DEFAULT '',
	observed_at INTEGER NOT NULL,
	PRIMARY KEY (control_tenant_id, run_id, work_unit_id, package_path)
);
CREATE INDEX IF NOT EXISTS idx_skill_attributions_skill
	ON skill_attributions(control_tenant_id, skill_key, observed_at);`)
			return err
		},
	},
	{
		Version: 7,
		Name:    "run-parent-edge",
		Apply: func(ctx context.Context, db *sql.DB) error {
			// The forward continuation edge: child.parent_run_id -> parent.id is the only ownership
			// authority; the legacy reverse resumed_by_run_id becomes read-only
			// compatibility. Handoffs gain their formal run key, and queued rows
			// carry structured reply/approval/clarification return metadata
			// durably, including answers that arrive after a gateway restart.
			for _, column := range []struct{ table, name, definition string }{
				{"task_runs", "parent_run_id", "TEXT NOT NULL DEFAULT ''"},
				{"task_handoffs", "run_id", "TEXT NOT NULL DEFAULT ''"},
				{"task_queue", "reply_to_run_id", "TEXT NOT NULL DEFAULT ''"},
				{"task_queue", "approval_id", "TEXT NOT NULL DEFAULT ''"},
				{"task_queue", "clarify_id", "TEXT NOT NULL DEFAULT ''"},
			} {
				if err := ensureMigrationColumn(ctx, db, column.table, column.name, column.definition); err != nil {
					return err
				}
			}
			// Run ids are globally unique, so (tenant_id, parent_run_id) fully
			// enforces "at most one child per parent" — adding person/task keys
			// would only weaken the constraint. Parent/child agreement on
			// tenant, person, task, and resumable state is validated by the
			// child-creation transaction (no foreign keys exist).
			if _, err := db.ExecContext(ctx, `
CREATE UNIQUE INDEX IF NOT EXISTS idx_task_runs_parent_once
	ON task_runs(tenant_id, parent_run_id) WHERE parent_run_id <> '';
CREATE INDEX IF NOT EXISTS idx_task_handoffs_run
	ON task_handoffs(run_id) WHERE run_id <> '';`); err != nil {
				return err
			}
			// Backfill 1: the production finalizer always keyed handoffs
			// "handoff_run_<run_id>", so the formal column recovers losslessly
			// from the primary key. Rows under any other id stay empty (audit).
			if _, err := db.ExecContext(ctx,
				`UPDATE task_handoffs SET run_id = substr(id, 13)
				  WHERE run_id = '' AND id LIKE 'handoff_run_%'`); err != nil {
				return err
			}
			// Backfill 2: convert exact legacy reverse edges
			// (parent.resumed_by_run_id = child.id) into the forward edge, but
			// only same-tenant/person/task relationships with exactly one
			// claiming parent. Conflicting, missing, or cross-boundary history
			// is never guessed — it stays visible to the read-only audit.
			// (Expected to be near-empty: live data shows zero legacy edges.)
			_, err := db.ExecContext(ctx, `
UPDATE task_runs SET parent_run_id = (
	SELECT p.id FROM task_runs p
	 WHERE p.tenant_id = task_runs.tenant_id
	   AND p.task_id = task_runs.task_id
	   AND p.person_id = task_runs.person_id
	   AND p.resumed_by_run_id = task_runs.id
)
WHERE parent_run_id = ''
  AND (SELECT COUNT(*) FROM task_runs p
	    WHERE p.tenant_id = task_runs.tenant_id
	      AND p.task_id = task_runs.task_id
	      AND p.person_id = task_runs.person_id
	      AND p.resumed_by_run_id = task_runs.id) = 1`)
			return err
		},
	},
	{
		Version: 8,
		Name:    "turn-continuity-resolution",
		Apply: func(ctx context.Context, db *sql.DB) error {
			// A continuity choice exists before a child run, so it cannot reuse
			// run-bound clarify_requests. The request snapshot is short-lived and
			// person-partitioned; it lets a reply on another bound endpoint resume
			// the original message without parsing the rendered candidate list.
			// Resolution events are a separate control-plane audit surface because
			// OBSERVE and CLARIFY deliberately create no task or run.
			_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS pending_turn_choices (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT '',
	resolution_id TEXT NOT NULL DEFAULT '',
	request_json TEXT NOT NULL,
	options_json TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL DEFAULT 'pending',
	chosen_key TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	claimed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_pending_turn_choices_person
	ON pending_turn_choices(tenant_id, person_id, status, expires_at, created_at);
CREATE TABLE IF NOT EXISTS turn_resolution_events (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT '',
	input_hash TEXT NOT NULL DEFAULT '',
	mode TEXT NOT NULL DEFAULT '',
	decision TEXT NOT NULL DEFAULT '',
	certainty TEXT NOT NULL DEFAULT '',
	target_task_id TEXT NOT NULL DEFAULT '',
	target_run_id TEXT NOT NULL DEFAULT '',
	candidate_ids_json TEXT NOT NULL DEFAULT '[]',
	evidence_json TEXT NOT NULL DEFAULT '[]',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	latency_ms INTEGER NOT NULL DEFAULT 0,
	error_class TEXT NOT NULL DEFAULT '',
	correction_of TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_turn_resolution_events_person
	ON turn_resolution_events(tenant_id, person_id, created_at);
CREATE INDEX IF NOT EXISTS idx_turn_resolution_events_target
	ON turn_resolution_events(tenant_id, target_run_id, created_at)
	WHERE target_run_id <> '';`)
			return err
		},
	},
	{
		Version: 9,
		Name:    "run-recovery-contract",
		Apply: func(ctx context.Context, db *sql.DB) error {
			// Historical runs stay capability-inert (version 0). Only runs created
			// by a v9-aware binary opt into the durable plan/effect/checkpoint
			// contract, so an upgrade never changes the meaning of old rows.
			for _, column := range []struct{ table, name, definition string }{
				{"task_runs", "recovery_contract_version", "INTEGER NOT NULL DEFAULT 0"},
				{"tool_ledger", "effect_id", "TEXT NOT NULL DEFAULT ''"},
				{"tool_ledger", "plan_version", "INTEGER NOT NULL DEFAULT 0"},
				{"tool_ledger", "plan_step_id", "TEXT NOT NULL DEFAULT ''"},
				{"tool_ledger", "strategy", "TEXT NOT NULL DEFAULT ''"},
				{"tool_ledger", "effect_class", "TEXT NOT NULL DEFAULT ''"},
				{"tool_ledger", "environment_generation", "INTEGER NOT NULL DEFAULT 0"},
				{"tool_ledger", "result_ref", "TEXT NOT NULL DEFAULT ''"},
				{"tool_ledger", "verification_state", "TEXT NOT NULL DEFAULT ''"},
				{"loop_checkpoints", "contract_version", "INTEGER NOT NULL DEFAULT 0"},
				{"loop_checkpoints", "recovery_json", "TEXT NOT NULL DEFAULT '{}'"},
				{"external_watches", "observation_adapter", "TEXT NOT NULL DEFAULT ''"},
				{"external_watches", "preflight_receipt_json", "TEXT NOT NULL DEFAULT '{}'"},
				{"external_watches", "wait_group_id", "TEXT NOT NULL DEFAULT ''"},
			} {
				if err := ensureMigrationColumn(ctx, db, column.table, column.name, column.definition); err != nil {
					return err
				}
			}
			_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS run_plan_versions (
	run_id TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	version INTEGER NOT NULL,
	explanation TEXT NOT NULL DEFAULT '',
	content_hash TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (run_id, version)
);
CREATE INDEX IF NOT EXISTS idx_run_plan_versions_latest
	ON run_plan_versions(tenant_id, run_id, version DESC);
CREATE TABLE IF NOT EXISTS run_plan_steps (
	run_id TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	plan_version INTEGER NOT NULL,
	step_id TEXT NOT NULL,
	sequence INTEGER NOT NULL,
	step_text TEXT NOT NULL,
	status TEXT NOT NULL,
	success_criteria TEXT NOT NULL DEFAULT '',
	verification_required INTEGER NOT NULL DEFAULT 0,
	related_task_id TEXT NOT NULL DEFAULT '',
	work_unit_id TEXT NOT NULL DEFAULT '',
	work_unit_boundary INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (run_id, plan_version, step_id),
	UNIQUE (run_id, plan_version, sequence)
);
CREATE INDEX IF NOT EXISTS idx_run_plan_steps_latest
	ON run_plan_steps(tenant_id, run_id, plan_version DESC, sequence);
CREATE INDEX IF NOT EXISTS idx_tool_ledger_effect
	ON tool_ledger(tenant_id, effect_id) WHERE effect_id <> '';
CREATE INDEX IF NOT EXISTS idx_tool_ledger_plan_step
	ON tool_ledger(tenant_id, run_id, plan_step_id) WHERE plan_step_id <> '';
CREATE TABLE IF NOT EXISTS external_watch_groups (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	group_key TEXT NOT NULL,
	mode TEXT NOT NULL,
	expected_count INTEGER NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	winner_watch_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0,
	UNIQUE (tenant_id, run_id, group_key)
);
CREATE INDEX IF NOT EXISTS idx_external_watch_groups_pending
	ON external_watch_groups(tenant_id, person_id, status, created_at);`)
			return err
		},
	},
}

func ensureMigrationColumn(ctx context.Context, db *sql.DB, table, name, definition string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull, primaryKey int
		var defaultValue interface{}
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if columnName == name {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+name+` `+definition)
	return err
}

// StoreSchemaStatus is safe diagnostic metadata. It contains no user content.
type StoreSchemaStatus struct {
	Version         int
	CurrentVersion  int
	MigrationBackup string
}

// SchemaStatus reports the schema accepted by this process and the backup made
// by this OpenStore call, if it crossed a migration boundary.
func (s *Store) SchemaStatus() StoreSchemaStatus {
	if s == nil {
		return StoreSchemaStatus{CurrentVersion: CurrentControlSchemaVersion}
	}
	return StoreSchemaStatus{
		Version: s.schemaVersion, CurrentVersion: CurrentControlSchemaVersion,
		MigrationBackup: s.migrationBackup,
	}
}

func nonEmptyRegularFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", path)
	}
	return info.Size() > 0, nil
}

type controlIntegrityCheck func(context.Context, *sql.DB) error

func (s *Store) prepareAndMigrateSchema(ctx context.Context, dataDir, dbPath string, existing bool, checkIntegrity controlIntegrityCheck) error {
	version, versioned, err := s.readSchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("inspect control schema: %w", err)
	}
	if version > CurrentControlSchemaVersion {
		return fmt.Errorf("control.db schema %d is newer than this SelfMind binary supports (max %d); refusing to write user data", version, CurrentControlSchemaVersion)
	}
	if versioned && version == CurrentControlSchemaVersion {
		// Compatibility is an O(1) version decision. A full PRAGMA quick_check
		// walks the database and made every cold daemon start scale with years of
		// task history. Keep that expensive validation at actual migration and
		// restore boundaries; matching versions need no schema work.
		s.schemaVersion = version
		return nil
	}

	before, err := captureMigrationInvariants(ctx, s.db)
	if err != nil {
		return fmt.Errorf("capture pre-migration invariants: %w", err)
	}
	if existing {
		if err := checkIntegrity(ctx, s.db); err != nil {
			return fmt.Errorf("control.db failed pre-migration integrity check: %w", err)
		}
		backup, backupErr := backupControlDatabase(ctx, s.db, dataDir, version, CurrentControlSchemaVersion)
		if backupErr != nil {
			return fmt.Errorf("create pre-migration control.db backup: %w", backupErr)
		}
		s.migrationBackup = backup
	}

	// Version 1 adopts the historical additive schema as a compatibility
	// baseline. All subsequent durable changes must be explicit ordered
	// migrations rather than more implicit OpenStore side effects.
	if err := s.InitSchema(ctx); err != nil {
		return migrationFailure(err, s.migrationBackup)
	}
	if err := ensureSchemaMigrationsTable(ctx, s.db); err != nil {
		return migrationFailure(err, s.migrationBackup)
	}
	if err := verifyMigrationInvariants(ctx, s.db, before); err != nil {
		return migrationFailure(err, s.migrationBackup)
	}
	// Record the baseline before any ordered step so the ledger describes what
	// was actually applied. A database with no ledger has, by definition, just
	// received the baseline; one already at or past it keeps its own row.
	if version < schemaBaselineVersion {
		if err := recordSchemaMigration(ctx, s.db, schemaBaselineVersion, "legacy-baseline"); err != nil {
			return migrationFailure(err, s.migrationBackup)
		}
		version = schemaBaselineVersion
	}
	for _, migration := range orderedMigrations {
		if migration.Version <= version {
			continue
		}
		if err := migration.Apply(ctx, s.db); err != nil {
			return migrationFailure(fmt.Errorf("apply migration %d (%s): %w", migration.Version, migration.Name, err), s.migrationBackup)
		}
		if err := recordSchemaMigration(ctx, s.db, migration.Version, migration.Name); err != nil {
			return migrationFailure(err, s.migrationBackup)
		}
		// Re-verify after every step: a step that drops user rows must fail at
		// that step, with the backup still describing the state before it.
		if err := verifyMigrationInvariants(ctx, s.db, before); err != nil {
			return migrationFailure(err, s.migrationBackup)
		}
		version = migration.Version
	}
	if err := checkIntegrity(ctx, s.db); err != nil {
		return migrationFailure(err, s.migrationBackup)
	}
	if version != CurrentControlSchemaVersion {
		return migrationFailure(fmt.Errorf("schema stopped at version %d but this binary requires %d; orderedMigrations is missing a step", version, CurrentControlSchemaVersion), s.migrationBackup)
	}
	s.schemaVersion = CurrentControlSchemaVersion
	return nil
}

func recordSchemaMigration(ctx context.Context, db *sql.DB, version int, name string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, applied_at)
		VALUES (?, ?, ?) ON CONFLICT(version) DO NOTHING`, version, name, time.Now().Unix())
	return err
}

func migrationFailure(err error, backup string) error {
	if strings.TrimSpace(backup) == "" {
		return fmt.Errorf("migrate control.db: %w", err)
	}
	return fmt.Errorf("migrate control.db: %w (pre-migration backup preserved at %s)", err, backup)
}

func ensureSchemaMigrationsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	)`)
	return err
}

func (s *Store) readSchemaVersion(ctx context.Context) (version int, versioned bool, err error) {
	var exists int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&exists); err != nil {
		return 0, false, err
	}
	if exists == 0 {
		return 0, false, nil
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, true, err
	}
	return version, true, nil
}

func backupControlDatabase(ctx context.Context, db *sql.DB, dataDir string, fromVersion, toVersion int) (string, error) {
	backupDir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("control-v%d-to-v%d-%s.db", fromVersion, toVersion, time.Now().UTC().Format("20060102T150405.000000000Z"))
	path := filepath.Join(backupDir, name)
	quoted := strings.ReplaceAll(path, "'", "''")
	if _, err := db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'"); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return "", err
	}
	backup, err := sql.Open("sqlite", path)
	if err != nil {
		return "", err
	}
	defer backup.Close()
	if err := quickCheckDB(ctx, backup); err != nil {
		return "", fmt.Errorf("verify backup: %w", err)
	}
	if err := pruneControlBackups(backupDir, path, 3); err != nil {
		return "", fmt.Errorf("prune old backups: %w", err)
	}
	return path, nil
}

func pruneControlBackups(dir, keepPath string, retain int) error {
	if retain < 1 {
		retain = 1
	}
	paths, err := filepath.Glob(filepath.Join(dir, "control-v*-to-v*.db"))
	if err != nil {
		return err
	}
	sort.Strings(paths)
	removeCount := len(paths) - retain
	for _, candidate := range paths {
		if removeCount <= 0 {
			break
		}
		if candidate == keepPath {
			continue
		}
		if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
			return err
		}
		removeCount--
	}
	return nil
}

// RestoreControlDatabase replaces control.db with one migration backup. The
// caller must stop the daemon and explicitly confirm the destructive restore.
// The failed database and WAL sidecars are preserved beside it for diagnosis.
func RestoreControlDatabase(ctx context.Context, dataDir, backupPath string) (failedPath string, err error) {
	dataDir = strings.TrimSpace(dataDir)
	backupPath = strings.TrimSpace(backupPath)
	if dataDir == "" || backupPath == "" {
		return "", fmt.Errorf("data dir and backup path are required")
	}
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	absBackup, err := filepath.Abs(backupPath)
	if err != nil {
		return "", err
	}
	backupRoot := filepath.Join(absData, "backups")
	rel, err := filepath.Rel(backupRoot, absBackup)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("backup must be a file under %s", backupRoot)
	}
	if !strings.HasPrefix(filepath.Base(absBackup), "control-v") || !strings.HasSuffix(absBackup, ".db") {
		return "", fmt.Errorf("backup name is not a SelfMind control migration snapshot")
	}
	if ok, statErr := nonEmptyRegularFile(absBackup); statErr != nil || !ok {
		if statErr != nil {
			return "", statErr
		}
		return "", fmt.Errorf("backup is empty")
	}
	if err := checkSQLiteFile(ctx, absBackup); err != nil {
		return "", fmt.Errorf("backup integrity check: %w", err)
	}

	tmpPath := filepath.Join(absData, fmt.Sprintf("control.restore-%d.tmp", time.Now().UnixNano()))
	if err := copyFileSynced(absBackup, tmpPath, 0600); err != nil {
		return "", err
	}
	defer os.Remove(tmpPath)
	if err := checkSQLiteFile(ctx, tmpPath); err != nil {
		return "", fmt.Errorf("restored copy integrity check: %w", err)
	}

	dbPath := filepath.Join(absData, "control.db")
	failedPath = filepath.Join(absData, fmt.Sprintf("control.failed-%s.db", time.Now().UTC().Format("20060102T150405.000000000Z")))
	if _, err := os.Stat(dbPath); err == nil {
		if err := os.Rename(dbPath, failedPath); err != nil {
			return "", fmt.Errorf("preserve failed control.db: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	} else {
		failedPath = ""
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, statErr := os.Stat(dbPath + suffix); statErr == nil {
			target := failedPath + suffix
			if failedPath == "" {
				target = filepath.Join(absData, "control.failed-orphan"+suffix)
			}
			if renameErr := os.Rename(dbPath+suffix, target); renameErr != nil {
				if failedPath != "" {
					_ = os.Rename(failedPath, dbPath)
				}
				return "", fmt.Errorf("preserve SQLite sidecar %s: %w", suffix, renameErr)
			}
		}
	}
	if err := os.Rename(tmpPath, dbPath); err != nil {
		if failedPath != "" {
			_ = os.Rename(failedPath, dbPath)
		}
		return "", fmt.Errorf("activate restored control.db: %w", err)
	}
	if err := os.Chmod(dbPath, 0600); err != nil {
		return failedPath, err
	}
	return failedPath, nil
}

func checkSQLiteFile(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	return quickCheckDB(ctx, db)
}

func copyFileSynced(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func quickCheckDB(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		if !strings.EqualFold(strings.TrimSpace(result), "ok") {
			return fmt.Errorf("quick_check returned %q", result)
		}
	}
	return rows.Err()
}

type migrationInvariants map[string]map[string]int64

func captureMigrationInvariants(ctx context.Context, db *sql.DB) (migrationInvariants, error) {
	out := make(migrationInvariants)
	for _, spec := range []struct {
		table, state string
	}{
		{"approval_requests", "status"},
		{"task_runs", "status"},
		{"task_queue", "status"},
		{"tasks", "status"},
	} {
		buckets, err := stateCountsIfTableExists(ctx, db, spec.table, spec.state)
		if err != nil {
			return nil, err
		}
		if buckets != nil {
			out[spec.table] = buckets
		}
	}
	return out, nil
}

func stateCountsIfTableExists(ctx context.Context, db *sql.DB, table, stateColumn string) (map[string]int64, error) {
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT COALESCE(%s, ''), COUNT(*) FROM %s GROUP BY COALESCE(%s, '')`, stateColumn, table, stateColumn))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return nil, err
		}
		out[state] = count
	}
	return out, rows.Err()
}

func verifyMigrationInvariants(ctx context.Context, db *sql.DB, before migrationInvariants) error {
	after, err := captureMigrationInvariants(ctx, db)
	if err != nil {
		return err
	}
	for table, want := range before {
		if got := after[table]; !sameStateCounts(want, got) {
			return fmt.Errorf("migration changed %s state counts: before=%s after=%s", table, formatStateCounts(want), formatStateCounts(got))
		}
	}
	// A schema upgrade may expose historical decisions to new readers, but must
	// never arm them for cross-run execution. Only a post-upgrade human decision
	// may write decision_recorded_at/authorization_state=available.
	var armedHistorical int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM approval_requests
		WHERE COALESCE(authorization_state, '') = 'available' AND COALESCE(decision_recorded_at, 0) = 0`).Scan(&armedHistorical); err != nil {
		return err
	}
	if armedHistorical != 0 {
		return fmt.Errorf("migration armed %d historical approval authorization(s)", armedHistorical)
	}
	return nil
}

func sameStateCounts(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for key, count := range a {
		if b[key] != count {
			return false
		}
	}
	return true
}

func formatStateCounts(counts map[string]int64) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
}
