package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
	"selfmind/internal/executionenv"
)

const DefaultTenantID = "default"

type Store struct {
	db              *sql.DB
	events          *eventAppendBus
	schemaVersion   int
	migrationBackup string
}

type IdentityContext struct {
	TenantID       string `json:"tenant_id"`
	PersonID       string `json:"person_id"`
	AccountID      string `json:"account_id"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id"`
	DisplayName    string `json:"display_name,omitempty"`
}

type Workspace struct {
	ID            string     `json:"id"`
	TenantID      string     `json:"tenant_id"`
	OwnerPersonID string     `json:"owner_person_id"`
	Name          string     `json:"name"`
	RepoURL       string     `json:"repo_url,omitempty"`
	LocalPath     string     `json:"local_path"`
	DefaultBranch string     `json:"default_branch,omitempty"`
	AllowedRoots  []string   `json:"allowed_roots,omitempty"`
	Status        string     `json:"status"`
	TrustLevel    string     `json:"trust_level"`
	TrustSource   string     `json:"trust_source,omitempty"`
	TrustedAt     *time.Time `json:"trusted_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type Task struct {
	ID             string     `json:"id"`
	TenantID       string     `json:"tenant_id"`
	PersonID       string     `json:"person_id"`
	WorkspaceID    string     `json:"workspace_id,omitempty"`
	Title          string     `json:"title"`
	Status         string     `json:"status"`
	Kind           string     `json:"kind,omitempty"`
	Visibility     string     `json:"visibility,omitempty"`
	Pinned         bool       `json:"pinned,omitempty"`
	CurrentSummary string     `json:"current_summary,omitempty"`
	NextSteps      []string   `json:"next_steps,omitempty"`
	BlockedReason  string     `json:"blocked_reason,omitempty"`
	ActiveRunID    string     `json:"active_run_id,omitempty"`
	LastChannel    string     `json:"last_channel,omitempty"`
	ArchivedAt     *time.Time `json:"archived_at,omitempty"`
	LastActivityAt time.Time  `json:"last_activity_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type Run struct {
	ID           string     `json:"id"`
	TaskID       string     `json:"task_id"`
	TenantID     string     `json:"tenant_id"`
	PersonID     string     `json:"person_id"`
	WorkspaceID  string     `json:"workspace_id,omitempty"`
	Channel      string     `json:"channel"`
	InputSummary string     `json:"input_summary,omitempty"`
	WorkKey      string     `json:"work_key,omitempty"`
	WorkUnitID   string     `json:"work_unit_id,omitempty"`
	Status       string     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

type Event struct {
	ID             string          `json:"id"`
	Cursor         int64           `json:"cursor,omitempty"`
	TenantID       string          `json:"tenant_id,omitempty"`
	PersonID       string          `json:"person_id,omitempty"`
	TaskID         string          `json:"task_id"`
	RunID          string          `json:"run_id,omitempty"`
	Type           string          `json:"type"`
	Visibility     string          `json:"visibility"`
	Channel        string          `json:"channel,omitempty"`
	Payload        json.RawMessage `json:"payload_json,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

type Handoff struct {
	ID           string    `json:"id"`
	TaskID       string    `json:"task_id"`
	Summary      string    `json:"summary"`
	DoneItems    []string  `json:"done_items,omitempty"`
	NextSteps    []string  `json:"next_steps,omitempty"`
	ChangedFiles []string  `json:"changed_files,omitempty"`
	TestStatus   string    `json:"test_status,omitempty"`
	Risks        []string  `json:"risks,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type Artifact struct {
	ID        string          `json:"id"`
	TaskID    string          `json:"task_id"`
	RunID     string          `json:"run_id,omitempty"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name,omitempty"`
	URI       string          `json:"uri"`
	MimeType  string          `json:"mime_type,omitempty"`
	Metadata  json.RawMessage `json:"metadata_json,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type TaskCreate struct {
	// ID is optional. Most callers leave it empty and receive a generated task
	// id. Durable replay paths may supply a stable task_ id so a crash between
	// creation and completion reuses the same display label.
	ID          string
	TenantID    string
	PersonID    string
	WorkspaceID string
	Title       string
	Channel     string
	Kind        string
	Visibility  string
	Pinned      bool
	// KeepCurrent creates the task without changing the person's current-task
	// pointer. Post-run display governance uses this when it splits one
	// completed run from a weak pre-label: creation must not race a newer user
	// selection or acquire execution authority retroactively.
	KeepCurrent bool
}

func OpenStore(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("data dir is required")
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dbPath := filepath.Join(dataDir, "control.db")
	existing, err := nonEmptyRegularFile(dbPath)
	if err != nil {
		return nil, fmt.Errorf("inspect control store: %w", err)
	}
	db, err := sql.Open("sqlite", dbPath+"?_journal=WAL&_sync=NORMAL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure sqlite: %w", err)
	}
	store := &Store{db: db, events: newEventAppendBus()}
	if err := store.prepareAndMigrateSchema(context.Background(), dataDir, dbPath, existing, quickCheckDB); err != nil {
		db.Close()
		return nil, err
	}
	tightenStorePerms(dataDir, dbPath)
	return store, nil
}

// tightenStorePerms keeps conversation history and account bindings private on
// shared hosts: SQLite creates files 0644 by default, and pre-existing installs
// created the data dir 0755. Best-effort — chmod is a no-op on some filesystems
// (e.g. Windows) and must never block startup.
func tightenStorePerms(dataDir, dbPath string) {
	_ = os.Chmod(dataDir, 0700)
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			_ = os.Chmod(p, 0600)
		}
	}
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) InitSchema(ctx context.Context) error {
	schema := `
CREATE TABLE IF NOT EXISTS tenants (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT 'personal',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS persons (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	display_name TEXT,
	default_workspace_id TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS accounts (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	platform TEXT NOT NULL,
	platform_user_id TEXT NOT NULL,
	display_name TEXT,
	status TEXT NOT NULL DEFAULT 'active',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(tenant_id, platform, platform_user_id)
);
CREATE TABLE IF NOT EXISTS workspaces (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	owner_person_id TEXT NOT NULL,
	name TEXT NOT NULL,
	repo_url TEXT,
	local_path TEXT NOT NULL,
	default_branch TEXT,
	allowed_roots_json TEXT,
	status TEXT NOT NULL DEFAULT 'active',
	trust_level TEXT NOT NULL DEFAULT 'untrusted',
	trust_source TEXT,
	trusted_at INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(tenant_id, owner_person_id, local_path)
);
CREATE TABLE IF NOT EXISTS current_workspace (
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(tenant_id, person_id)
);
CREATE TABLE IF NOT EXISTS workspace_knowledge_sections (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL,
	file_path TEXT NOT NULL,
	file_name TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	file_mtime INTEGER NOT NULL DEFAULT 0,
	section_index INTEGER NOT NULL,
	title TEXT NOT NULL,
	excerpt TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(tenant_id, person_id, workspace_id, file_path, section_index)
);
CREATE INDEX IF NOT EXISTS idx_workspace_knowledge_scope
	ON workspace_knowledge_sections(tenant_id, person_id, workspace_id, updated_at);
CREATE TABLE IF NOT EXISTS execution_leases (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL UNIQUE,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT,
	environment_profile TEXT NOT NULL,
	credential_refs_json TEXT NOT NULL DEFAULT '[]',
	principal_fingerprint TEXT,
	capabilities_json TEXT NOT NULL DEFAULT '[]',
	environment_snapshot_id TEXT NOT NULL DEFAULT '',
	environment_generation INTEGER NOT NULL DEFAULT 0,
	environment_fingerprint TEXT NOT NULL DEFAULT '',
	credential_source_hash TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_execution_leases_person
	ON execution_leases(tenant_id, person_id, created_at);
CREATE TABLE IF NOT EXISTS execution_capability_grants (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL,
	capability TEXT NOT NULL,
	resource_fingerprint TEXT NOT NULL DEFAULT '',
	granted_by TEXT,
	expires_at INTEGER NOT NULL,
	revoked_at INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(tenant_id, person_id, workspace_id, capability, resource_fingerprint)
);
CREATE INDEX IF NOT EXISTS idx_execution_capability_active
	ON execution_capability_grants(tenant_id, person_id, workspace_id, capability, expires_at);
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT,
	title TEXT NOT NULL,
	status TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'work',
	visibility TEXT NOT NULL DEFAULT 'visible',
	pinned INTEGER NOT NULL DEFAULT 0,
	current_summary TEXT,
	next_steps_json TEXT,
	blocked_reason TEXT,
	active_run_id TEXT,
	last_channel TEXT,
	archived_at INTEGER,
	last_activity_at INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_owner ON tasks(tenant_id, person_id, updated_at);
CREATE TABLE IF NOT EXISTS current_task (
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(tenant_id, person_id)
);
CREATE TABLE IF NOT EXISTS task_runs (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT,
	channel TEXT NOT NULL,
	input_summary TEXT,
	status TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	finished_at INTEGER,
	heartbeat_at INTEGER,
	cancel_requested INTEGER NOT NULL DEFAULT 0,
	last_error TEXT,
	resumed_by_run_id TEXT NOT NULL DEFAULT '',
	work_key TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_task_runs_task_started ON task_runs(tenant_id, task_id, started_at);
CREATE INDEX IF NOT EXISTS idx_task_runs_person_status ON task_runs(tenant_id, person_id, status, started_at);
CREATE TABLE IF NOT EXISTS task_references (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	class TEXT NOT NULL,
	raw_value TEXT NOT NULL,
	normalized_value TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'shadow',
	user_confirmed INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(tenant_id, person_id, normalized_value, task_id)
);
CREATE INDEX IF NOT EXISTS idx_task_references_owner_value
	ON task_references(tenant_id, person_id, normalized_value, status);
CREATE INDEX IF NOT EXISTS idx_task_references_task
	ON task_references(tenant_id, task_id, status, updated_at);
CREATE TABLE IF NOT EXISTS task_reference_evidence (
	id TEXT PRIMARY KEY,
	reference_id TEXT NOT NULL,
	run_id TEXT NOT NULL DEFAULT '',
	provenance TEXT NOT NULL,
	source_ref TEXT NOT NULL DEFAULT '',
	evidence_hash TEXT NOT NULL,
	observed_at INTEGER NOT NULL,
	UNIQUE(reference_id, run_id, provenance, evidence_hash)
);
CREATE INDEX IF NOT EXISTS idx_task_reference_evidence_ref
	ON task_reference_evidence(reference_id, provenance, observed_at);
CREATE TABLE IF NOT EXISTS task_resolution_events (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	run_id TEXT NOT NULL DEFAULT '',
	input_hash TEXT NOT NULL,
	matched_surface_forms_json TEXT NOT NULL DEFAULT '[]',
	unmatched_salient_tokens_json TEXT NOT NULL DEFAULT '[]',
	candidates_json TEXT NOT NULL DEFAULT '[]',
	selected_task_id TEXT NOT NULL DEFAULT '',
	final_task_id TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL DEFAULT 'unverified',
	attach_policy_json TEXT NOT NULL DEFAULT '{}',
	analyzer_evaluated INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_task_resolution_events_owner
	ON task_resolution_events(tenant_id, person_id, created_at);
CREATE INDEX IF NOT EXISTS idx_task_resolution_events_run
	ON task_resolution_events(tenant_id, run_id, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_task_resolution_events_run_unique
	ON task_resolution_events(tenant_id, run_id) WHERE run_id != '';
CREATE TABLE IF NOT EXISTS task_blockers (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	origin_run_id TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'open',
	detail_json TEXT NOT NULL DEFAULT '{}',
	resolved_by_run_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	resolved_at INTEGER,
	UNIQUE(origin_run_id, kind)
);
CREATE INDEX IF NOT EXISTS idx_task_blockers_open
	ON task_blockers(tenant_id, task_id, status, created_at);
CREATE TABLE IF NOT EXISTS task_events (
	id TEXT PRIMARY KEY,
	cursor INTEGER,
	task_id TEXT NOT NULL,
	run_id TEXT,
	type TEXT NOT NULL,
	visibility TEXT NOT NULL DEFAULT 'task',
	channel TEXT,
	payload_json TEXT,
	idempotency_key TEXT,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_task_events_task ON task_events(task_id, created_at);
CREATE TABLE IF NOT EXISTS event_sequence (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	next_cursor INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS channel_messages (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	account_id TEXT,
	channel TEXT NOT NULL,
	task_id TEXT,
	role TEXT NOT NULL,
	content TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_channel_messages_person_channel ON channel_messages(tenant_id, person_id, channel, created_at);
CREATE TABLE IF NOT EXISTS task_handoffs (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL,
	summary TEXT NOT NULL,
	done_items_json TEXT,
	next_steps_json TEXT,
	changed_files_json TEXT,
	test_status TEXT,
	risks_json TEXT,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_task_handoffs_task_created ON task_handoffs(task_id, created_at);
CREATE TABLE IF NOT EXISTS task_artifacts (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL,
	run_id TEXT,
	kind TEXT NOT NULL,
	name TEXT,
	uri TEXT NOT NULL,
	mime_type TEXT,
	metadata_json TEXT,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_task_artifacts_task ON task_artifacts(task_id, created_at);
CREATE TABLE IF NOT EXISTS approval_requests (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT,
	run_id TEXT,
	action_type TEXT NOT NULL,
	payload_json TEXT,
	status TEXT NOT NULL,
	requested_channel TEXT,
	approved_channel TEXT,
	decision_scope TEXT,
	decision_id TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_approval_pending ON approval_requests(tenant_id, person_id, status, created_at);
CREATE TABLE IF NOT EXISTS approval_grants (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	scope_kind TEXT NOT NULL,
	scope_id TEXT NOT NULL,
	pattern_key TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL DEFAULT 0,
	revoked_at INTEGER NOT NULL DEFAULT 0,
	UNIQUE(tenant_id, person_id, scope_kind, scope_id, pattern_key)
);
CREATE INDEX IF NOT EXISTS idx_approval_grants_lookup ON approval_grants(tenant_id, person_id, pattern_key);
CREATE TABLE IF NOT EXISTS clarify_requests (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT,
	run_id TEXT,
	question TEXT NOT NULL,
	options_json TEXT,
	status TEXT NOT NULL,
	answer TEXT,
	channel TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_clarify_pending ON clarify_requests(tenant_id, person_id, status, created_at);
CREATE TABLE IF NOT EXISTS notifications (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	channel TEXT NOT NULL,
	task_id TEXT,
	event_id TEXT,
	status TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS outbound_messages (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	platform TEXT NOT NULL,
	platform_user_id TEXT,
	channel TEXT NOT NULL,
	task_id TEXT,
	run_id TEXT,
	content TEXT NOT NULL,
	kind TEXT,
	approval_id TEXT,
	status TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	max_attempts INTEGER NOT NULL DEFAULT 3,
	next_attempt_at INTEGER NOT NULL,
	last_error TEXT,
	part_index INTEGER NOT NULL DEFAULT 1,
	part_total INTEGER NOT NULL DEFAULT 1,
	idempotency_key TEXT,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	delivered_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_outbound_due ON outbound_messages(status, next_attempt_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_outbound_idempotency ON outbound_messages(idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '';
CREATE TABLE IF NOT EXISTS person_settings (
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(tenant_id, person_id, key)
);
CREATE TABLE IF NOT EXISTS task_queue (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	channel TEXT NOT NULL,
	platform TEXT NOT NULL,
	platform_user_id TEXT,
	content TEXT NOT NULL,
	approval_mode TEXT,
	workspace_id TEXT,
	task_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL DEFAULT '',
	idempotency_key TEXT NOT NULL DEFAULT '',
	class TEXT NOT NULL DEFAULT 'foreground',
	priority INTEGER NOT NULL DEFAULT 100,
	not_before INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'queued',
	restarts INTEGER NOT NULL DEFAULT 0,
	claim_token TEXT NOT NULL DEFAULT '',
	lease_until INTEGER NOT NULL DEFAULT 0,
	attempt_generation INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_task_queue_person ON task_queue(tenant_id, person_id, status, created_at);
CREATE TABLE IF NOT EXISTS effect_receipts (
	effect_key TEXT NOT NULL,
	tenant_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	delivery_enqueued INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, effect_key)
);
CREATE INDEX IF NOT EXISTS idx_effect_receipts_run ON effect_receipts(tenant_id, run_id);
CREATE TABLE IF NOT EXISTS inbound_dedup (
	platform TEXT NOT NULL,
	message_id TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (platform, message_id)
);
CREATE INDEX IF NOT EXISTS idx_inbound_dedup_created ON inbound_dedup(created_at);
CREATE TABLE IF NOT EXISTS maintenance_jobs (
	run_id TEXT NOT NULL,
	analyzer_version INTEGER NOT NULL DEFAULT 1,
	tenant_id TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	attempts INTEGER NOT NULL DEFAULT 0,
	next_retry_at INTEGER NOT NULL DEFAULT 0,
	result_hash TEXT NOT NULL DEFAULT '',
	payload_json TEXT NOT NULL DEFAULT '',
	proposal_json TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (run_id, analyzer_version)
);
CREATE INDEX IF NOT EXISTS idx_maintenance_jobs_status ON maintenance_jobs(tenant_id, status, next_retry_at);
CREATE TABLE IF NOT EXISTS maintenance_attempts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id TEXT NOT NULL,
	analyzer_version INTEGER NOT NULL DEFAULT 1,
	tenant_id TEXT NOT NULL,
	attempt INTEGER NOT NULL DEFAULT 0,
	outcome TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	route_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_maintenance_attempts_job
	ON maintenance_attempts(tenant_id, run_id, analyzer_version, created_at);
CREATE INDEX IF NOT EXISTS idx_maintenance_attempts_recent
	ON maintenance_attempts(tenant_id, created_at);
CREATE TABLE IF NOT EXISTS provider_route_health (
	tenant_id TEXT NOT NULL,
	route_id TEXT NOT NULL,
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'closed',
	failure_class TEXT NOT NULL DEFAULT '',
	consecutive_failures INTEGER NOT NULL DEFAULT 0,
	opened_at INTEGER NOT NULL DEFAULT 0,
	next_probe_at INTEGER NOT NULL DEFAULT 0,
	probe_lease_until INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	last_request_id TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (tenant_id, route_id)
);
CREATE INDEX IF NOT EXISTS idx_provider_route_probe
	ON provider_route_health(tenant_id, state, next_probe_at);
CREATE TABLE IF NOT EXISTS maintenance_provider_calls (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL DEFAULT '',
	task_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL DEFAULT '',
	role TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	route_id TEXT NOT NULL DEFAULT '',
	candidate_index INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL,
	trigger_class TEXT NOT NULL DEFAULT '',
	finish_reason TEXT NOT NULL DEFAULT '',
	error_class TEXT NOT NULL DEFAULT '',
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
	cache_miss_input_tokens INTEGER NOT NULL DEFAULT 0,
	cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_usage_reported INTEGER NOT NULL DEFAULT 0,
	batch_size INTEGER NOT NULL DEFAULT 1,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_maintenance_provider_calls_recent
	ON maintenance_provider_calls(tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_maintenance_provider_calls_route
	ON maintenance_provider_calls(tenant_id, route_id, created_at);
CREATE TABLE IF NOT EXISTS approval_triage_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL DEFAULT '',
	tool_name TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL,
	risk_level TEXT NOT NULL DEFAULT '',
	user_authorization TEXT NOT NULL DEFAULT '',
	grant_key TEXT NOT NULL DEFAULT '',
	provider_route TEXT NOT NULL DEFAULT '',
	latency_ms INTEGER NOT NULL DEFAULT 0,
	error_class TEXT NOT NULL DEFAULT '',
	policy_version TEXT NOT NULL DEFAULT '',
	rationale TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_approval_triage_events_recent
	ON approval_triage_events(tenant_id, person_id, created_at);
CREATE TABLE IF NOT EXISTS gateway_runtime_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	instance_id TEXT NOT NULL,
	event_type TEXT NOT NULL,
	payload_json TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL,
	UNIQUE(instance_id, event_type)
);
CREATE INDEX IF NOT EXISTS idx_gateway_runtime_events_recent
	ON gateway_runtime_events(created_at);
CREATE TABLE IF NOT EXISTS external_watches (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT,
	task_id TEXT NOT NULL,
	run_id TEXT,
	channel TEXT,
	description TEXT NOT NULL DEFAULT '',
	cwd TEXT NOT NULL,
	command TEXT NOT NULL,
	success_pattern TEXT NOT NULL,
	failure_pattern TEXT NOT NULL DEFAULT '',
	spec_version INTEGER NOT NULL DEFAULT 1,
	target_pattern TEXT NOT NULL DEFAULT '',
	terminal_success_pattern TEXT NOT NULL DEFAULT '',
	terminal_failure_pattern TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	checker_status TEXT NOT NULL DEFAULT '',
	operation_status TEXT NOT NULL DEFAULT 'pending',
	verification_status TEXT NOT NULL DEFAULT 'not_required',
	interval_seconds INTEGER NOT NULL DEFAULT 30,
	current_interval_seconds INTEGER NOT NULL DEFAULT 0,
	command_timeout_seconds INTEGER NOT NULL DEFAULT 30,
	timeout_at INTEGER NOT NULL,
	next_check_at INTEGER NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	extensions INTEGER NOT NULL DEFAULT 0,
	finalized INTEGER NOT NULL DEFAULT 0,
	verdict_revision INTEGER NOT NULL DEFAULT 1,
	notified INTEGER NOT NULL DEFAULT 0,
	last_output TEXT NOT NULL DEFAULT '',
	last_output_hash TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	execution_binding_json TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	finished_at INTEGER
);
CREATE TABLE IF NOT EXISTS steering_mailbox (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	run_id TEXT NOT NULL DEFAULT '',
	task_id TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT '',
	platform TEXT NOT NULL DEFAULT '',
	platform_user_id TEXT NOT NULL DEFAULT '',
	workspace_id TEXT NOT NULL DEFAULT '',
	approval_mode TEXT NOT NULL DEFAULT '',
	content TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'accepted',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_steering_mailbox_live
	ON steering_mailbox(tenant_id, status, created_at);
CREATE INDEX IF NOT EXISTS idx_steering_mailbox_run
	ON steering_mailbox(run_id, status);
CREATE TABLE IF NOT EXISTS loop_checkpoints (
	run_id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	iteration INTEGER NOT NULL DEFAULT 0,
	outcome TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	snapshot_json BLOB NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_loop_checkpoints_task
	ON loop_checkpoints(tenant_id, task_id, updated_at);
CREATE TABLE IF NOT EXISTS tool_ledger (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	tenant_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	tool_call_id TEXT NOT NULL,
	tool_name TEXT NOT NULL,
	args_hash TEXT NOT NULL DEFAULT '',
	retry_class TEXT NOT NULL DEFAULT 'side_effect',
	status TEXT NOT NULL DEFAULT 'planned',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(run_id, tool_call_id)
);
CREATE INDEX IF NOT EXISTS idx_tool_ledger_uncertain
	ON tool_ledger(tenant_id, run_id, status);
CREATE TABLE IF NOT EXISTS workflow_profiles (
	run_id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	workflow_signature TEXT NOT NULL,
	skill_versions_json TEXT NOT NULL DEFAULT '{}',
	plan_hash TEXT NOT NULL DEFAULT '',
	tool_sequence_json TEXT NOT NULL DEFAULT '[]',
	tool_calls INTEGER NOT NULL DEFAULT 0,
	tool_failures INTEGER NOT NULL DEFAULT 0,
	provider_calls INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	billed_input_tokens INTEGER NOT NULL DEFAULT 0,
	outcome_status TEXT NOT NULL DEFAULT '',
	verification_state TEXT NOT NULL DEFAULT '',
	read_only INTEGER NOT NULL DEFAULT 0,
	applied_candidate_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workflow_profiles_signature
	ON workflow_profiles(tenant_id, person_id, workspace_id, workflow_signature, created_at);
CREATE TABLE IF NOT EXISTS evolution_candidates (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	last_task_id TEXT NOT NULL DEFAULT '',
	workflow_signature TEXT NOT NULL,
	kind TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'candidate',
	contract_json TEXT NOT NULL DEFAULT '{}',
	repair_json TEXT NOT NULL DEFAULT '{}',
	observation_count INTEGER NOT NULL DEFAULT 0,
	shadow_runs INTEGER NOT NULL DEFAULT 0,
	shadow_matches INTEGER NOT NULL DEFAULT 0,
	fallback_count INTEGER NOT NULL DEFAULT 0,
	consecutive_failures INTEGER NOT NULL DEFAULT 0,
	last_failure TEXT NOT NULL DEFAULT '',
	enabled_at INTEGER,
	last_applied_at INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(tenant_id, person_id, workspace_id, workflow_signature, kind)
);
CREATE INDEX IF NOT EXISTS idx_evolution_candidates_active
	ON evolution_candidates(tenant_id, person_id, workspace_id, status, updated_at);
CREATE TABLE IF NOT EXISTS skill_versions (
	control_tenant_id TEXT NOT NULL,
	skill_key TEXT NOT NULL,
	skill_name TEXT NOT NULL,
	version_hash TEXT NOT NULL,
	parent_version_hash TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	content_ref TEXT NOT NULL DEFAULT '',
	content_body TEXT NOT NULL DEFAULT '',
	source_observation_ids_json TEXT NOT NULL DEFAULT '[]',
	evidence_set_hash TEXT NOT NULL DEFAULT '',
	evidence_json TEXT NOT NULL DEFAULT '{}',
	created_by TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	promoted_at INTEGER,
	PRIMARY KEY(control_tenant_id, skill_key, version_hash)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_versions_one_active
	ON skill_versions(control_tenant_id, skill_key) WHERE state = 'active';
CREATE UNIQUE INDEX IF NOT EXISTS idx_skill_versions_candidate_evidence
	ON skill_versions(control_tenant_id, evidence_set_hash)
	WHERE state = 'candidate' AND evidence_set_hash != '';
CREATE TABLE IF NOT EXISTS run_work_units (
	id TEXT PRIMARY KEY,
	identity_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL,
	sequence INTEGER NOT NULL,
	primary_task_id TEXT NOT NULL,
	related_task_id TEXT NOT NULL DEFAULT '',
	goal_digest TEXT NOT NULL DEFAULT '',
	plan_status TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	outcome_summary TEXT NOT NULL DEFAULT '',
		verification_state TEXT NOT NULL DEFAULT '',
		verification_refs_json TEXT NOT NULL DEFAULT '[]',
		started_at INTEGER,
		created_at INTEGER NOT NULL,
		finished_at INTEGER,
		started_cursor INTEGER NOT NULL DEFAULT 0,
		finished_cursor INTEGER NOT NULL DEFAULT 0,
		UNIQUE(run_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_run_work_units_run
	ON run_work_units(identity_tenant_id, run_id, sequence);
CREATE TABLE IF NOT EXISTS run_skill_activations (
	id TEXT PRIMARY KEY,
	identity_tenant_id TEXT NOT NULL,
	control_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL,
	sequence INTEGER NOT NULL,
	work_unit_id TEXT NOT NULL,
	execution_lane TEXT NOT NULL DEFAULT 'main',
	primary_task_id TEXT NOT NULL,
	related_task_id TEXT NOT NULL DEFAULT '',
	skill_key TEXT NOT NULL,
	skill_name TEXT NOT NULL,
	version_hash TEXT NOT NULL,
	activation_source TEXT NOT NULL,
	attachment_mode TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL,
	fallback_reason TEXT NOT NULL DEFAULT '',
	selected_at INTEGER NOT NULL,
	finished_at INTEGER,
	UNIQUE(run_id, sequence)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_run_skill_activations_live_lane
	ON run_skill_activations(run_id, work_unit_id, execution_lane)
	WHERE state IN ('selected', 'active');
CREATE INDEX IF NOT EXISTS idx_run_skill_activations_skill
	ON run_skill_activations(control_tenant_id, skill_key, version_hash, selected_at);
CREATE TABLE IF NOT EXISTS task_skill_bindings (
	identity_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	task_id TEXT NOT NULL,
	control_tenant_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	skill_key TEXT NOT NULL,
	skill_name TEXT NOT NULL,
	state TEXT NOT NULL,
	binding_source TEXT NOT NULL,
	bound_from_run_id TEXT NOT NULL DEFAULT '',
	last_resolved_version_hash TEXT NOT NULL DEFAULT '',
	suspended_reason TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY(identity_tenant_id, person_id, task_id)
);
CREATE INDEX IF NOT EXISTS idx_task_skill_bindings_skill
	ON task_skill_bindings(control_tenant_id, skill_key, state, updated_at);
CREATE TABLE IF NOT EXISTS skill_failure_guards (
	control_tenant_id TEXT NOT NULL,
	skill_key TEXT NOT NULL,
	version_hash TEXT NOT NULL,
	failure_signature TEXT NOT NULL,
	failed_step_id TEXT NOT NULL DEFAULT '',
	error_category TEXT NOT NULL DEFAULT '',
	normalized_input_shape TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT 'active',
	source_run_id TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	occurrence_count INTEGER NOT NULL DEFAULT 1,
	last_seen_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(control_tenant_id, skill_key, version_hash, failure_signature)
);
CREATE TABLE IF NOT EXISTS workflow_observations (
	id TEXT PRIMARY KEY,
	identity_tenant_id TEXT NOT NULL,
	control_tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL,
	work_unit_id TEXT NOT NULL UNIQUE,
	related_task_id TEXT NOT NULL DEFAULT '',
	workflow_signature TEXT NOT NULL,
	goal_digest TEXT NOT NULL DEFAULT '',
	environment_fingerprint TEXT NOT NULL DEFAULT '',
	skill_key TEXT NOT NULL DEFAULT '',
	version_hash TEXT NOT NULL DEFAULT '',
	activation_state TEXT NOT NULL DEFAULT '',
	outcome_status TEXT NOT NULL,
	verification_state TEXT NOT NULL DEFAULT '',
	tool_sequence_json TEXT NOT NULL DEFAULT '[]',
	tool_failures INTEGER NOT NULL DEFAULT 0,
	provider_calls INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	user_corrected INTEGER NOT NULL DEFAULT 0,
	evidence_role TEXT NOT NULL DEFAULT 'audit',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workflow_observations_cohort
	ON workflow_observations(identity_tenant_id, person_id, workspace_id, workflow_signature, created_at);
CREATE INDEX IF NOT EXISTS idx_task_events_run_created
	ON task_events(run_id, created_at);
CREATE INDEX IF NOT EXISTS idx_external_watches_due
	ON external_watches(status, next_check_at);
CREATE INDEX IF NOT EXISTS idx_external_watches_owner
	ON external_watches(tenant_id, person_id, status, updated_at);`

	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	for _, col := range []struct {
		table string
		name  string
		def   string
	}{
		{"task_runs", "heartbeat_at", "INTEGER"},
		{"task_runs", "cancel_requested", "INTEGER NOT NULL DEFAULT 0"},
		{"task_runs", "last_error", "TEXT"},
		{"task_runs", "resumed_by_run_id", "TEXT NOT NULL DEFAULT ''"},
		{"task_runs", "work_key", "TEXT NOT NULL DEFAULT ''"},
		{"outbound_messages", "platform_user_id", "TEXT"},
		// kind/approval_id let retried deliveries keep their typed rendering
		// (e.g. Telegram approval inline buttons) instead of degrading to plain
		// text after the first failed attempt.
		{"outbound_messages", "kind", "TEXT"},
		{"outbound_messages", "approval_id", "TEXT"},
		// last_seen_at is durable endpoint recency (unix seconds) used only to
		// pick the person's preferred notify endpoint when the CLI is detached.
		// It is NOT presence: liveness stays in the gateway's in-memory
		// registry (docs/identity-continuity.md "Runtime attachment model").
		{"accounts", "last_seen_at", "INTEGER"},
		// decision_scope carries the class-level approval memory scope
		// (""/task/person) recorded when an approval is answered; older DBs
		// created before the layered approval funnel lack the column.
		{"approval_requests", "decision_scope", "TEXT"},
		{"approval_requests", "decision_id", "TEXT"},
		// decision_grant_key stores the narrow RULE a person picked instead of the
		// action class; decision_note stores their words when they refused. Both
		// arrived with the structured-decision batch, so older DBs lack them.
		{"approval_requests", "decision_grant_key", "TEXT"},
		{"approval_requests", "decision_note", "TEXT"},
		// decision_recorded_at is an explicit compatibility fence for crash
		// recovery. Historical approved/rejected rows remain NULL and must never
		// be replayed merely because newly added waiter columns default to live.
		{"approval_requests", "decision_recorded_at", "INTEGER"},
		// Human decision state and waiter/resource state are orthogonal. A parked
		// request remains status=pending (answerable) after its original run has
		// released resources; decision claim and authorization fields close crash
		// windows around resume.
		{"approval_requests", "waiter_state", "TEXT NOT NULL DEFAULT 'live'"},
		{"approval_requests", "parked_at", "INTEGER"},
		{"approval_requests", "park_reason", "TEXT"},
		{"approval_requests", "decision_claimed_at", "INTEGER"},
		{"approval_requests", "claimed_by_run_id", "TEXT"},
		{"approval_requests", "resume_queue_id", "TEXT"},
		{"approval_requests", "authorization_fingerprint", "TEXT"},
		{"approval_requests", "authorization_state", "TEXT"},
		{"approval_requests", "authorization_expires_at", "INTEGER"},
		// notified_at (unix seconds) records when an IM notification was actually
		// SENT for a pending approval/clarify (never when a CLI-attached push was
		// suppressed). The escrow sweep uses NULL to find pendings that left the
		// person uninformed and re-pushes them once the CLI detaches (Fix 2).
		{"approval_requests", "notified_at", "INTEGER"},
		{"clarify_requests", "notified_at", "INTEGER"},
		{"approval_triage_events", "task_id", "TEXT NOT NULL DEFAULT ''"},
		{"approval_triage_events", "run_id", "TEXT NOT NULL DEFAULT ''"},
		{"approval_triage_events", "tool_name", "TEXT NOT NULL DEFAULT ''"},
		{"approval_triage_events", "risk_level", "TEXT NOT NULL DEFAULT ''"},
		{"approval_triage_events", "user_authorization", "TEXT NOT NULL DEFAULT ''"},
		{"approval_triage_events", "grant_key", "TEXT NOT NULL DEFAULT ''"},
		{"approval_triage_events", "provider_route", "TEXT NOT NULL DEFAULT ''"},
		{"approval_triage_events", "latency_ms", "INTEGER NOT NULL DEFAULT 0"},
		{"approval_triage_events", "error_class", "TEXT NOT NULL DEFAULT ''"},
		{"approval_triage_events", "policy_version", "TEXT NOT NULL DEFAULT ''"},
		{"approval_triage_events", "rationale", "TEXT NOT NULL DEFAULT ''"},
		// restarts counts boot requeues of a 'started' row. Without a cap a
		// queued task whose run never finishes before the next daemon restart
		// resurrects FOREVER (observed live: a 10-min task + repeated deploy
		// restarts spawned 5 duplicate task corpses).
		{"task_queue", "restarts", "INTEGER NOT NULL DEFAULT 0"},
		// task_id pins a system-originated queued item (external-watch
		// finalization) to its task; ordinary inbound rows leave it empty.
		{"task_queue", "task_id", "TEXT NOT NULL DEFAULT ''"},
		// run_id binds a drained queue row to the concrete run that executed it.
		// Recovery may reopen a done system row only when that run has no durable
		// terminal event; old rows with no binding are never guessed/replayed.
		{"task_queue", "run_id", "TEXT NOT NULL DEFAULT ''"},
		// idempotency_key makes system-originated enqueues (watch
		// finalization) replay-safe: a crash-recovery re-enqueue with the
		// same stable key is one row, never a duplicate run.
		{"task_queue", "idempotency_key", "TEXT NOT NULL DEFAULT ''"},
		// Queue class is an execution-quality property, not a task label. It
		// keeps interactive work, watcher closure and scheduled work from being
		// forced through one undifferentiated FIFO policy.
		{"task_queue", "class", "TEXT NOT NULL DEFAULT 'foreground'"},
		{"task_queue", "priority", "INTEGER NOT NULL DEFAULT 100"},
		{"task_queue", "not_before", "INTEGER NOT NULL DEFAULT 0"},
		// Queue claims are durable worker ownership, separate from the run
		// heartbeat. A finalization reconciler may reopen a started row only after
		// this lease expires; attempt_generation makes every ownership epoch
		// observable without changing the row's stable idempotency key.
		{"task_queue", "claim_token", "TEXT NOT NULL DEFAULT ''"},
		{"task_queue", "lease_until", "INTEGER NOT NULL DEFAULT 0"},
		{"task_queue", "attempt_generation", "INTEGER NOT NULL DEFAULT 0"},
		// verdict_revision counts terminal-verdict revisions (timed_out ->
		// succeeded). It keys all finalization products, so replaying the
		// SAME verdict creates nothing new while a REVISED verdict emits one
		// fresh correction event/run/notice.
		{"external_watches", "verdict_revision", "INTEGER NOT NULL DEFAULT 1"},
		// Task governance columns are additive and default old rows to normal,
		// visible work. No migration may hide, archive, or rename existing tasks.
		{"tasks", "kind", "TEXT NOT NULL DEFAULT 'work'"},
		{"tasks", "visibility", "TEXT NOT NULL DEFAULT 'visible'"},
		{"tasks", "pinned", "INTEGER NOT NULL DEFAULT 0"},
		{"tasks", "archived_at", "INTEGER"},
		{"tasks", "last_activity_at", "INTEGER"},
		// catchup_at (unix seconds) claims a sent_unconfirmed row's ONE catch-up
		// re-push (fired when the peer's next inbound refreshes the platform
		// session, e.g. a fresh iLink context_token). Non-zero = already caught
		// up; the row is never re-pushed again (anti-duplicate rail, P0-1).
		{"outbound_messages", "catchup_at", "INTEGER"},
		// Replay data makes post-run maintenance a real daemon job rather than a
		// one-shot goroutine. proposal_json freezes the model decision before any
		// memory mutation, so crash recovery re-applies the same proposal.
		{"maintenance_jobs", "payload_json", "TEXT NOT NULL DEFAULT ''"},
		{"maintenance_jobs", "proposal_json", "TEXT NOT NULL DEFAULT ''"},
		// blocked_route_id binds a provider-quota pause to the physical route
		// that caused it. A successful half-open probe requeues only jobs for
		// that route; unrelated provider/configuration failures remain blocked.
		{"maintenance_jobs", "blocked_route_id", "TEXT NOT NULL DEFAULT ''"},
		{"skill_failure_guards", "occurrence_count", "INTEGER NOT NULL DEFAULT 1"},
		{"skill_failure_guards", "last_seen_at", "INTEGER NOT NULL DEFAULT 0"},
		// Work-unit timing and cursor windows make outcome, verification, and cost
		// attribution independent from the enclosing run. Older rows fall back to
		// their creation/run boundaries until a later plan transition closes them.
		{"run_work_units", "started_at", "INTEGER"},
		{"run_work_units", "started_cursor", "INTEGER NOT NULL DEFAULT 0"},
		{"run_work_units", "finished_cursor", "INTEGER NOT NULL DEFAULT 0"},
		// Compatible providers such as DeepSeek report cache hit/miss and
		// reasoning-token details outside the base OpenAI usage fields.
		{"maintenance_provider_calls", "person_id", "TEXT NOT NULL DEFAULT ''"},
		{"maintenance_provider_calls", "task_id", "TEXT NOT NULL DEFAULT ''"},
		{"maintenance_provider_calls", "run_id", "TEXT NOT NULL DEFAULT ''"},
		{"maintenance_provider_calls", "cache_miss_input_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"maintenance_provider_calls", "reasoning_output_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"maintenance_provider_calls", "cache_usage_reported", "INTEGER NOT NULL DEFAULT 0"},
		// cursor is a daemon-wide, never-reused append sequence for resumable
		// event streams. It cannot rely on SQLite's implicit rowid because
		// cleanup or VACUUM may reuse/change rowids.
		{"task_events", "cursor", "INTEGER"},
		// System-originated event products use a stable key so a crash replay
		// cannot append the same logical event twice. Ordinary events leave it
		// NULL and retain append-only behavior.
		{"task_events", "idempotency_key", "TEXT"},
		// extensions bounds automatic deadline extension of an external watch
		// to exactly one grant, so a build that outlives its window gets one
		// extra bounded wait instead of an endless rolling deadline.
		{"external_watches", "extensions", "INTEGER NOT NULL DEFAULT 0"},
		// Steering accepted by one endpoint must retain the complete execution
		// and delivery scope when it is deferred after a restart.
		{"steering_mailbox", "platform", "TEXT NOT NULL DEFAULT ''"},
		{"steering_mailbox", "platform_user_id", "TEXT NOT NULL DEFAULT ''"},
		{"steering_mailbox", "workspace_id", "TEXT NOT NULL DEFAULT ''"},
		{"steering_mailbox", "approval_mode", "TEXT NOT NULL DEFAULT ''"},
		// A remembered approval class used to be permanent and irrevocable:
		// the table only ever supported INSERT and an existence check, so a
		// grant recorded from one over-broad class key stayed authoritative
		// forever with no surface that could show or withdraw it. expires_at
		// bounds the blast radius of any future key defect; revoked_at makes
		// withdrawal durable and auditable. 0 keeps the legacy "no expiry"
		// meaning for rows written before this column existed.
		{"approval_grants", "expires_at", "INTEGER NOT NULL DEFAULT 0"},
		{"approval_grants", "revoked_at", "INTEGER NOT NULL DEFAULT 0"},
		// A lease binds a run to ONE in-process environment snapshot. Without
		// these columns a restarted daemon could not tell whether rebuilding
		// the snapshot preserves the environment the run started with, so it
		// would silently continue under a different PATH or account.
		{"execution_leases", "environment_snapshot_id", "TEXT NOT NULL DEFAULT ''"},
		{"execution_leases", "environment_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"execution_leases", "environment_fingerprint", "TEXT NOT NULL DEFAULT ''"},
		{"execution_leases", "credential_source_hash", "TEXT NOT NULL DEFAULT ''"},
		// A durable watch outlives its run and survives restarts, so it is the
		// execution path most likely to straddle an environment change. Without
		// its own recorded identity a watch registered under one account would
		// silently continue under whichever account the daemon happens to have
		// after a restart.
		// A watcher used to keep only the raw text of its last check, so the
		// worker re-derived a diagnosis from that string with its own marker
		// list. The typed class, a signature of the failure, and the streak
		// length make "the same environment failure again" a decidable fact and
		// let the watch stop instead of retrying until its deadline.
		{"external_watches", "failure_class", "TEXT NOT NULL DEFAULT ''"},
		{"external_watches", "spec_version", "INTEGER NOT NULL DEFAULT 1"},
		{"external_watches", "target_pattern", "TEXT NOT NULL DEFAULT ''"},
		{"external_watches", "terminal_success_pattern", "TEXT NOT NULL DEFAULT ''"},
		{"external_watches", "terminal_failure_pattern", "TEXT NOT NULL DEFAULT ''"},
		{"external_watches", "checker_status", "TEXT NOT NULL DEFAULT ''"},
		{"external_watches", "operation_status", "TEXT NOT NULL DEFAULT 'pending'"},
		{"external_watches", "verification_status", "TEXT NOT NULL DEFAULT 'not_required'"},
		{"external_watches", "current_interval_seconds", "INTEGER NOT NULL DEFAULT 0"},
		{"external_watches", "last_output_hash", "TEXT NOT NULL DEFAULT ''"},
		{"external_watches", "check_signature", "TEXT NOT NULL DEFAULT ''"},
		{"external_watches", "consecutive_failures", "INTEGER NOT NULL DEFAULT 0"},
		{"external_watches", "environment_snapshot_id", "TEXT NOT NULL DEFAULT ''"},
		{"external_watches", "environment_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"external_watches", "principal_fingerprint", "TEXT NOT NULL DEFAULT ''"},
		{"external_watches", "environment_fingerprint", "TEXT NOT NULL DEFAULT ''"},
		{"external_watches", "credential_source_hash", "TEXT NOT NULL DEFAULT ''"},
		// Durable execution stores one secret-free binding rather than
		// reconstructing environment and permissions from daemon-global state on
		// every poll. Legacy identity columns remain during the migration window.
		{"external_watches", "execution_binding_json", "TEXT NOT NULL DEFAULT '{}'"},
		// A finalization effect is complete only after its user-facing result is
		// in the durable outbound queue (or the local transcript is the target).
		{"effect_receipts", "delivery_enqueued", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := s.ensureColumn(ctx, col.table, col.name, col.def); err != nil {
			return err
		}
	}
	if err := s.migrateEffectReceiptsTenantScope(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_task_runs_work_key
		ON task_runs(tenant_id, task_id, work_key, status, started_at)`); err != nil {
		return err
	}
	// Existing workspaces predate an explicit trust act, so migration must not
	// promote every path ever observed by a client into trusted instructions.
	// They remain usable but require a one-time local-CLI review.
	if added, err := s.ensureColumnAdded(ctx, "workspaces", "trust_level", "TEXT NOT NULL DEFAULT 'untrusted'"); err != nil {
		return err
	} else {
		if err := s.ensureColumn(ctx, "workspaces", "trust_source", "TEXT"); err != nil {
			return err
		}
		if err := s.ensureColumn(ctx, "workspaces", "trusted_at", "INTEGER"); err != nil {
			return err
		}
		if added {
			if _, err := s.db.ExecContext(ctx, `UPDATE workspaces
				SET trust_level = ?, trust_source = 'migration_review_required', trusted_at = NULL
				WHERE status = 'active'`, executionenv.TrustUntrusted); err != nil {
				return err
			}
		}
		// Correct installations that briefly shipped the overly broad
		// migration_local promotion. Explicit local_cli trust is untouched.
		if _, err := s.db.ExecContext(ctx, `UPDATE workspaces
			SET trust_level = ?, trust_source = 'migration_review_required', trusted_at = NULL
			WHERE trust_source = 'migration_local'`, executionenv.TrustUntrusted); err != nil {
			return err
		}
	}
	// finalized marks that a terminal watch's completion side effects (task
	// update, event, notification, finalization run) actually ran. The
	// terminal-status CAS and the side effects are not one transaction, so
	// boot recovery compensates unfinalized terminal watches. Backfill runs
	// exactly once: watches already terminal before this column existed had
	// their side effects delivered by the old code path, and replaying them
	// would duplicate notifications.
	if added, err := s.ensureColumnAdded(ctx, "external_watches", "finalized", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	} else if added {
		if _, err := s.db.ExecContext(ctx, `UPDATE external_watches SET finalized = 1
			WHERE status IN (?, ?, ?, ?)`,
			ExternalWatchSucceeded, ExternalWatchFailed, ExternalWatchTimedOut, ExternalWatchCancelled); err != nil {
			return err
		}
	}
	// Notification delivery is a separate replay product. Existing terminal
	// watches predate that split and must not notify again after an upgrade;
	// only newly completed watches begin with notified=0.
	if added, err := s.ensureColumnAdded(ctx, "external_watches", "notified", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	} else if added {
		if _, err := s.db.ExecContext(ctx, `UPDATE external_watches SET notified = 1
			WHERE status IN (?, ?, ?, ?)`,
			ExternalWatchSucceeded, ExternalWatchFailed, ExternalWatchTimedOut, ExternalWatchCancelled); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE task_events SET cursor = rowid WHERE cursor IS NULL;
		INSERT OR IGNORE INTO event_sequence (id, next_cursor) VALUES (1, 0);
		UPDATE event_sequence
			SET next_cursor = MAX(next_cursor, COALESCE((SELECT MAX(cursor) FROM task_events), 0))
			WHERE id = 1;
		CREATE UNIQUE INDEX IF NOT EXISTS idx_task_events_cursor ON task_events(cursor);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_task_events_idempotency
			ON task_events(idempotency_key)
			WHERE idempotency_key IS NOT NULL AND idempotency_key != '';
		UPDATE tasks SET last_activity_at = updated_at WHERE last_activity_at IS NULL;
		CREATE INDEX IF NOT EXISTS idx_tasks_governance
			ON tasks(tenant_id, person_id, visibility, status, updated_at);
		CREATE INDEX IF NOT EXISTS idx_tasks_retention
			ON tasks(visibility, pinned, status, last_activity_at);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_inbox_unique
			ON tasks(tenant_id, person_id, COALESCE(workspace_id, ''))
			WHERE kind = 'inbox';
		DROP INDEX IF EXISTS idx_task_queue_idempotency;
		CREATE UNIQUE INDEX IF NOT EXISTS idx_task_queue_idempotency ON task_queue(tenant_id, idempotency_key)
			WHERE idempotency_key != '';
		UPDATE task_queue SET class = 'finalization', priority = 80
			WHERE idempotency_key LIKE 'external-watch:%' AND class = 'foreground';
		CREATE INDEX IF NOT EXISTS idx_task_queue_schedule
			ON task_queue(status, not_before, priority DESC, created_at);
		CREATE INDEX IF NOT EXISTS idx_task_queue_claims
			ON task_queue(status, lease_until);
		CREATE INDEX IF NOT EXISTS idx_approval_resume_authorization
			ON approval_requests(tenant_id, person_id, task_id, authorization_fingerprint, authorization_state);
		CREATE INDEX IF NOT EXISTS idx_maintenance_provider_calls_person
			ON maintenance_provider_calls(tenant_id, person_id, created_at);`); err != nil {
		return err
	}
	return nil
}

func (s *Store) migrateEffectReceiptsTenantScope(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(effect_receipts)`)
	if err != nil {
		return err
	}
	legacyPrimaryKey := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultValue, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		if name == "effect_key" && pk == 1 {
			legacyPrimaryKey = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !legacyPrimaryKey {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE effect_receipts_v2 (
			effect_key TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			task_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT '',
			delivery_enqueued INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, effect_key)
		);
		INSERT OR IGNORE INTO effect_receipts_v2
			(effect_key, tenant_id, task_id, run_id, kind, delivery_enqueued, created_at)
		SELECT effect_key, tenant_id, task_id, run_id, kind, delivery_enqueued, created_at
		FROM effect_receipts;
		DROP TABLE effect_receipts;
		ALTER TABLE effect_receipts_v2 RENAME TO effect_receipts;
		CREATE INDEX idx_effect_receipts_run ON effect_receipts(tenant_id, run_id);`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ensureColumn(ctx context.Context, table, name, definition string) error {
	_, err := s.ensureColumnAdded(ctx, table, name, definition)
	return err
}

// ensureColumnAdded reports whether it just created the column, so one-time
// backfills can run exactly on the upgrade that introduced the column.
func (s *Store) ensureColumnAdded(ctx context.Context, table, name, definition string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var colName, colType string
		var notNull int
		var defaultValue interface{}
		var pk int
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if colName == name {
			return false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, name, definition)); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) EnsureTenant(ctx context.Context, tenantID, name string) error {
	tenantID = normalizeTenant(tenantID)
	if strings.TrimSpace(name) == "" {
		name = tenantID
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tenants (id, name, mode, created_at)
		 VALUES (?, ?, 'personal', ?)
		 ON CONFLICT(id) DO NOTHING`,
		tenantID, name, now)
	return err
}

// ResolveAccount returns an existing platform binding without creating a
// tenant, person, or account. Read-only commands such as doctor must use this
// method so observing a store cannot mutate its identity graph.
func (s *Store) ResolveAccount(ctx context.Context, tenantID, platform, platformUserID string) (*IdentityContext, error) {
	tenantID = normalizeTenant(tenantID)
	platform = normalizeName(platform, "cli")
	platformUserID = normalizeName(platformUserID, "local")
	var out IdentityContext
	err := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, person_id, platform, platform_user_id, display_name
		FROM accounts
		WHERE tenant_id = ? AND platform = ? AND platform_user_id = ?`,
		tenantID, platform, platformUserID).
		Scan(&out.AccountID, &out.TenantID, &out.PersonID, &out.Platform, &out.PlatformUserID, &out.DisplayName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *Store) ResolveOrCreateAccount(ctx context.Context, tenantID, platform, platformUserID, displayName string) (*IdentityContext, error) {
	tenantID = normalizeTenant(tenantID)
	platform = normalizeName(platform, "cli")
	platformUserID = normalizeName(platformUserID, "local")
	if err := s.EnsureTenant(ctx, tenantID, tenantID); err != nil {
		return nil, err
	}

	if out, err := s.ResolveAccount(ctx, tenantID, platform, platformUserID); err != nil {
		return nil, err
	} else if out != nil {
		return out, nil
	}

	now := time.Now().Unix()
	personID := "person_" + uuid.NewString()
	accountID := "acct_" + uuid.NewString()
	if strings.TrimSpace(displayName) == "" {
		displayName = platform + ":" + platformUserID
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO persons (id, tenant_id, display_name, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		personID, tenantID, displayName, now, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO accounts (id, tenant_id, person_id, platform, platform_user_id, display_name, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)`,
		accountID, tenantID, personID, platform, platformUserID, displayName, now, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &IdentityContext{
		TenantID:       tenantID,
		PersonID:       personID,
		AccountID:      accountID,
		Platform:       platform,
		PlatformUserID: platformUserID,
		DisplayName:    displayName,
	}, nil
}

func (s *Store) BindAccount(ctx context.Context, tenantID, personID, platform, platformUserID, displayName string) (*IdentityContext, error) {
	tenantID = normalizeTenant(tenantID)
	platform = normalizeName(platform, "cli")
	platformUserID = normalizeName(platformUserID, "local")
	if personID == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if err := s.EnsureTenant(ctx, tenantID, tenantID); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	accountID := "acct_" + uuid.NewString()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO accounts (id, tenant_id, person_id, platform, platform_user_id, display_name, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)
		 ON CONFLICT(tenant_id, platform, platform_user_id) DO UPDATE SET
		   person_id = excluded.person_id,
		   display_name = excluded.display_name,
		   updated_at = excluded.updated_at`,
		accountID, tenantID, personID, platform, platformUserID, displayName, now, now)
	if err != nil {
		return nil, err
	}
	return s.ResolveOrCreateAccount(ctx, tenantID, platform, platformUserID, displayName)
}

// Account is one platform binding row for a person. The gateway uses it to fan
// notifications (e.g. approval requests raised from the CLI) out to the
// person's other bound endpoints; adapters never enumerate accounts themselves.
type Account struct {
	ID             string `json:"id"`
	TenantID       string `json:"tenant_id"`
	PersonID       string `json:"person_id"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id"`
	DisplayName    string `json:"display_name,omitempty"`
	Status         string `json:"status"`
	// LastSeenAt is the unix second of the account's most recent activity
	// beat (inbound message or presence touch); 0 means never seen.
	LastSeenAt int64 `json:"last_seen_at,omitempty"`
}

// ListAccountsByPerson returns the person's active platform bindings in bind
// order. Only 'active' rows are returned so an unbound endpoint stops
// receiving cross-channel notifications immediately.
func (s *Store) ListAccountsByPerson(ctx context.Context, tenantID, personID string) ([]Account, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, platform, platform_user_id, COALESCE(display_name, ''), status, COALESCE(last_seen_at, 0)
		 FROM accounts
		 WHERE tenant_id = ? AND person_id = ? AND status = 'active'
		 ORDER BY created_at ASC, id ASC`,
		normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.TenantID, &a.PersonID, &a.Platform, &a.PlatformUserID, &a.DisplayName, &a.Status, &a.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TouchAccountLastSeen stamps the account's durable recency beat. Callers are
// expected to throttle (the gateway's presence registry only asks for a write
// when the last one is >60s stale) — this must never become a per-request
// write on the hot path.
func (s *Store) TouchAccountLastSeen(ctx context.Context, tenantID, accountID string) error {
	if strings.TrimSpace(accountID) == "" {
		return fmt.Errorf("account id is required")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET last_seen_at = ? WHERE tenant_id = ? AND id = ?`,
		time.Now().Unix(), normalizeTenant(tenantID), accountID)
	return err
}

// MostRecentIMAccount returns the person's most recently seen active non-cli
// account whose platform the supplied filter accepts (the gateway passes the
// delivery service's SupportsPlatform, as a func so control stays free of a
// delivery import). Never-seen accounts rank after seen ones, in bind order,
// so a fresh install still resolves deterministically. Returns nil, nil when
// no bound IM account qualifies.
// AccountForChannel resolves the account whose platform_user_id equals the
// given channel string (IM channels use the platform user id as the durable
// channel key). Returns nil when the channel does not map to a bound account
// (e.g. CLI session channels).
func (s *Store) AccountForChannel(ctx context.Context, tenantID, personID, channel string) (*Account, error) {
	channel = strings.TrimSpace(channel)
	if channel == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, platform, platform_user_id,
		COALESCE(display_name, ''), status, COALESCE(last_seen_at, 0)
		FROM accounts WHERE tenant_id = ? AND person_id = ? AND platform_user_id = ? AND status = 'active' LIMIT 1`,
		normalizeTenant(tenantID), personID, channel)
	var a Account
	if err := row.Scan(&a.ID, &a.TenantID, &a.PersonID, &a.Platform, &a.PlatformUserID, &a.DisplayName, &a.Status, &a.LastSeenAt); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func (s *Store) MostRecentIMAccount(ctx context.Context, tenantID, personID string, supported func(platform string) bool) (*Account, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, platform, platform_user_id, COALESCE(display_name, ''), status, COALESCE(last_seen_at, 0)
		 FROM accounts
		 WHERE tenant_id = ? AND person_id = ? AND status = 'active' AND platform != 'cli'
		 ORDER BY COALESCE(last_seen_at, 0) DESC, created_at ASC, rowid ASC`,
		normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.TenantID, &a.PersonID, &a.Platform, &a.PlatformUserID, &a.DisplayName, &a.Status, &a.LastSeenAt); err != nil {
			return nil, err
		}
		if supported == nil || supported(a.Platform) {
			return &a, nil
		}
	}
	return nil, rows.Err()
}

// SetPersonSetting stores a small per-person key-value preference (e.g. the
// /notify endpoint choice). An empty value deletes the row, which is how a
// preference resets to its default.
func (s *Store) SetPersonSetting(ctx context.Context, tenantID, personID, key, value string) error {
	if strings.TrimSpace(personID) == "" || strings.TrimSpace(key) == "" {
		return fmt.Errorf("person id and key are required")
	}
	tenantID = normalizeTenant(tenantID)
	if strings.TrimSpace(value) == "" {
		_, err := s.db.ExecContext(ctx,
			`DELETE FROM person_settings WHERE tenant_id = ? AND person_id = ? AND key = ?`,
			tenantID, personID, key)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO person_settings (tenant_id, person_id, key, value, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, person_id, key) DO UPDATE SET
		   value = excluded.value,
		   updated_at = excluded.updated_at`,
		tenantID, personID, key, value, time.Now().Unix())
	return err
}

// GetPersonSetting returns the stored preference value, or "" when unset.
func (s *Store) GetPersonSetting(ctx context.Context, tenantID, personID, key string) (string, error) {
	if strings.TrimSpace(personID) == "" || strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("person id and key are required")
	}
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM person_settings WHERE tenant_id = ? AND person_id = ? AND key = ?`,
		normalizeTenant(tenantID), personID, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

func (s *Store) RegisterWorkspace(ctx context.Context, ws Workspace) (*Workspace, error) {
	return s.registerWorkspace(ctx, ws, true)
}

// EnsureWorkspace creates a workspace when it is first observed by a client,
// but preserves explicit configuration such as allowed roots on later visits.
// RegisterWorkspace remains the authoritative replace operation used by the
// workspace management API.
func (s *Store) EnsureWorkspace(ctx context.Context, ws Workspace) (*Workspace, error) {
	return s.registerWorkspace(ctx, ws, false)
}

func (s *Store) registerWorkspace(ctx context.Context, ws Workspace, replaceConfiguration bool) (*Workspace, error) {
	ws.TenantID = normalizeTenant(ws.TenantID)
	if ws.OwnerPersonID == "" {
		return nil, fmt.Errorf("owner person id is required")
	}
	if ws.LocalPath == "" {
		return nil, fmt.Errorf("local path is required")
	}
	if ws.Name == "" {
		ws.Name = filepath.Base(ws.LocalPath)
	}
	if ws.Status == "" {
		ws.Status = "active"
	}
	now := time.Now()
	if replaceConfiguration {
		ws.TrustLevel = executionenv.TrustTrusted
		ws.TrustSource = "local_cli"
		ws.TrustedAt = &now
	} else {
		ws.TrustLevel = executionenv.TrustUntrusted
	}
	if len(ws.AllowedRoots) == 0 {
		ws.AllowedRoots = []string{ws.LocalPath}
	}
	roots, _ := json.Marshal(ws.AllowedRoots)

	var existingID string
	var existingRoots string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, COALESCE(allowed_roots_json, '[]') FROM workspaces WHERE tenant_id = ? AND owner_person_id = ? AND local_path = ?`,
		ws.TenantID, ws.OwnerPersonID, ws.LocalPath).Scan(&existingID, &existingRoots)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if existingID == "" {
		existingID = "ws_" + uuid.NewString()
	} else if !replaceConfiguration {
		var preserved []string
		if json.Unmarshal([]byte(existingRoots), &preserved) == nil && len(preserved) > 0 {
			ws.AllowedRoots = preserved
			roots, _ = json.Marshal(ws.AllowedRoots)
		}
	}
	ws.ID = existingID
	trustedAt := interface{}(nil)
	if ws.TrustedAt != nil {
		trustedAt = ws.TrustedAt.Unix()
	}
	upsert := `INSERT INTO workspaces
		   (id, tenant_id, owner_person_id, name, repo_url, local_path, default_branch, allowed_roots_json, status,
		    trust_level, trust_source, trusted_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, owner_person_id, local_path) DO UPDATE SET
		   name = excluded.name,
		   repo_url = excluded.repo_url,
		   default_branch = excluded.default_branch,
		   allowed_roots_json = excluded.allowed_roots_json,
		   status = excluded.status,
		   updated_at = excluded.updated_at`
	if replaceConfiguration {
		upsert += `,
		   trust_level = excluded.trust_level,
		   trust_source = excluded.trust_source,
		   trusted_at = excluded.trusted_at`
	}
	_, err = s.db.ExecContext(ctx, upsert,
		ws.ID, ws.TenantID, ws.OwnerPersonID, ws.Name, ws.RepoURL, ws.LocalPath, ws.DefaultBranch, string(roots), ws.Status,
		ws.TrustLevel, ws.TrustSource, trustedAt, now.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	if err := s.SetCurrentWorkspace(ctx, ws.TenantID, ws.OwnerPersonID, ws.ID); err != nil {
		return nil, err
	}
	return s.GetWorkspace(ctx, ws.TenantID, ws.ID)
}

func (s *Store) SetCurrentWorkspace(ctx context.Context, tenantID, personID, workspaceID string) error {
	if workspaceID == "" {
		return fmt.Errorf("workspace id is required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO current_workspace (tenant_id, person_id, workspace_id, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(tenant_id, person_id) DO UPDATE SET
		   workspace_id = excluded.workspace_id,
		   updated_at = excluded.updated_at`,
		normalizeTenant(tenantID), personID, workspaceID, time.Now().Unix())
	return err
}

func (s *Store) CurrentWorkspace(ctx context.Context, tenantID, personID string) (*Workspace, error) {
	var workspaceID string
	err := s.db.QueryRowContext(ctx,
		`SELECT workspace_id FROM current_workspace WHERE tenant_id = ? AND person_id = ?`,
		normalizeTenant(tenantID), personID).Scan(&workspaceID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetWorkspace(ctx, tenantID, workspaceID)
}

func (s *Store) GetWorkspace(ctx context.Context, tenantID, workspaceID string) (*Workspace, error) {
	var ws Workspace
	var roots string
	var trustedAt sql.NullInt64
	var created, updated int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, owner_person_id, name, COALESCE(repo_url, ''), local_path,
		        COALESCE(default_branch, ''), COALESCE(allowed_roots_json, '[]'),
		        status, COALESCE(trust_level, 'untrusted'), COALESCE(trust_source, ''), trusted_at,
		        created_at, updated_at
		 FROM workspaces WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), workspaceID).
		Scan(&ws.ID, &ws.TenantID, &ws.OwnerPersonID, &ws.Name, &ws.RepoURL, &ws.LocalPath, &ws.DefaultBranch,
			&roots, &ws.Status, &ws.TrustLevel, &ws.TrustSource, &trustedAt, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(roots), &ws.AllowedRoots)
	if trustedAt.Valid {
		value := time.Unix(trustedAt.Int64, 0)
		ws.TrustedAt = &value
	}
	ws.CreatedAt = time.Unix(created, 0)
	ws.UpdatedAt = time.Unix(updated, 0)
	return &ws, nil
}

func (s *Store) ListWorkspaces(ctx context.Context, tenantID, personID string) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, owner_person_id, name, COALESCE(repo_url, ''), local_path,
		        COALESCE(default_branch, ''), COALESCE(allowed_roots_json, '[]'),
		        status, COALESCE(trust_level, 'untrusted'), COALESCE(trust_source, ''), trusted_at,
		        created_at, updated_at
		 FROM workspaces WHERE tenant_id = ? AND owner_person_id = ? ORDER BY updated_at DESC`,
		normalizeTenant(tenantID), personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var ws Workspace
		var roots string
		var trustedAt sql.NullInt64
		var created, updated int64
		if err := rows.Scan(&ws.ID, &ws.TenantID, &ws.OwnerPersonID, &ws.Name, &ws.RepoURL, &ws.LocalPath,
			&ws.DefaultBranch, &roots, &ws.Status, &ws.TrustLevel, &ws.TrustSource, &trustedAt, &created, &updated); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(roots), &ws.AllowedRoots)
		if trustedAt.Valid {
			value := time.Unix(trustedAt.Int64, 0)
			ws.TrustedAt = &value
		}
		ws.CreatedAt = time.Unix(created, 0)
		ws.UpdatedAt = time.Unix(updated, 0)
		out = append(out, ws)
	}
	return out, rows.Err()
}

func (s *Store) SetWorkspaceTrust(ctx context.Context, tenantID, personID, workspaceID, trustLevel, source string) (*Workspace, error) {
	trustLevel = strings.ToLower(strings.TrimSpace(trustLevel))
	if trustLevel != executionenv.TrustTrusted && trustLevel != executionenv.TrustUntrusted {
		return nil, fmt.Errorf("invalid workspace trust level %q", trustLevel)
	}
	var trustedAt interface{}
	if trustLevel == executionenv.TrustTrusted {
		trustedAt = time.Now().Unix()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE workspaces
		SET trust_level = ?, trust_source = ?, trusted_at = ?, updated_at = ?
		WHERE tenant_id = ? AND owner_person_id = ? AND id = ?`,
		trustLevel, strings.TrimSpace(source), trustedAt, time.Now().Unix(),
		normalizeTenant(tenantID), personID, workspaceID)
	if err != nil {
		return nil, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return nil, fmt.Errorf("workspace not found")
	}
	if trustLevel == executionenv.TrustUntrusted {
		if _, err := tx.ExecContext(ctx, `UPDATE execution_capability_grants SET revoked_at = ?, updated_at = ?
			WHERE tenant_id = ? AND person_id = ? AND workspace_id = ? AND revoked_at IS NULL`,
			time.Now().Unix(), time.Now().Unix(), normalizeTenant(tenantID), personID, workspaceID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetWorkspace(ctx, tenantID, workspaceID)
}

func (s *Store) CreateTask(ctx context.Context, req TaskCreate) (*Task, error) {
	req.TenantID = normalizeTenant(req.TenantID)
	if req.PersonID == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if strings.TrimSpace(req.Title) == "" {
		req.Title = "Untitled task"
	}
	now := time.Now().Unix()
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = "task_" + uuid.NewString()
	} else if !strings.HasPrefix(id, "task_") {
		return nil, fmt.Errorf("task id must start with task_")
	}
	kind := normalizeTaskKind(req.Kind)
	visibility := normalizeTaskVisibility(req.Visibility)
	pinned := 0
	if req.Pinned {
		pinned = 1
	}
	// A freshly created task is NOT running — a run sets status='running' with
	// an active_run_id via StartRun. Hardcoding 'running' here made /new-created
	// (and any not-yet-run) tasks look running with no run behind them, so the
	// stuck-run sweeper then flipped them to 'interrupted' (observed live: a
	// brand-new empty task showing [interrupted]). Start as 'new' — non-terminal
	// (resolveContinueTask still offers it, the sweeper ignores it) and honest.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, tenant_id, person_id, workspace_id, title, status, kind, visibility,
		 pinned, last_channel, last_activity_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 'new', ?, ?, ?, ?, ?, ?, ?)`,
		id, req.TenantID, req.PersonID, req.WorkspaceID, req.Title, kind, visibility,
		pinned, req.Channel, now, now, now)
	if err != nil {
		return nil, err
	}
	if !req.KeepCurrent {
		if err := s.SetCurrentTask(ctx, req.TenantID, req.PersonID, id); err != nil {
			return nil, err
		}
	}
	return s.GetTask(ctx, req.TenantID, id)
}

func (s *Store) SetCurrentTask(ctx context.Context, tenantID, personID, taskID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO current_task (tenant_id, person_id, task_id, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(tenant_id, person_id) DO UPDATE SET
		   task_id = excluded.task_id,
		   updated_at = excluded.updated_at`,
		normalizeTenant(tenantID), personID, taskID, time.Now().Unix())
	return err
}

func (s *Store) CurrentTask(ctx context.Context, tenantID, personID string) (*Task, error) {
	var taskID string
	err := s.db.QueryRowContext(ctx,
		`SELECT task_id FROM current_task WHERE tenant_id = ? AND person_id = ?`,
		normalizeTenant(tenantID), personID).Scan(&taskID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetTask(ctx, tenantID, taskID)
}

// NOTE: CurrentTaskForChannel (most recent non-terminal task per channel) was
// removed 2026-07-05 with the task-attach fix: ordinary new messages no longer
// silently attach to a parked task, so the gateway resolves attach targets
// only via explicit evidence (req.TaskID, IntentContinue, the /resume pin).
// See internal/gateway/httpapi resolveTask.

func (s *Store) GetTask(ctx context.Context, tenantID, taskID string) (*Task, error) {
	var t Task
	var nextSteps string
	var pinned int
	var archived sql.NullInt64
	var created, updated, lastActivity int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, person_id, COALESCE(workspace_id, ''), title, status,
		        COALESCE(kind, 'work'), COALESCE(visibility, 'visible'), COALESCE(pinned, 0),
		        COALESCE(current_summary, ''), COALESCE(next_steps_json, '[]'),
		        COALESCE(blocked_reason, ''), COALESCE(active_run_id, ''),
		        COALESCE(last_channel, ''), archived_at,
		        COALESCE(last_activity_at, updated_at), created_at, updated_at
		 FROM tasks WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), taskID).
		Scan(&t.ID, &t.TenantID, &t.PersonID, &t.WorkspaceID, &t.Title, &t.Status,
			&t.Kind, &t.Visibility, &pinned, &t.CurrentSummary, &nextSteps, &t.BlockedReason,
			&t.ActiveRunID, &t.LastChannel, &archived, &lastActivity, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(nextSteps), &t.NextSteps)
	t.Pinned = pinned != 0
	if archived.Valid {
		at := time.Unix(archived.Int64, 0)
		t.ArchivedAt = &at
	}
	t.LastActivityAt = time.Unix(lastActivity, 0)
	t.CreatedAt = time.Unix(created, 0)
	t.UpdatedAt = time.Unix(updated, 0)
	return &t, nil
}

func (s *Store) SetTaskWorkspace(ctx context.Context, tenantID, taskID, workspaceID string) error {
	if strings.TrimSpace(taskID) == "" {
		return fmt.Errorf("task id is required")
	}
	if strings.TrimSpace(workspaceID) == "" {
		return fmt.Errorf("workspace id is required")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks
		 SET workspace_id = ?, updated_at = ?
		 WHERE tenant_id = ? AND id = ? AND COALESCE(workspace_id, '') = ''`,
		workspaceID, time.Now().Unix(), normalizeTenant(tenantID), taskID)
	return err
}

func (s *Store) ListTasks(ctx context.Context, tenantID, personID string, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, person_id, COALESCE(workspace_id, ''), title, status,
		        COALESCE(kind, 'work'), COALESCE(visibility, 'visible'), COALESCE(pinned, 0),
		        COALESCE(current_summary, ''), COALESCE(next_steps_json, '[]'),
		        COALESCE(blocked_reason, ''), COALESCE(active_run_id, ''),
		        COALESCE(last_channel, ''), archived_at,
		        COALESCE(last_activity_at, updated_at), created_at, updated_at
		 FROM tasks WHERE tenant_id = ? AND person_id = ?
		   AND COALESCE(visibility, 'visible') != 'hidden'
		 ORDER BY updated_at DESC LIMIT ?`,
		normalizeTenant(tenantID), personID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var nextSteps string
		var pinned int
		var archived sql.NullInt64
		var created, updated, lastActivity int64
		if err := rows.Scan(&t.ID, &t.TenantID, &t.PersonID, &t.WorkspaceID, &t.Title, &t.Status,
			&t.Kind, &t.Visibility, &pinned, &t.CurrentSummary, &nextSteps, &t.BlockedReason,
			&t.ActiveRunID, &t.LastChannel, &archived, &lastActivity, &created, &updated); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(nextSteps), &t.NextSteps)
		t.Pinned = pinned != 0
		if archived.Valid {
			at := time.Unix(archived.Int64, 0)
			t.ArchivedAt = &at
		}
		t.LastActivityAt = time.Unix(lastActivity, 0)
		t.CreatedAt = time.Unix(created, 0)
		t.UpdatedAt = time.Unix(updated, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTasksByStatusSince returns the person's tasks whose status is one of
// statuses and whose last update is at or after since, newest first, bounded
// by limit. It backs the attach digest (G0-c): "which tasks finished or
// stopped early while this endpoint was away". It filters on existing columns
// only (tenant_id, person_id, status, updated_at), so it stays a cheap scan of
// the person's task list.
func (s *Store) ListTasksByStatusSince(ctx context.Context, tenantID, personID string, statuses []string, since time.Time, limit int) ([]Task, error) {
	if strings.TrimSpace(personID) == "" {
		return nil, fmt.Errorf("person id is required")
	}
	if len(statuses) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	query := `SELECT id, tenant_id, person_id, COALESCE(workspace_id, ''), title, status,
	                 COALESCE(kind, 'work'), COALESCE(visibility, 'visible'), COALESCE(pinned, 0),
	                 COALESCE(current_summary, ''), COALESCE(next_steps_json, '[]'),
	                 COALESCE(blocked_reason, ''), COALESCE(active_run_id, ''),
	                 COALESCE(last_channel, ''), archived_at,
	                 COALESCE(last_activity_at, updated_at), created_at, updated_at
	          FROM tasks
	          WHERE tenant_id = ? AND person_id = ? AND updated_at >= ?
	            AND COALESCE(visibility, 'visible') != 'hidden'
	            AND status IN (` + placeholders(len(statuses)) + `)
	          ORDER BY updated_at DESC LIMIT ?`
	args := []any{normalizeTenant(tenantID), personID, since.Unix()}
	args = append(args, toAnySlice(statuses)...)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var nextSteps string
		var pinned int
		var archived sql.NullInt64
		var created, updated, lastActivity int64
		if err := rows.Scan(&t.ID, &t.TenantID, &t.PersonID, &t.WorkspaceID, &t.Title, &t.Status,
			&t.Kind, &t.Visibility, &pinned, &t.CurrentSummary, &nextSteps, &t.BlockedReason,
			&t.ActiveRunID, &t.LastChannel, &archived, &lastActivity, &created, &updated); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(nextSteps), &t.NextSteps)
		t.Pinned = pinned != 0
		if archived.Valid {
			at := time.Unix(archived.Int64, 0)
			t.ArchivedAt = &at
		}
		t.LastActivityAt = time.Unix(lastActivity, 0)
		t.CreatedAt = time.Unix(created, 0)
		t.UpdatedAt = time.Unix(updated, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) UpdateTaskStatus(ctx context.Context, tenantID, taskID, status, summary string, nextSteps []string) error {
	nextStepsJSON, _ := json.Marshal(nextSteps)
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, current_summary = COALESCE(NULLIF(?, ''), current_summary),
		 next_steps_json = ?, archived_at = CASE
		   WHEN ? = 'archived' THEN COALESCE(archived_at, ?)
		   ELSE NULL
		 END, last_activity_at = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
		status, summary, string(nextStepsJSON), status, now, now, now, normalizeTenant(tenantID), taskID)
	return err
}

func (s *Store) StartRun(ctx context.Context, task *Task, channel, inputSummary string) (*Run, error) {
	return s.startRun(ctx, task, channel, inputSummary, StartRunOptions{})
}

type StartRunOptions struct {
	WorkKey               string
	PreserveTaskLifecycle bool
}

// StartRunWithWorkKey atomically creates a run with its deterministic work
// identity. Keeping the key in the same transaction as the run prevents a
// daemon crash between admission and a follow-up UPDATE from turning an
// explicit continuation into an ambiguous one.
func (s *Store) StartRunWithWorkKey(ctx context.Context, task *Task, channel, inputSummary, workKey string) (*Run, error) {
	return s.startRun(ctx, task, channel, inputSummary, StartRunOptions{WorkKey: workKey})
}

// StartRunWithOptions is used when ingress attached a run to a display-only
// weak label. The run is durable and active, but the guessed task lifecycle is
// left untouched until deterministic or post-run label resolution.
func (s *Store) StartRunWithOptions(ctx context.Context, task *Task, channel, inputSummary string, options StartRunOptions) (*Run, error) {
	return s.startRun(ctx, task, channel, inputSummary, options)
}

func (s *Store) startRun(ctx context.Context, task *Task, channel, inputSummary string, options StartRunOptions) (*Run, error) {
	if task == nil {
		return nil, fmt.Errorf("task is required")
	}
	run := &Run{
		ID:           "run_" + uuid.NewString(),
		TaskID:       task.ID,
		TenantID:     task.TenantID,
		PersonID:     task.PersonID,
		WorkspaceID:  task.WorkspaceID,
		Channel:      normalizeName(channel, "cli"),
		InputSummary: inputSummary,
		WorkKey:      strings.ToUpper(strings.TrimSpace(options.WorkKey)),
		Status:       "running",
		StartedAt:    time.Now(),
	}
	run.WorkUnitID = "wu_" + uuid.NewString()
	// Insert the run and flip the task to running atomically: a partial write
	// would leave tasks and task_runs disagreeing about the active run.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO task_runs (id, task_id, tenant_id, person_id, workspace_id, channel, input_summary, work_key, status, started_at, heartbeat_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.TaskID, run.TenantID, run.PersonID, run.WorkspaceID, run.Channel, run.InputSummary, run.WorkKey, run.Status, run.StartedAt.Unix(), run.StartedAt.Unix()); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO run_work_units
			 (id, identity_tenant_id, person_id, workspace_id, run_id, sequence, primary_task_id,
			  related_task_id, goal_digest, plan_status, status, started_at, created_at, started_cursor)
			 VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, 'in_progress', 'active', ?, ?, 0)`,
		run.WorkUnitID, run.TenantID, run.PersonID, run.WorkspaceID, run.ID, run.TaskID,
		run.TaskID, run.InputSummary, run.StartedAt.Unix(), run.StartedAt.Unix()); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE tasks SET active_run_id = ?, status = CASE WHEN ? THEN status ELSE 'running' END, last_channel = ?,
		 archived_at = NULL, last_activity_at = ?, updated_at = ? WHERE id = ?`,
		run.ID, options.PreserveTaskLifecycle, run.Channel, time.Now().Unix(), time.Now().Unix(), run.TaskID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return run, nil
}

// PriorRunChannel returns the channel of the task's most recent run other than
// exceptRunID (typically the current run). It backs the backward-compat read of
// working-context history: a task whose transcript was stored channel-keyed
// before history became task-keyed can still be loaded under this channel. It is
// a bounded, read-only lookup; an empty result (no prior run) is not an error.
func (s *Store) PriorRunChannel(ctx context.Context, tenantID, taskID, exceptRunID string) (string, error) {
	if strings.TrimSpace(taskID) == "" {
		return "", nil
	}
	var channel string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(channel, '') FROM task_runs
		 WHERE tenant_id = ? AND task_id = ? AND id != ?
		 ORDER BY started_at DESC, id DESC LIMIT 1`,
		normalizeTenant(tenantID), taskID, exceptRunID).Scan(&channel)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return channel, nil
}

// GetRun reads one run by id. It is used by recovery/finalization paths that
// must reconstruct the same durable maintenance evidence as the normal run
// path after an asynchronous panic.
func (s *Store) GetRun(ctx context.Context, tenantID, runID string) (*Run, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, nil
	}
	var r Run
	var started int64
	var finished sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, task_id, tenant_id, person_id, COALESCE(workspace_id, ''), channel,
		        COALESCE(input_summary, ''), COALESCE(work_key, ''),
		        COALESCE((SELECT id FROM run_work_units WHERE run_id = task_runs.id ORDER BY sequence LIMIT 1), ''),
		        status, started_at, finished_at
		 FROM task_runs WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), runID).
		Scan(&r.ID, &r.TaskID, &r.TenantID, &r.PersonID, &r.WorkspaceID, &r.Channel,
			&r.InputSummary, &r.WorkKey, &r.WorkUnitID, &r.Status, &started, &finished)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.StartedAt = time.Unix(started, 0)
	if finished.Valid {
		at := time.Unix(finished.Int64, 0)
		r.FinishedAt = &at
	}
	return &r, nil
}

func (s *Store) FinishRun(ctx context.Context, tenantID, runID, status string) error {
	return s.finishRun(ctx, tenantID, runID, status, 0, "")
}

// FinishRunWithMaintenancePayload atomically records both the terminal run
// state and the immutable replay input consumed by post-run maintenance. The
// ordinary FinishRun wrapper is terminal-only for administrative and fallback
// callers that have no analyzer evidence.
func (s *Store) FinishRunWithMaintenancePayload(ctx context.Context, tenantID, runID, status string, analyzerVersion int, payload string) error {
	if analyzerVersion <= 0 {
		return fmt.Errorf("maintenance analyzer version must be positive")
	}
	return s.finishRun(ctx, tenantID, runID, status, analyzerVersion, payload)
}

func (s *Store) finishRun(ctx context.Context, tenantID, runID, status string, analyzerVersion int, payload string) error {
	// FinishRun's contract is to write a TERMINAL run status. Agent outcomes
	// can legitimately say "running" ("turn done, more work planned"), but a
	// finished run row left in status 'running' — with active_run_id cleared
	// below — is exactly the phantom state that recovery sweeps could no
	// longer attribute to a task, leaving tasks '[running]' forever. Coerce
	// non-terminal statuses to 'done'; the caller maps the task-level status
	// separately.
	if status == "" || status == "running" {
		status = "done"
	}
	now := time.Now().Unix()
	tenant := normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx,
		`UPDATE task_runs SET status = ?, finished_at = ?, heartbeat_at = ? WHERE tenant_id = ? AND id = ?`,
		status, now, now, tenant, runID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE tasks SET active_run_id = '', last_activity_at = ?, updated_at = ?
		 WHERE tenant_id = ? AND active_run_id = ?`,
		now, now, tenant, runID); err != nil {
		return err
	}
	// Eligible gateway finalization creates the maintenance job in this same
	// transaction. Terminal-only callers intentionally leave no empty job.
	if analyzerVersion > 0 {
		if err = createMaintenanceJobTx(ctx, tx, tenant, runID, analyzerVersion, payload); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) AppendEvent(ctx context.Context, event Event) (*Event, error) {
	if event.TaskID == "" {
		return nil, fmt.Errorf("task id is required")
	}
	if event.Type == "" {
		event.Type = "note"
	}
	if event.Visibility == "" {
		event.Visibility = "task"
	}
	event.IdempotencyKey = strings.TrimSpace(event.IdempotencyKey)
	if event.IdempotencyKey != "" {
		existing, err := s.eventByIdempotencyKey(ctx, event.IdempotencyKey)
		if err == nil {
			return existing, nil
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
	}
	event.ID = "event_" + uuid.NewString()
	event.CreatedAt = time.Now()
	if err := s.db.QueryRowContext(ctx,
		`SELECT tenant_id, person_id FROM tasks WHERE id = ?`, event.TaskID,
	).Scan(&event.TenantID, &event.PersonID); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx,
		`UPDATE event_sequence SET next_cursor = next_cursor + 1 WHERE id = 1 RETURNING next_cursor`,
	).Scan(&event.Cursor); err != nil {
		return nil, err
	}
	var idempotencyKey interface{}
	if event.IdempotencyKey != "" {
		idempotencyKey = event.IdempotencyKey
	}
	result, err := tx.ExecContext(ctx,
		`INSERT INTO task_events (id, cursor, task_id, run_id, type, visibility, channel, payload_json, idempotency_key, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '' DO NOTHING`,
		event.ID, event.Cursor, event.TaskID, event.RunID, event.Type, event.Visibility, event.Channel, string(event.Payload), idempotencyKey, event.CreatedAt.Unix())
	if err != nil {
		return nil, err
	}
	if inserted, err := result.RowsAffected(); err != nil {
		return nil, err
	} else if inserted == 0 {
		// Roll back the cursor increment before returning the concurrently
		// inserted logical event.
		if err := tx.Rollback(); err != nil {
			return nil, err
		}
		existing, err := s.eventByIdempotencyKey(ctx, event.IdempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("load duplicate event %q: %w", event.IdempotencyKey, err)
		}
		return existing, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if s.events != nil {
		s.events.publish(event)
	}
	return &event, nil
}

func (s *Store) eventByIdempotencyKey(ctx context.Context, key string) (*Event, error) {
	var event Event
	var payload string
	var createdAt int64
	err := s.db.QueryRowContext(ctx, `SELECT e.id, e.cursor, t.tenant_id, t.person_id,
		e.task_id, COALESCE(e.run_id, ''), e.type, e.visibility,
		COALESCE(e.channel, ''), COALESCE(e.payload_json, ''),
		COALESCE(e.idempotency_key, ''), e.created_at
		FROM task_events e JOIN tasks t ON t.id = e.task_id
		WHERE e.idempotency_key = ?`, key).Scan(
		&event.ID, &event.Cursor, &event.TenantID, &event.PersonID,
		&event.TaskID, &event.RunID, &event.Type, &event.Visibility,
		&event.Channel, &payload, &event.IdempotencyKey, &createdAt)
	if err != nil {
		return nil, err
	}
	event.Payload = json.RawMessage(payload)
	event.CreatedAt = time.Unix(createdAt, 0)
	return &event, nil
}

func (s *Store) RecordChannelMessage(ctx context.Context, identity IdentityContext, channel, taskID, role, content string) error {
	if role == "" {
		role = "user"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO channel_messages (id, tenant_id, person_id, account_id, channel, task_id, role, content, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"msg_"+uuid.NewString(), normalizeTenant(identity.TenantID), identity.PersonID, identity.AccountID,
		normalizeName(channel, identity.Platform), taskID, role, content, time.Now().Unix())
	return err
}

func (s *Store) SaveHandoff(ctx context.Context, handoff Handoff) (*Handoff, error) {
	if handoff.TaskID == "" {
		return nil, fmt.Errorf("task id is required")
	}
	handoff.ID = "handoff_" + uuid.NewString()
	handoff.CreatedAt = time.Now()
	doneJSON, _ := json.Marshal(handoff.DoneItems)
	nextJSON, _ := json.Marshal(handoff.NextSteps)
	filesJSON, _ := json.Marshal(handoff.ChangedFiles)
	risksJSON, _ := json.Marshal(handoff.Risks)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO task_handoffs (id, task_id, summary, done_items_json, next_steps_json, changed_files_json, test_status, risks_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		handoff.ID, handoff.TaskID, handoff.Summary, string(doneJSON), string(nextJSON), string(filesJSON), handoff.TestStatus, string(risksJSON), handoff.CreatedAt.Unix())
	if err != nil {
		return nil, err
	}
	return &handoff, nil
}

func (s *Store) LatestHandoff(ctx context.Context, taskID string) (*Handoff, error) {
	var h Handoff
	var doneJSON, nextJSON, filesJSON, risksJSON string
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, task_id, summary, done_items_json, next_steps_json, changed_files_json, test_status, risks_json, created_at
		 FROM task_handoffs WHERE task_id = ? ORDER BY created_at DESC LIMIT 1`,
		taskID).
		Scan(&h.ID, &h.TaskID, &h.Summary, &doneJSON, &nextJSON, &filesJSON, &h.TestStatus, &risksJSON, &created)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(doneJSON), &h.DoneItems)
	_ = json.Unmarshal([]byte(nextJSON), &h.NextSteps)
	_ = json.Unmarshal([]byte(filesJSON), &h.ChangedFiles)
	_ = json.Unmarshal([]byte(risksJSON), &h.Risks)
	h.CreatedAt = time.Unix(created, 0)
	return &h, nil
}

func normalizeTenant(tenantID string) string {
	return normalizeName(tenantID, DefaultTenantID)
}

func normalizeName(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
