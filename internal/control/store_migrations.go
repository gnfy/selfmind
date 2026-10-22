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
const CurrentControlSchemaVersion = 26

// schemaBaselineVersion is the version recorded for the historical additive
// schema created by InitSchema. Every durable change after it is an entry in
// orderedMigrations, so schema_migrations always describes what was applied.
const schemaBaselineVersion = 1

// threadedWorkHistorySchemaVersion is the version whose shape
// hasThreadedWorkHistorySchema recognizes. It is pinned to that detector and
// must NEVER be replaced by CurrentControlSchemaVersion: shape adoption may
// only claim the steps it can actually see, and every step above it still has
// to run. Tying it to "current" silently stamps future migrations as applied on
// any database presenting this shape — which is every released v11 install.
const threadedWorkHistorySchemaVersion = 11

// schemaMigration is one ordered, idempotent step above the baseline. Steps run
// lowest version first and each records its own ledger row, so a non-additive
// future change (column type, backfill, constraint) has a real slot instead of
// being smuggled into InitSchema, where existing databases would silently skip
// it.
type schemaMigration struct {
	Version int
	Name    string
	Apply   func(context.Context, migrationDB) error
}

// migrationDB is what a step and its checks run against: during an upgrade,
// the step's own transaction (see applyMigrationStep).
type migrationDB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// orderedMigrations must stay sorted by Version, and the highest Version must
// equal CurrentControlSchemaVersion (pinned by test).
var orderedMigrations = []schemaMigration{
	{
		Version: 2,
		Name:    "memory-governance-schedule",
		Apply: func(ctx context.Context, db migrationDB) error {
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
		Apply: func(ctx context.Context, db migrationDB) error {
			if err := ensureMigrationColumn(ctx, db, "task_queue", "execution_roots_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
				return err
			}
			return ensureMigrationColumn(ctx, db, "task_runs", "execution_roots_json", "TEXT NOT NULL DEFAULT '[]'")
		},
	},
	{
		Version: 4,
		Name:    "skill-presentation-contract",
		Apply: func(ctx context.Context, db migrationDB) error {
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
		Apply: func(ctx context.Context, db migrationDB) error {
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
		Apply: func(ctx context.Context, db migrationDB) error {
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
		Apply: func(ctx context.Context, db migrationDB) error {
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
		Apply: func(ctx context.Context, db migrationDB) error {
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
		Apply: func(ctx context.Context, db migrationDB) error {
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
	{
		Version: 10,
		Name:    "run-delivery-override",
		Apply: func(ctx context.Context, db migrationDB) error {
			// Overrides are opt-in rows created only from an authenticated,
			// server-issued steering input. Historical runs gain no capability.
			_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS run_delivery_overrides (
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	platform TEXT NOT NULL,
	platform_user_id TEXT NOT NULL,
	channel TEXT NOT NULL,
	source_steering_id TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, run_id),
	UNIQUE (tenant_id, source_steering_id)
);
CREATE INDEX IF NOT EXISTS idx_run_delivery_overrides_person
	ON run_delivery_overrides(tenant_id, person_id, updated_at);`)
			return err
		},
	},
	{
		Version: 11,
		Name:    "threaded-work-history",
		Apply: func(ctx context.Context, db migrationDB) error {
			return migrateThreadedWorkHistory(ctx, db)
		},
	},
	{
		Version: 12,
		Name:    "resume-edge-naming",
		Apply: func(ctx context.Context, db migrationDB) error {
			return migrateResumeEdgeNaming(ctx, db)
		},
	},
	{
		Version: 13,
		Name:    "attention-dismissal-only",
		Apply: func(ctx context.Context, db migrationDB) error {
			return migrateAttentionDismissalOnly(ctx, db)
		},
	},
	{
		Version: 14,
		Name:    "durable-attachments",
		Apply: func(ctx context.Context, db migrationDB) error {
			return migrateDurableAttachments(ctx, db)
		},
	},
	{
		Version: 15,
		Name:    "drop-task-references",
		Apply: func(ctx context.Context, db migrationDB) error {
			// The task-reference feature existed to ADDRESS a Task by a
			// human-facing name. Task is no longer a domain object, so there is
			// nothing left for a reference to point at, and the code that read
			// these tables went with it: no production path has written either
			// one since. What remained was schema plus two `NOT EXISTS` guards
			// that could only ever be true, which read as conditions on two
			// DELETE statements that in fact had none.
			//
			// Dropping them is safe for any install: with no writer, every
			// existing table is empty by construction.
			_, err := db.ExecContext(ctx, `
DROP TABLE IF EXISTS task_reference_evidence;
DROP TABLE IF EXISTS task_references;`)
			return err
		},
	},
	{
		Version: 16,
		Name:    "drop-thread-pin",
		Apply: func(ctx context.Context, db migrationDB) error {
			// A pin was a display flag that decided what counts as work: it
			// kept a Thread listed and exempt from automatic archival. That is
			// the same mistake v13 corrected when it stopped ranking Attention
			// by Thread columns — the Run owns "what needs me now", and a
			// presentation bit must not override evidence.
			//
			// Its writer had no production caller, so no Thread has been pinned
			// since; what remained were three guards that could only ever pass
			// and a `/diag` counter that could only ever read zero. Dropping
			// the column removes the concept rather than leaving it inert for
			// the next reader to reintroduce.
			return dropMigrationColumn(ctx, db, "threads", "pinned")
		},
	},
	{
		Version: 17,
		Name:    "run-plan-step-source",
		Apply: func(ctx context.Context, db migrationDB) error {
			// Historical steps have no asserted origin. Only a new exact-parent
			// import may set this reference; an upgrade must not infer one from
			// position or similar prose.
			if err := ensureMigrationColumn(ctx, db, "run_plan_steps", "source_step_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
			if err := ensureMigrationColumn(ctx, db, "run_plan_steps", "source_plan_version", "INTEGER NOT NULL DEFAULT 0"); err != nil {
				return err
			}
			if err := ensureMigrationColumn(ctx, db, "run_plan_steps", "prior_verification_reused", "INTEGER NOT NULL DEFAULT 0"); err != nil {
				return err
			}
			return ensureMigrationColumn(ctx, db, "run_plan_steps", "reuse_reason", "TEXT NOT NULL DEFAULT ''")
		},
	},
	{
		Version: 18,
		Name:    "steering-execution-roots",
		Apply: func(ctx context.Context, db migrationDB) error {
			// Input steered into another run keeps the roots its own request
			// froze, for when it is queued as separate work. Historical rows stay
			// NULL — nothing was recorded — and keep the run's roots, as before.
			return ensureMigrationColumn(ctx, db, "steering_mailbox", "execution_roots_json", "TEXT")
		},
	},
	{
		Version: 19,
		Name:    "native-im-reply-edges",
		Apply: func(ctx context.Context, db migrationDB) error {
			if err := ensureMigrationColumn(ctx, db, "outbound_messages", "clarify_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
			_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS native_im_reply_edges (
				platform TEXT NOT NULL,
				channel TEXT NOT NULL,
				message_id TEXT NOT NULL,
				outbound_id TEXT NOT NULL,
				tenant_id TEXT NOT NULL,
				person_id TEXT NOT NULL,
				run_id TEXT NOT NULL DEFAULT '',
				approval_id TEXT NOT NULL DEFAULT '',
				clarify_id TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				PRIMARY KEY (platform, channel, message_id)
			);
			CREATE INDEX IF NOT EXISTS idx_native_im_reply_edges_outbound
				ON native_im_reply_edges(outbound_id);`)
			return err
		},
	},
	{
		Version: 20,
		Name:    "run-execution-class",
		Apply: func(ctx context.Context, db migrationDB) error {
			// Historical rows are work. A short Main coordination Run has its own
			// admission slot and cannot consume a work Run's capacity.
			return ensureMigrationColumn(ctx, db, "runs", "execution_class", "TEXT NOT NULL DEFAULT 'work'")
		},
	},
	{
		Version: 21,
		Name:    "turn-choice-observation-receipt",
		Apply: func(ctx context.Context, db migrationDB) error {
			if err := ensureMigrationColumn(ctx, db, "pending_turn_choices", "resolution_kind", "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
			return ensureMigrationColumn(ctx, db, "pending_turn_choices", "response_text", "TEXT NOT NULL DEFAULT ''")
		},
	},
	{
		Version: 22,
		Name:    "inbound-processing-receipt",
		Apply: func(ctx context.Context, db migrationDB) error {
			// Historical first-seen rows have no replayable payload. They remain
			// terminal; only newly accepted messages enter the processing states.
			for _, column := range []struct{ name, definition string }{
				{"state", "TEXT NOT NULL DEFAULT 'accepted'"},
				{"payload", "BLOB NOT NULL DEFAULT ''"},
				{"updated_at", "INTEGER NOT NULL DEFAULT 0"},
				{"last_error", "TEXT NOT NULL DEFAULT ''"},
				{"tenant_id", "TEXT NOT NULL DEFAULT ''"},
				{"person_id", "TEXT NOT NULL DEFAULT ''"},
				{"preview", "TEXT NOT NULL DEFAULT ''"},
			} {
				if err := ensureMigrationColumn(ctx, db, "inbound_dedup", column.name, column.definition); err != nil {
					return err
				}
			}
			_, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_inbound_processing_owner
				ON inbound_dedup(tenant_id, person_id, state, updated_at) WHERE state <> 'accepted'`)
			return err
		},
	},
	{
		Version: 23,
		Name:    "external-effect-claims",
		Apply: func(ctx context.Context, db migrationDB) error {
			// Old Runs never asserted an external target, so the new table is
			// deliberately empty after migration. Guessing claims from prose or
			// tool names would confer false authority to historical work.
			_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS external_effect_claims (
				id TEXT PRIMARY KEY,
				tenant_id TEXT NOT NULL,
				person_id TEXT NOT NULL,
				run_id TEXT NOT NULL,
				effect_id TEXT NOT NULL,
				target_key TEXT NOT NULL,
				state TEXT NOT NULL CHECK(state IN ('reserved', 'uncertain', 'observed')),
				observation_ref TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				UNIQUE(tenant_id, run_id, effect_id, target_key)
			);
			CREATE INDEX IF NOT EXISTS idx_external_effect_claims_active
				ON external_effect_claims(tenant_id, person_id, state, target_key)
				WHERE state <> 'observed';
			CREATE TABLE IF NOT EXISTS external_resource_waits (
				tenant_id TEXT NOT NULL,
				person_id TEXT NOT NULL,
				run_id TEXT NOT NULL,
				effect_id TEXT NOT NULL,
				targets_json TEXT NOT NULL,
				status TEXT NOT NULL CHECK(status IN ('pending', 'queued')),
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				PRIMARY KEY (tenant_id, run_id, effect_id)
			);
			CREATE INDEX IF NOT EXISTS idx_external_resource_waits_pending
				ON external_resource_waits(status, created_at) WHERE status = 'pending';`)
			return err
		},
	},
	{Version: 24, Name: "assessed-plan-cancellations", Apply: func(ctx context.Context, db migrationDB) error {
		// Empty values grant no new authority to historical plans or Runs.
		for _, name := range []string{"cancellation_disposition", "cancellation_reason", "user_takeover_quote"} {
			if err := ensureMigrationColumn(ctx, db, "run_plan_steps", name, "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
		return nil
	}},
	{Version: 25, Name: "plan-cancellation-scope-evidence", Apply: func(ctx context.Context, db migrationDB) error {
		// Historical Runs keep their contract and receive no new authority.
		for _, name := range []string{"replacement_step_id", "scope_change_quote"} {
			if err := ensureMigrationColumn(ctx, db, "run_plan_steps", name, "TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
		return nil
	}},
	{Version: 26, Name: "admission-effect-order-human-wait", Apply: func(ctx context.Context, db migrationDB) error {
		if err := migrateConcurrencyHardeningFacts(ctx, db); err != nil {
			return err
		}
		return migrateMCPReadOnlyMarks(ctx, db)
	}},
}

// migrateConcurrencyHardeningFacts adds the durable facts the concurrency
// hardening batch records from now on. Every value is inert for history: an
// empty admission snapshot keeps a Run or queued row on today's
// reconstruction, sequence 0 marks a claim or watcher recorded before this
// version, no settlement exists until a person settles an effect, an empty
// checkpoint wait binding keeps today's soft resume, parked_at 0 records no
// clarification park, an empty cancellation quote source records that no
// input identity was kept for a step's quote, and claim generation 0 marks an
// inbound receipt claimed before generations existed. Nothing here infers a
// snapshot, an order, a settlement, a binding, a quote's source, or a
// generation from historical rows; the framework's invariants reject a step
// that would.
func migrateConcurrencyHardeningFacts(ctx context.Context, db migrationDB) error {
	for _, column := range []struct{ table, name, definition string }{
		{"runs", "admission_json", "TEXT NOT NULL DEFAULT ''"},
		{"task_queue", "admission_json", "TEXT NOT NULL DEFAULT ''"},
		{"external_effect_claims", "sequence", "INTEGER NOT NULL DEFAULT 0"},
		{"external_watches", "sequence", "INTEGER NOT NULL DEFAULT 0"},
		{"loop_checkpoints", "wait_kind", "TEXT NOT NULL DEFAULT ''"},
		{"loop_checkpoints", "wait_ref", "TEXT NOT NULL DEFAULT ''"},
		{"loop_checkpoints", "wait_tool_call_id", "TEXT NOT NULL DEFAULT ''"},
		{"clarify_requests", "parked_at", "INTEGER NOT NULL DEFAULT 0"},
		{"run_plan_steps", "cancellation_quote_source", "TEXT NOT NULL DEFAULT ''"},
		{"inbound_dedup", "claim_generation", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := ensureMigrationColumn(ctx, db, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	// One settlement per effect group: a retried or concurrent settle meets
	// the unique key and returns the recorded audit instead of a second one.
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS external_effect_settlements (
		id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		person_id TEXT NOT NULL,
		claim_id TEXT NOT NULL,
		run_id TEXT NOT NULL,
		effect_id TEXT NOT NULL,
		actor_account_id TEXT NOT NULL DEFAULT '',
		actor_channel TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		UNIQUE (tenant_id, run_id, effect_id)
	);
	CREATE INDEX IF NOT EXISTS idx_external_effect_settlements_person
		ON external_effect_settlements(tenant_id, person_id, created_at);`)
	return err
}

// migrateDurableAttachments gives parked and steered work somewhere to keep its
// attachments. Both paths persisted the text and dropped the files, so a person
// who attached an image to work that happened to be queued — or to guidance
// added mid-run — was told it was accepted and the model never saw it. Existing
// rows are capability-inert: an absent column reads as no attachments, which is
// exactly what those rows had.
func migrateDurableAttachments(ctx context.Context, db migrationDB) error {
	for _, table := range []string{"task_queue", "steering_mailbox"} {
		if err := ensureMigrationColumn(ctx, db, table, "attachments_json", "TEXT"); err != nil {
			return fmt.Errorf("add %s.attachments_json: %w", table, err)
		}
	}
	return nil
}

// migrateResumeEdgeNaming renames the forward continuation edge so the column
// says what it means: this run resumes that run. The edge itself is unchanged —
// no row gains, loses, or redirects an edge — which is why the pre/post
// invariant bucket must agree exactly across this step.
//
// The name it replaces, parent_run_id, invited two readings the edge never
// carried: a topic hierarchy, and general causal descent. Both let "related"
// share a code path with "resumes execution state". The legacy reverse pointer
// resumed_by_run_id keeps its name; it stays read-only compatibility.
// migrateAttentionDismissalOnly moves Thread-row visibility out of Attention.
// Attention used to exclude runs whose Thread was hidden or archived and to
// rank pinned Threads first, which made a display row an input to "what needs
// me now". Both are now expressed on the Run: dismissal is the only hide.
//
// Archiving a Thread was, in effect, a bulk dismissal of its Attention, so the
// upgrade performs exactly that bulk dismissal. Without it, removing the filter
// would resurface work the person had already put away. Runs already dismissed
// keep their original timestamp and actor.
func migrateAttentionDismissalOnly(ctx context.Context, db migrationDB) error {
	for _, table := range []string{"threads", "runs"} {
		exists, existsErr := firstExistingMigrationTable(ctx, db, table)
		if existsErr != nil {
			return existsErr
		}
		if exists == "" {
			return nil
		}
	}
	hasVisibility, err := migrationColumnExists(ctx, db, "threads", "visibility")
	if err != nil || !hasVisibility {
		return err
	}
	hasDismissal, err := migrationColumnExists(ctx, db, "runs", "attention_dismissed_at")
	if err != nil || !hasDismissal {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE runs
		SET attention_dismissed_at = ?, attention_dismissed_by = 'retention'
		WHERE COALESCE(attention_dismissed_at, 0) = 0
		  AND thread_id IN (
		    SELECT id FROM threads
		     WHERE threads.tenant_id = runs.tenant_id
		       AND COALESCE(visibility, 'visible') IN ('hidden', 'archived')
		  )`, time.Now().Unix())
	return err
}

func migrateResumeEdgeNaming(ctx context.Context, db migrationDB) error {
	table := ""
	for _, candidate := range []string{"runs", "task_runs"} {
		exists, existsErr := migrationTableExists(ctx, db, candidate)
		if existsErr != nil {
			return existsErr
		}
		if exists {
			table = candidate
			break
		}
	}
	if table == "" {
		return nil
	}
	if err := renameMigrationColumn(ctx, db, table, "parent_run_id", "resumes_run_id"); err != nil {
		return err
	}
	// SQLite rewrites the indexed column reference on RENAME COLUMN but keeps
	// the index's own name, so the old name would outlive the column it
	// describes. Recreate it under the new name; "at most one resume per
	// target" is unchanged, and run ids are globally unique so (tenant_id,
	// resumes_run_id) still fully enforces it.
	if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_task_runs_parent_once`); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_resumes_once
	ON `+table+`(tenant_id, resumes_run_id) WHERE resumes_run_id <> ''`)
	return err
}

func migrateThreadedWorkHistory(ctx context.Context, db migrationDB) (err error) {
	// Build the new aggregate without any execution-lifecycle columns. Public
	// upgrades retain every historical label: visible labels become listed
	// history, hidden labels stay out of the ordinary list as unlisted, archived
	// labels remain archived, and the legacy kind survives (the retired inbox
	// kind is an interaction). Only roots created by v11-aware code start
	// unlisted by default.
	threadsExist, err := migrationTableExists(ctx, db, "threads")
	if err != nil {
		return err
	}
	if !threadsExist {
		if _, err = db.ExecContext(ctx, `CREATE TABLE threads_v11 (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			person_id TEXT NOT NULL,
			workspace_id TEXT,
			kind TEXT NOT NULL,
			visibility TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			pinned INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			last_activity_at INTEGER NOT NULL
		)`); err != nil {
			return err
		}
		tasksExist, tableErr := migrationTableExists(ctx, db, "tasks")
		if tableErr != nil {
			return tableErr
		}
		if tasksExist {
			if _, err = db.ExecContext(ctx, `INSERT INTO threads_v11 (
				id, tenant_id, person_id, workspace_id, kind, visibility, title,
				summary, pinned, created_at, updated_at, last_activity_at)
			SELECT id, tenant_id, person_id, workspace_id,
				CASE COALESCE(kind, 'work')
					WHEN 'inbox' THEN 'interaction'
					WHEN 'interaction' THEN 'interaction'
					WHEN 'recurring' THEN 'recurring'
					ELSE 'work' END,
				CASE COALESCE(visibility, 'visible')
					WHEN 'archived' THEN 'archived'
					WHEN 'hidden' THEN 'unlisted'
					ELSE 'listed' END,
				title, COALESCE(current_summary, ''), COALESCE(pinned, 0),
				created_at, updated_at, COALESCE(last_activity_at, updated_at)
			FROM tasks`); err != nil {
				return err
			}
		}
		if _, err = db.ExecContext(ctx, `ALTER TABLE threads_v11 RENAME TO threads`); err != nil {
			return err
		}
	}

	// Historical rows acquire no dismissal authority. The fields suppress only
	// derived Attention and never rewrite Run execution status.
	if exists, tableErr := migrationTableExists(ctx, db, "task_runs"); tableErr != nil {
		return tableErr
	} else if exists {
		for _, column := range []struct{ name, definition string }{
			{"attention_dismissed_at", "INTEGER NOT NULL DEFAULT 0"},
			{"attention_dismissed_by", "TEXT NOT NULL DEFAULT ''"},
		} {
			if err = ensureMigrationColumn(ctx, db, "task_runs", column.name, column.definition); err != nil {
				return err
			}
		}
		if err = renameMigrationColumn(ctx, db, "task_runs", "task_id", "thread_id"); err != nil {
			return err
		}
		if _, err = db.ExecContext(ctx, `ALTER TABLE task_runs RENAME TO runs`); err != nil {
			return err
		}
	}

	// All subordinate records keep their existing table and record identity;
	// only the aggregate reference changes its domain name.
	for _, table := range []string{
		"approval_requests", "approval_triage_events", "channel_messages",
		"clarify_requests", "effect_receipts", "external_watch_groups",
		"external_watches", "loop_checkpoints", "maintenance_provider_calls",
		"notifications", "outbound_messages", "steering_mailbox", "task_artifacts",
		"task_blockers", "task_events", "task_handoffs", "task_queue",
		"task_references", "task_skill_bindings", "workflow_profiles",
	} {
		if err = renameMigrationColumn(ctx, db, table, "task_id", "thread_id"); err != nil {
			return err
		}
	}
	if _, err = db.ExecContext(ctx, `DROP TABLE IF EXISTS current_task; DROP TABLE IF EXISTS tasks;
		CREATE INDEX IF NOT EXISTS idx_threads_owner
			ON threads(tenant_id, person_id, visibility, last_activity_at);
		CREATE INDEX IF NOT EXISTS idx_runs_thread_started
			ON runs(tenant_id, thread_id, started_at);
		CREATE INDEX IF NOT EXISTS idx_runs_person_status
			ON runs(tenant_id, person_id, status, started_at);
		CREATE INDEX IF NOT EXISTS idx_runs_attention
			ON runs(tenant_id, person_id, status, attention_dismissed_at, started_at);`); err != nil {
		return err
	}
	return nil
}

func migrationTableExists(ctx context.Context, db migrationDB, table string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count)
	return count == 1, err
}

// dropMigrationColumn removes a column that is no longer read, and is a no-op
// when the database never had it. Tolerance is the same rule the rename helper
// follows: a migration describes the END state, and a database that already
// matches it is not an error. Without this an install whose aggregate predates
// the column would fail the whole upgrade on a column it never carried.
func dropMigrationColumn(ctx context.Context, db migrationDB, table, column string) error {
	tableExists, err := migrationTableExists(ctx, db, table)
	if err != nil || !tableExists {
		return err
	}
	columnExists, err := migrationColumnExists(ctx, db, table, column)
	if err != nil || !columnExists {
		return err
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE `+table+` DROP COLUMN `+column)
	return err
}

func renameMigrationColumn(ctx context.Context, db migrationDB, table, oldName, newName string) error {
	exists, err := migrationTableExists(ctx, db, table)
	if err != nil || !exists {
		return err
	}
	oldExists, err := migrationColumnExists(ctx, db, table, oldName)
	if err != nil || !oldExists {
		return err
	}
	newExists, err := migrationColumnExists(ctx, db, table, newName)
	if err != nil || newExists {
		return err
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE `+table+` RENAME COLUMN `+oldName+` TO `+newName)
	return err
}

func ensureMigrationColumn(ctx context.Context, db migrationDB, table, name, definition string) error {
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
	// MissingTables are tables the current version requires that this
	// database, already stamped with that version, does not have.
	MissingTables []string
}

// amendedCurrentSchemaTables were added to the current version after some
// development builds had already stamped databases with it. Such a database
// opens without migrating, so schema health checks for these by name.
var amendedCurrentSchemaTables = []string{mcpReadOnlyMarksTable}

// SchemaStatus reports the schema accepted by this process and the backup made
// by this OpenStore call, if it crossed a migration boundary.
func (s *Store) SchemaStatus() StoreSchemaStatus {
	if s == nil {
		return StoreSchemaStatus{CurrentVersion: CurrentControlSchemaVersion}
	}
	return StoreSchemaStatus{
		Version: s.schemaVersion, CurrentVersion: CurrentControlSchemaVersion,
		MigrationBackup: s.migrationBackup, MissingTables: append([]string(nil), s.missingTables...),
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

func (s *Store) prepareAndMigrateSchema(ctx context.Context, dataDir, dbPath string, existing bool, checkIntegrity controlIntegrityCheck, migrations []schemaMigration) error {
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
		for _, table := range amendedCurrentSchemaTables {
			exists, err := migrationTableExists(ctx, s.db, table)
			if err != nil {
				return fmt.Errorf("inspect control schema: %w", err)
			}
			if !exists {
				s.missingTables = append(s.missingTables, table)
			}
		}
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

	// A database already presenting the v11 Thread/Run shape must not run the
	// legacy additive InitSchema: doing so would recreate tasks/task_runs beside
	// threads/runs and manufacture a second aggregate source. So adopt the shape
	// it unmistakably has — up to threadedWorkHistorySchemaVersion and no
	// further — and then let the ordinary ordered loop below apply every step
	// above it. Claiming the whole ledger here would stamp those steps as
	// applied without running them.
	modernShape, shapeErr := hasThreadedWorkHistorySchema(ctx, s.db)
	if shapeErr != nil {
		return migrationFailure(shapeErr, s.migrationBackup)
	}
	if modernShape {
		// The schedule is the only pre-v11 object that older recovery tests and
		// released installs may legitimately lack while already presenting the
		// unmistakable Thread shape; its migration is fully idempotent.
		if version < 2 {
			if err := migrations[0].Apply(ctx, s.db); err != nil {
				return migrationFailure(err, s.migrationBackup)
			}
		}
		if err := ensureSchemaMigrationsTable(ctx, s.db); err != nil {
			return migrationFailure(err, s.migrationBackup)
		}
		if err := recordSchemaMigration(ctx, s.db, schemaBaselineVersion, "legacy-baseline"); err != nil {
			return migrationFailure(err, s.migrationBackup)
		}
		for _, migration := range migrations {
			if migration.Version > threadedWorkHistorySchemaVersion {
				break
			}
			if err := recordSchemaMigration(ctx, s.db, migration.Version, migration.Name); err != nil {
				return migrationFailure(err, s.migrationBackup)
			}
		}
		if version < threadedWorkHistorySchemaVersion {
			version = threadedWorkHistorySchemaVersion
		}
	} else {
		// Version 1 adopts the historical additive schema as a compatibility
		// baseline. All subsequent durable changes must be explicit ordered
		// migrations rather than more implicit OpenStore side effects.
		if err := s.InitSchema(ctx); err != nil {
			return migrationFailure(err, s.migrationBackup)
		}
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
	for _, migration := range migrations {
		if migration.Version <= version {
			continue
		}
		if err := applyMigrationStep(ctx, s.db, migration, before); err != nil {
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

// applyMigrationStep commits a step's changes, its invariant check, and its
// ledger row in one transaction; SQLite DDL is transactional. A step that
// fails or whose result breaks an invariant leaves nothing behind, so the
// database stays at the previous version and every later open runs the step,
// and its check, again. Recording before verifying let a refused step's
// changes stand under a ledger row that the next open trusted.
//
// The step takes the write lock before its first statement. Every step starts
// by reading the schema, and a deferred transaction that has read cannot wait
// for the lock: SQLite returns SQLITE_BUSY at once, without the busy timeout,
// while another connection (an older daemon) writes. BEGIN IMMEDIATE waits
// for that writer within the timeout instead.
func applyMigrationStep(ctx context.Context, db *sql.DB, migration schemaMigration, before migrationInvariants) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin migration %d (%s): %w", migration.Version, migration.Name, err)
	}
	committed := false
	defer func() {
		if !committed {
			// A failed statement may already have ended the transaction; then
			// there is nothing to roll back.
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()
	if err := migration.Apply(ctx, conn); err != nil {
		return fmt.Errorf("apply migration %d (%s): %w", migration.Version, migration.Name, err)
	}
	if err := verifyMigrationInvariants(ctx, conn, before); err != nil {
		return fmt.Errorf("verify migration %d (%s): %w", migration.Version, migration.Name, err)
	}
	if err := recordSchemaMigration(ctx, conn, migration.Version, migration.Name); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("commit migration %d (%s): %w", migration.Version, migration.Name, err)
	}
	committed = true
	return nil
}

func hasThreadedWorkHistorySchema(ctx context.Context, db *sql.DB) (bool, error) {
	for _, table := range []string{"threads", "runs"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			return false, err
		}
		if count != 1 {
			return false, nil
		}
	}
	for _, retired := range []string{"tasks", "task_runs", "current_task"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, retired).Scan(&count); err != nil {
			return false, err
		}
		if count != 0 {
			return false, nil
		}
	}
	var columns int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('runs')
		WHERE name IN ('thread_id', 'attention_dismissed_at', 'attention_dismissed_by')`).Scan(&columns); err != nil {
		return false, err
	}
	return columns == 3, nil
}

func recordSchemaMigration(ctx context.Context, db migrationDB, version int, name string) error {
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
	name := fmt.Sprintf("control-v%d-to-v%d-%s.db", fromVersion, toVersion, time.Now().UTC().Format(controlBackupTimeLayout))
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

// controlBackupTimeLayout stamps each migration backup's name; pruning reads
// the time back from it.
const controlBackupTimeLayout = "20060102T150405.000000000Z"

// pruneControlBackups keeps the newest retain migration backups by the time in
// their names. Sorting whole names put control-v17-… before control-v8-…, so
// every two-digit migration deleted the previous backup and kept the oldest
// ones. A name without a readable time is never deleted.
func pruneControlBackups(dir, keepPath string, retain int) error {
	if retain < 1 {
		retain = 1
	}
	paths, err := filepath.Glob(filepath.Join(dir, "control-v*-to-v*.db"))
	if err != nil {
		return err
	}
	type datedBackup struct {
		path string
		at   time.Time
	}
	var backups []datedBackup
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".db")
		at, err := time.Parse(controlBackupTimeLayout, name[strings.LastIndex(name, "-")+1:])
		if err != nil {
			continue
		}
		backups = append(backups, datedBackup{path: path, at: at})
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].at.Before(backups[j].at) })
	removeCount := len(backups) - retain
	for _, backup := range backups {
		if removeCount <= 0 {
			break
		}
		if backup.path == keepPath {
			continue
		}
		if err := os.Remove(backup.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		removeCount--
	}
	return nil
}

// RestoreControlDatabase replaces control.db with one SelfMind control backup. The
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
	base := filepath.Base(absBackup)
	if (!strings.HasPrefix(base, "control-v") && !strings.HasPrefix(base, "control-before-work-history-reset-")) ||
		!strings.HasSuffix(base, ".db") {
		return "", fmt.Errorf("backup name is not a SelfMind control snapshot")
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

// migrationInvariants is the safety snapshot compared before and after every
// migration step: state buckets must match exactly, and no step may leave a
// subordinate row pointing at an aggregate that no longer exists.
type migrationInvariants struct {
	stateCounts map[string]map[string]int64
	// orphanRefs counts, per subordinate table, rows whose non-empty aggregate
	// reference (task_id before v11, thread_id after) names no aggregate row.
	// Pre-existing dangling history is tolerated so an upgrade cannot strand a
	// person's database over old rows, but a step may never add to it.
	orphanRefs map[string]int64
	// recordedFacts counts values only live work may record: admission
	// snapshots, effect ordering, person settlements, human-wait bindings,
	// clarification parks, cancellation quote sources, inbound claim
	// generations, and person read-only markings of MCP tools. A step may
	// neither manufacture them for history nor discard them. A table or column
	// that does not exist yet counts as zero.
	recordedFacts map[string]int64
}

// recordedFactSpecs name the recorded facts by table, column (empty for a
// whole-table count) and the predicate that marks a recorded value.
var recordedFactSpecs = []struct{ table, column, predicate string }{
	{"runs", "admission_json", "admission_json <> ''"},
	{"task_queue", "admission_json", "admission_json <> ''"},
	{"external_effect_claims", "sequence", "sequence <> 0"},
	{"external_watches", "sequence", "sequence <> 0"},
	{"external_effect_settlements", "", "1 = 1"},
	{"loop_checkpoints", "wait_kind", "wait_kind <> ''"},
	{"loop_checkpoints", "wait_ref", "wait_ref <> ''"},
	{"loop_checkpoints", "wait_tool_call_id", "wait_tool_call_id <> ''"},
	{"clarify_requests", "parked_at", "parked_at <> 0"},
	{"run_plan_steps", "cancellation_quote_source", "cancellation_quote_source <> ''"},
	{"inbound_dedup", "claim_generation", "claim_generation <> 0"},
	{"mcp_read_only_marks", "", "1 = 1"},
}

// aggregateReferenceTables are the subordinate tables whose aggregate
// reference the v11 step renames from task_id to thread_id.
var aggregateReferenceTables = []struct{ oldTable, newTable string }{
	{"task_runs", "runs"},
	{"task_events", "task_events"},
	{"task_handoffs", "task_handoffs"},
	{"task_artifacts", "task_artifacts"},
	{"approval_requests", "approval_requests"},
	{"clarify_requests", "clarify_requests"},
	{"external_watches", "external_watches"},
	{"task_queue", "task_queue"},
}

func captureMigrationInvariants(ctx context.Context, db migrationDB) (migrationInvariants, error) {
	out := migrationInvariants{stateCounts: make(map[string]map[string]int64)}
	for _, spec := range []struct {
		key, oldTable, newTable, state string
	}{
		{"approval_requests", "approval_requests", "approval_requests", "status"},
		{"runs", "task_runs", "runs", "status"},
		{"task_queue", "task_queue", "task_queue", "status"},
		// Thread has deliberately dropped Task lifecycle status. Owner buckets
		// preserve the safety invariant that the migration neither loses nor
		// crosses a person's aggregate history.
		{"threads", "tasks", "threads", "tenant_id || char(0) || person_id"},
		// The forward continuation edge is the only ownership authority, so a
		// step may neither drop nor invent one. The column exists from v7 on;
		// older shapes have no edge bucket to compare. Its name is resolved at
		// capture time rather than hardcoded: v12 renames it, and this check
		// runs both before and after that step, so a fixed name would compare
		// the edge against nothing on one side of its own migration.
		{"run_resume_edges", "task_runs", "runs", ""},
		// Claims, watchers, and resource waits are effect evidence: a step may
		// not add, drop, observe, or re-queue one.
		{"external_effect_claims", "external_effect_claims", "external_effect_claims", "state"},
		{"external_watches", "external_watches", "external_watches", "status"},
		{"external_resource_waits", "external_resource_waits", "external_resource_waits", "status"},
	} {
		table, err := firstExistingMigrationTable(ctx, db, spec.newTable, spec.oldTable)
		if err != nil {
			return migrationInvariants{}, err
		}
		if table == "" {
			continue
		}
		state := spec.state
		if spec.key == "run_resume_edges" {
			column := ""
			for _, candidate := range []string{"resumes_run_id", "parent_run_id"} {
				hasEdge, existsErr := migrationColumnExists(ctx, db, table, candidate)
				if existsErr != nil {
					return migrationInvariants{}, existsErr
				}
				if hasEdge {
					column = candidate
					break
				}
			}
			if column == "" {
				continue
			}
			state = "CASE WHEN COALESCE(" + column + ", '') <> '' THEN 'linked' ELSE 'root' END"
		}
		buckets, err := stateCountsIfTableExists(ctx, db, table, state)
		if err != nil {
			return migrationInvariants{}, err
		}
		if buckets != nil {
			out.stateCounts[spec.key] = buckets
		}
	}
	orphans, err := captureAggregateOrphanRefs(ctx, db)
	if err != nil {
		return migrationInvariants{}, err
	}
	out.orphanRefs = orphans
	out.recordedFacts = make(map[string]int64, len(recordedFactSpecs))
	for _, spec := range recordedFactSpecs {
		key := spec.table
		if spec.column != "" {
			key += "." + spec.column
		}
		table, err := firstExistingMigrationTable(ctx, db, spec.table)
		if err != nil {
			return migrationInvariants{}, err
		}
		present := table != ""
		if present && spec.column != "" {
			if present, err = migrationColumnExists(ctx, db, spec.table, spec.column); err != nil {
				return migrationInvariants{}, err
			}
		}
		if !present {
			out.recordedFacts[key] = 0
			continue
		}
		var count int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+spec.table+` WHERE `+spec.predicate).Scan(&count); err != nil {
			return migrationInvariants{}, err
		}
		out.recordedFacts[key] = count
	}
	return out, nil
}

func migrationColumnExists(ctx context.Context, db migrationDB, table, column string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count)
	return count == 1, err
}

// captureAggregateOrphanRefs counts subordinate rows whose aggregate reference
// names no existing tasks/threads row. An empty reference is not a reference:
// queued new work, for example, has no Thread yet.
func captureAggregateOrphanRefs(ctx context.Context, db migrationDB) (map[string]int64, error) {
	aggregate, err := firstExistingMigrationTable(ctx, db, "threads", "tasks")
	if err != nil || aggregate == "" {
		return nil, err
	}
	out := make(map[string]int64)
	for _, spec := range aggregateReferenceTables {
		table, err := firstExistingMigrationTable(ctx, db, spec.newTable, spec.oldTable)
		if err != nil {
			return nil, err
		}
		if table == "" {
			continue
		}
		column := ""
		for _, candidate := range []string{"thread_id", "task_id"} {
			exists, err := migrationColumnExists(ctx, db, table, candidate)
			if err != nil {
				return nil, err
			}
			if exists {
				column = candidate
				break
			}
		}
		if column == "" {
			continue
		}
		var count int64
		if err := db.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT COUNT(*) FROM %s WHERE COALESCE(%s, '') <> '' AND %s NOT IN (SELECT id FROM %s)`,
			table, column, column, aggregate)).Scan(&count); err != nil {
			return nil, err
		}
		out[spec.newTable] = count
	}
	return out, nil
}

func firstExistingMigrationTable(ctx context.Context, db migrationDB, names ...string) (string, error) {
	for _, name := range names {
		var exists int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&exists); err != nil {
			return "", err
		}
		if exists == 1 {
			return name, nil
		}
	}
	return "", nil
}

func stateCountsIfTableExists(ctx context.Context, db migrationDB, table, stateColumn string) (map[string]int64, error) {
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

func verifyMigrationInvariants(ctx context.Context, db migrationDB, before migrationInvariants) error {
	after, err := captureMigrationInvariants(ctx, db)
	if err != nil {
		return err
	}
	for table, want := range before.stateCounts {
		if got := after.stateCounts[table]; !sameStateCounts(want, got) {
			return fmt.Errorf("migration changed %s state counts: before=%s after=%s", table, formatStateCounts(want), formatStateCounts(got))
		}
	}
	for table, want := range before.orphanRefs {
		if got := after.orphanRefs[table]; got > want {
			return fmt.Errorf("migration left %d %s row(s) referencing a missing thread (before=%d, after=%d)", got-want, table, want, got)
		}
	}
	for fact, want := range before.recordedFacts {
		if got := after.recordedFacts[fact]; got != want {
			return fmt.Errorf("migration changed recorded %s values (before=%d, after=%d); only live work may record them", fact, want, got)
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
