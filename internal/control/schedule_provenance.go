package control

// Evidence about who a stored schedule acts for. A schedule row from an older
// release carries no creator and may name no account, so the gateway asks what
// the identity graph and the Run history can prove about it, once, and keeps
// its answer. The evidence queries span every tenant: that release stored
// every job under the default tenant, whatever tenant its Run was in.

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// ActiveAccountsOnPlatform returns every active binding on one platform, most
// recently seen first.
func (s *Store) ActiveAccountsOnPlatform(ctx context.Context, platform string) ([]Account, error) {
	return s.queryAccounts(ctx,
		`SELECT id, tenant_id, person_id, platform, platform_user_id, COALESCE(display_name, ''), status, COALESCE(last_seen_at, 0)
		 FROM accounts
		 WHERE platform = ? AND status = 'active'
		 ORDER BY COALESCE(last_seen_at, 0) DESC, created_at ASC, id ASC`,
		normalizeName(platform, "cli"))
}

// AccountsOfPerson returns the person's active bindings in any tenant, most
// recently seen first.
func (s *Store) AccountsOfPerson(ctx context.Context, personID string) ([]Account, error) {
	return s.queryAccounts(ctx,
		`SELECT id, tenant_id, person_id, platform, platform_user_id, COALESCE(display_name, ''), status, COALESCE(last_seen_at, 0)
		 FROM accounts
		 WHERE person_id = ? AND status = 'active'
		 ORDER BY COALESCE(last_seen_at, 0) DESC, created_at ASC, id ASC`,
		strings.TrimSpace(personID))
}

func (s *Store) queryAccounts(ctx context.Context, query string, args ...interface{}) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
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

// PersonsWithAccounts returns up to limit distinct persons holding any account,
// active or not: everyone who has ever reached this installation. A
// work-history reset keeps them.
func (s *Store) PersonsWithAccounts(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT person_id FROM accounts LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var personID string
		if err := rows.Scan(&personID); err != nil {
			return nil, err
		}
		out = append(out, personID)
	}
	return out, rows.Err()
}

// PersonsRecordingToolResult returns the distinct persons, at most two, whose
// own Runs recorded exactly this result from the named tool between from and
// to. A Run the daemon started — a schedule firing, a watcher, a recovery —
// is not its person's own act, so its receipts name no one. The window bounds
// the scan to the Runs alive then, read through the run-ordered event index.
func (s *Store) PersonsRecordingToolResult(ctx context.Context, tool, result string, from, to time.Time) ([]string, error) {
	if strings.TrimSpace(tool) == "" || strings.TrimSpace(result) == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT r.person_id
		 FROM runs r CROSS JOIN task_events e ON e.run_id = r.id
		 WHERE r.started_at <= ? AND COALESCE(r.finished_at, ?) >= ?
		   AND e.created_at BETWEEN ? AND ? AND e.type = 'tool.completed'
		   AND json_extract(e.payload_json, '$.tool') = ? AND json_extract(e.payload_json, '$.result') = ?
		   AND NOT EXISTS (
		     SELECT 1 FROM task_events started
		      WHERE started.run_id = r.id AND started.type = 'run.started'
		        AND TRIM(COALESCE(json_extract(started.payload_json, '$.origin'), '')) != '')
		 LIMIT 2`,
		to.Unix(), to.Unix(), from.Unix(), from.Unix(), to.Unix(), tool, result)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var personID string
		if err := rows.Scan(&personID); err != nil {
			return nil, err
		}
		out = append(out, personID)
	}
	return out, rows.Err()
}

// installationSettingsPerson is the person id under which person_settings
// keeps a setting of the installation rather than of one person. No person
// has an empty id.
const installationSettingsPerson = ""

// LegacyScheduleVerdict returns the ownership decision recorded for one older
// schedule row, or "" when none is.
func (s *Store) LegacyScheduleVerdict(ctx context.Context, tenantID, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM person_settings WHERE tenant_id = ? AND person_id = ? AND key = ?`,
		normalizeTenant(tenantID), installationSettingsPerson, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// RecordLegacyScheduleVerdict keeps the first decision made about an older
// schedule row and returns the decision that is kept: evidence that later
// disappears never reopens it.
func (s *Store) RecordLegacyScheduleVerdict(ctx context.Context, tenantID, key, verdict string) (string, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO person_settings (tenant_id, person_id, key, value, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(tenant_id, person_id, key) DO NOTHING`,
		normalizeTenant(tenantID), installationSettingsPerson, key, verdict, time.Now().Unix()); err != nil {
		return "", err
	}
	return s.LegacyScheduleVerdict(ctx, tenantID, key)
}

// ReplaceLegacyScheduleVerdict changes the recorded decision about an older
// schedule row from one value to another, and reports whether this call did:
// of several callers racing to replace the same decision, one succeeds. Only
// a person's explicit act on the local command line may call it.
func (s *Store) ReplaceLegacyScheduleVerdict(ctx context.Context, tenantID, key, from, to string) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE person_settings SET value = ?, updated_at = ?
		 WHERE tenant_id = ? AND person_id = ? AND key = ? AND value = ?`,
		to, time.Now().Unix(), normalizeTenant(tenantID), installationSettingsPerson, key, from)
	if err != nil {
		return false, err
	}
	replaced, err := result.RowsAffected()
	return replaced == 1, err
}
