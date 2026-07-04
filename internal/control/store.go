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
)

const DefaultTenantID = "default"

type Store struct {
	db *sql.DB
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
	ID            string    `json:"id"`
	TenantID      string    `json:"tenant_id"`
	OwnerPersonID string    `json:"owner_person_id"`
	Name          string    `json:"name"`
	RepoURL       string    `json:"repo_url,omitempty"`
	LocalPath     string    `json:"local_path"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	AllowedRoots  []string  `json:"allowed_roots,omitempty"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Task struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	PersonID       string    `json:"person_id"`
	WorkspaceID    string    `json:"workspace_id,omitempty"`
	Title          string    `json:"title"`
	Status         string    `json:"status"`
	CurrentSummary string    `json:"current_summary,omitempty"`
	NextSteps      []string  `json:"next_steps,omitempty"`
	BlockedReason  string    `json:"blocked_reason,omitempty"`
	ActiveRunID    string    `json:"active_run_id,omitempty"`
	LastChannel    string    `json:"last_channel,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Run struct {
	ID           string     `json:"id"`
	TaskID       string     `json:"task_id"`
	TenantID     string     `json:"tenant_id"`
	PersonID     string     `json:"person_id"`
	WorkspaceID  string     `json:"workspace_id,omitempty"`
	Channel      string     `json:"channel"`
	InputSummary string     `json:"input_summary,omitempty"`
	Status       string     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

type Event struct {
	ID         string          `json:"id"`
	TaskID     string          `json:"task_id"`
	RunID      string          `json:"run_id,omitempty"`
	Type       string          `json:"type"`
	Visibility string          `json:"visibility"`
	Channel    string          `json:"channel,omitempty"`
	Payload    json.RawMessage `json:"payload_json,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
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
	TenantID    string
	PersonID    string
	WorkspaceID string
	Title       string
	Channel     string
}

func OpenStore(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("data dir is required")
	}
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "control.db")+"?_journal=WAL&_sync=NORMAL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.InitSchema(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
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
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	tenant_id TEXT NOT NULL,
	person_id TEXT NOT NULL,
	workspace_id TEXT,
	title TEXT NOT NULL,
	status TEXT NOT NULL,
	current_summary TEXT,
	next_steps_json TEXT,
	blocked_reason TEXT,
	active_run_id TEXT,
	last_channel TEXT,
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
	last_error TEXT
);
CREATE TABLE IF NOT EXISTS task_events (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL,
	run_id TEXT,
	type TEXT NOT NULL,
	visibility TEXT NOT NULL DEFAULT 'task',
	channel TEXT,
	payload_json TEXT,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_task_events_task ON task_events(task_id, created_at);
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
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
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
);`
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
	} {
		if err := s.ensureColumn(ctx, col.table, col.name, col.def); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, name, definition string) error {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var colName, colType string
		var notNull int
		var defaultValue interface{}
		var pk int
		if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if colName == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, name, definition))
	return err
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

func (s *Store) ResolveOrCreateAccount(ctx context.Context, tenantID, platform, platformUserID, displayName string) (*IdentityContext, error) {
	tenantID = normalizeTenant(tenantID)
	platform = normalizeName(platform, "cli")
	platformUserID = normalizeName(platformUserID, "local")
	if err := s.EnsureTenant(ctx, tenantID, tenantID); err != nil {
		return nil, err
	}

	var out IdentityContext
	err := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, person_id, platform, platform_user_id, display_name
		FROM accounts
		WHERE tenant_id = ? AND platform = ? AND platform_user_id = ?`,
		tenantID, platform, platformUserID).
		Scan(&out.AccountID, &out.TenantID, &out.PersonID, &out.Platform, &out.PlatformUserID, &out.DisplayName)
	if err == nil {
		return &out, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
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
	if len(ws.AllowedRoots) == 0 {
		ws.AllowedRoots = []string{ws.LocalPath}
	}
	roots, _ := json.Marshal(ws.AllowedRoots)

	var existingID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM workspaces WHERE tenant_id = ? AND owner_person_id = ? AND local_path = ?`,
		ws.TenantID, ws.OwnerPersonID, ws.LocalPath).Scan(&existingID)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if existingID == "" {
		existingID = "ws_" + uuid.NewString()
	}
	ws.ID = existingID
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO workspaces
		   (id, tenant_id, owner_person_id, name, repo_url, local_path, default_branch, allowed_roots_json, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, owner_person_id, local_path) DO UPDATE SET
		   name = excluded.name,
		   repo_url = excluded.repo_url,
		   default_branch = excluded.default_branch,
		   allowed_roots_json = excluded.allowed_roots_json,
		   status = excluded.status,
		   updated_at = excluded.updated_at`,
		ws.ID, ws.TenantID, ws.OwnerPersonID, ws.Name, ws.RepoURL, ws.LocalPath, ws.DefaultBranch, string(roots), ws.Status, now.Unix(), now.Unix())
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
	var created, updated int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, owner_person_id, name, COALESCE(repo_url, ''), local_path,
		        COALESCE(default_branch, ''), COALESCE(allowed_roots_json, '[]'),
		        status, created_at, updated_at
		 FROM workspaces WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), workspaceID).
		Scan(&ws.ID, &ws.TenantID, &ws.OwnerPersonID, &ws.Name, &ws.RepoURL, &ws.LocalPath, &ws.DefaultBranch, &roots, &ws.Status, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(roots), &ws.AllowedRoots)
	ws.CreatedAt = time.Unix(created, 0)
	ws.UpdatedAt = time.Unix(updated, 0)
	return &ws, nil
}

func (s *Store) ListWorkspaces(ctx context.Context, tenantID, personID string) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tenant_id, owner_person_id, name, COALESCE(repo_url, ''), local_path,
		        COALESCE(default_branch, ''), COALESCE(allowed_roots_json, '[]'),
		        status, created_at, updated_at
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
		var created, updated int64
		if err := rows.Scan(&ws.ID, &ws.TenantID, &ws.OwnerPersonID, &ws.Name, &ws.RepoURL, &ws.LocalPath, &ws.DefaultBranch, &roots, &ws.Status, &created, &updated); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(roots), &ws.AllowedRoots)
		ws.CreatedAt = time.Unix(created, 0)
		ws.UpdatedAt = time.Unix(updated, 0)
		out = append(out, ws)
	}
	return out, rows.Err()
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
	id := "task_" + uuid.NewString()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tasks (id, tenant_id, person_id, workspace_id, title, status, last_channel, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 'running', ?, ?, ?)`,
		id, req.TenantID, req.PersonID, req.WorkspaceID, req.Title, req.Channel, now, now)
	if err != nil {
		return nil, err
	}
	if err := s.SetCurrentTask(ctx, req.TenantID, req.PersonID, id); err != nil {
		return nil, err
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

// CurrentTaskForChannel returns the most recent non-terminal task for this
// person ON THIS CHANNEL. The per-person `current_task` pointer is a single
// slot, so two concurrent sessions (e.g. two CLI terminals) would otherwise
// share one task and bleed context into each other. Scoping by channel gives
// each session its own working task while tasks stay resumable by id from any
// channel. Falls back to the per-person current task when channel is empty.
func (s *Store) CurrentTaskForChannel(ctx context.Context, tenantID, personID, channel string) (*Task, error) {
	if strings.TrimSpace(channel) == "" {
		return s.CurrentTask(ctx, tenantID, personID)
	}
	var taskID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM tasks
		 WHERE tenant_id = ? AND person_id = ? AND last_channel = ?
		   AND status NOT IN ('done','completed','cancelled')
		 ORDER BY updated_at DESC LIMIT 1`,
		normalizeTenant(tenantID), personID, channel).Scan(&taskID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetTask(ctx, tenantID, taskID)
}

func (s *Store) GetTask(ctx context.Context, tenantID, taskID string) (*Task, error) {
	var t Task
	var nextSteps string
	var created, updated int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, person_id, COALESCE(workspace_id, ''), title, status,
		        COALESCE(current_summary, ''), COALESCE(next_steps_json, '[]'),
		        COALESCE(blocked_reason, ''), COALESCE(active_run_id, ''),
		        COALESCE(last_channel, ''), created_at, updated_at
		 FROM tasks WHERE tenant_id = ? AND id = ?`,
		normalizeTenant(tenantID), taskID).
		Scan(&t.ID, &t.TenantID, &t.PersonID, &t.WorkspaceID, &t.Title, &t.Status, &t.CurrentSummary,
			&nextSteps, &t.BlockedReason, &t.ActiveRunID, &t.LastChannel, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(nextSteps), &t.NextSteps)
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
		        COALESCE(current_summary, ''), COALESCE(next_steps_json, '[]'),
		        COALESCE(blocked_reason, ''), COALESCE(active_run_id, ''),
		        COALESCE(last_channel, ''), created_at, updated_at
		 FROM tasks WHERE tenant_id = ? AND person_id = ? ORDER BY updated_at DESC LIMIT ?`,
		normalizeTenant(tenantID), personID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var nextSteps string
		var created, updated int64
		if err := rows.Scan(&t.ID, &t.TenantID, &t.PersonID, &t.WorkspaceID, &t.Title, &t.Status, &t.CurrentSummary,
			&nextSteps, &t.BlockedReason, &t.ActiveRunID, &t.LastChannel, &created, &updated); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(nextSteps), &t.NextSteps)
		t.CreatedAt = time.Unix(created, 0)
		t.UpdatedAt = time.Unix(updated, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) UpdateTaskStatus(ctx context.Context, tenantID, taskID, status, summary string, nextSteps []string) error {
	nextStepsJSON, _ := json.Marshal(nextSteps)
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET status = ?, current_summary = COALESCE(NULLIF(?, ''), current_summary),
		 next_steps_json = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
		status, summary, string(nextStepsJSON), time.Now().Unix(), normalizeTenant(tenantID), taskID)
	return err
}

func (s *Store) StartRun(ctx context.Context, task *Task, channel, inputSummary string) (*Run, error) {
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
		Status:       "running",
		StartedAt:    time.Now(),
	}
	// Insert the run and flip the task to running atomically: a partial write
	// would leave tasks and task_runs disagreeing about the active run.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO task_runs (id, task_id, tenant_id, person_id, workspace_id, channel, input_summary, status, started_at, heartbeat_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.TaskID, run.TenantID, run.PersonID, run.WorkspaceID, run.Channel, run.InputSummary, run.Status, run.StartedAt.Unix(), run.StartedAt.Unix()); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE tasks SET active_run_id = ?, status = 'running', last_channel = ?, updated_at = ? WHERE id = ?`,
		run.ID, run.Channel, time.Now().Unix(), run.TaskID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return run, nil
}

func (s *Store) FinishRun(ctx context.Context, tenantID, runID, status string) error {
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
		`UPDATE tasks SET active_run_id = '', updated_at = ? WHERE tenant_id = ? AND active_run_id = ?`,
		now, tenant, runID); err != nil {
		return err
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
	event.ID = "event_" + uuid.NewString()
	event.CreatedAt = time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO task_events (id, task_id, run_id, type, visibility, channel, payload_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.TaskID, event.RunID, event.Type, event.Visibility, event.Channel, string(event.Payload), event.CreatedAt.Unix())
	if err != nil {
		return nil, err
	}
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
