package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// A person's read-only marking of one MCP tool (owner decision D6) says that
// this exact tool, of the server configured with this identity, only reads.
// The person's word replaces the once-only approval and the external effect
// claim an unclassified external tool otherwise needs; the server's own
// description of its tools never does. A marking applies only to its person
// and only while the configured server keeps the identity it was made for.
// Revoking keeps the row, so the audit says who marked and unmarked the tool,
// when, and from which endpoint.

const (
	mcpReadOnlyMarkNameMaxBytes    = 256
	mcpReadOnlyMarkChannelMaxBytes = 256
	mcpReadOnlyMarksTable          = "mcp_read_only_marks"
)

var (
	// ErrMCPReadOnlyMarkActor reports an actor that is not an active account
	// of the marking person, or a channel that is not one of its endpoints.
	ErrMCPReadOnlyMarkActor = errors.New("read-only marking actor is not an active endpoint of this person")
	// ErrMCPReadOnlyMarkInvalid reports a server, identity or tool name that
	// cannot name one exact MCP tool.
	ErrMCPReadOnlyMarkInvalid = errors.New("read-only marking needs one exact server, identity and tool name")
	// ErrMCPReadOnlyMarksUnavailable reports a database stamped with the
	// current schema version by an earlier development build, before the
	// marking table was part of that version.
	ErrMCPReadOnlyMarksUnavailable = errors.New("MCP read-only markings are unavailable: the control database lacks the " + mcpReadOnlyMarksTable + " table")
)

// markingsAvailable is false when schema health found the marking table
// missing; every marking call then returns ErrMCPReadOnlyMarksUnavailable.
func (s *Store) markingsAvailable() bool {
	return !slices.Contains(s.missingTables, mcpReadOnlyMarksTable)
}

// MCPReadOnlyMark is one marking and its audit. A zero RevokedAt means the
// marking is in force for its server identity.
type MCPReadOnlyMark struct {
	ID                 string
	TenantID           string
	PersonID           string
	ServerName         string
	ServerIdentity     string
	ToolName           string
	MarkedAt           time.Time
	MarkedByAccountID  string
	MarkedChannel      string
	RevokedAt          time.Time
	RevokedByAccountID string
	RevokedChannel     string
}

// MCPReadOnlyMarkRequest names the tool and the authenticated endpoint the
// person acts from. Unmarking ignores ServerIdentity: it withdraws the
// person's markings of that server name and tool under every identity.
type MCPReadOnlyMarkRequest struct {
	TenantID       string
	PersonID       string
	ServerName     string
	ServerIdentity string
	ToolName       string
	ActorAccountID string
	ActorChannel   string
}

// MarkMCPToolReadOnly records the person's marking for the server's current
// identity. Marking a tool already marked for that identity returns the
// existing marking with created false.
func (s *Store) MarkMCPToolReadOnly(ctx context.Context, request MCPReadOnlyMarkRequest) (MCPReadOnlyMark, bool, error) {
	if s == nil || s.db == nil {
		return MCPReadOnlyMark{}, false, fmt.Errorf("read-only marking store is unavailable")
	}
	if !s.markingsAvailable() {
		return MCPReadOnlyMark{}, false, ErrMCPReadOnlyMarksUnavailable
	}
	request = normalizeMCPReadOnlyMarkRequest(request)
	if !validMCPReadOnlyMarkName(request.ServerName) || !validMCPReadOnlyMarkName(request.ToolName) ||
		!validMCPReadOnlyMarkName(request.ServerIdentity) {
		return MCPReadOnlyMark{}, false, ErrMCPReadOnlyMarkInvalid
	}
	for attempt := 0; attempt < 8; attempt++ {
		mark, created, err := s.markMCPToolReadOnlyOnce(ctx, request)
		if (!isSQLiteBusy(err) && !isMCPReadOnlyMarkConflict(err)) || ctx.Err() != nil {
			return mark, created, err
		}
		select {
		case <-ctx.Done():
			return MCPReadOnlyMark{}, false, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 5 * time.Millisecond):
		}
	}
	return MCPReadOnlyMark{}, false, fmt.Errorf("read-only marking remained busy")
}

func (s *Store) markMCPToolReadOnlyOnce(ctx context.Context, request MCPReadOnlyMarkRequest) (MCPReadOnlyMark, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MCPReadOnlyMark{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateMCPReadOnlyMarkActorTx(ctx, tx, request); err != nil {
		return MCPReadOnlyMark{}, false, err
	}
	existing, err := scanMCPReadOnlyMarks(tx.QueryContext(ctx, mcpReadOnlyMarkColumns+` FROM mcp_read_only_marks
		WHERE tenant_id = ? AND person_id = ? AND server_name = ? AND server_identity = ? AND tool_name = ? AND revoked_at = 0`,
		request.TenantID, request.PersonID, request.ServerName, request.ServerIdentity, request.ToolName))
	if err != nil {
		return MCPReadOnlyMark{}, false, err
	}
	if len(existing) > 0 {
		return existing[0], false, nil
	}
	now := time.Now().Unix()
	mark := MCPReadOnlyMark{ID: "mcpro_" + uuid.NewString(), TenantID: request.TenantID, PersonID: request.PersonID,
		ServerName: request.ServerName, ServerIdentity: request.ServerIdentity, ToolName: request.ToolName,
		MarkedAt: time.Unix(now, 0), MarkedByAccountID: request.ActorAccountID, MarkedChannel: request.ActorChannel}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_read_only_marks
		(id, tenant_id, person_id, server_name, server_identity, tool_name, marked_at, marked_by_account_id, marked_channel)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, mark.ID, mark.TenantID, mark.PersonID, mark.ServerName,
		mark.ServerIdentity, mark.ToolName, now, mark.MarkedByAccountID, mark.MarkedChannel); err != nil {
		return MCPReadOnlyMark{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return MCPReadOnlyMark{}, false, err
	}
	return mark, true, nil
}

// UnmarkMCPToolReadOnly revokes the person's markings of the tool under every
// identity of the named server and reports how many it revoked.
func (s *Store) UnmarkMCPToolReadOnly(ctx context.Context, request MCPReadOnlyMarkRequest) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("read-only marking store is unavailable")
	}
	if !s.markingsAvailable() {
		return 0, ErrMCPReadOnlyMarksUnavailable
	}
	request = normalizeMCPReadOnlyMarkRequest(request)
	if !validMCPReadOnlyMarkName(request.ServerName) || !validMCPReadOnlyMarkName(request.ToolName) {
		return 0, ErrMCPReadOnlyMarkInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateMCPReadOnlyMarkActorTx(ctx, tx, request); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE mcp_read_only_marks
		SET revoked_at = ?, revoked_by_account_id = ?, revoked_channel = ?
		WHERE tenant_id = ? AND person_id = ? AND server_name = ? AND tool_name = ? AND revoked_at = 0`,
		time.Now().Unix(), request.ActorAccountID, request.ActorChannel,
		request.TenantID, request.PersonID, request.ServerName, request.ToolName)
	if err != nil {
		return 0, err
	}
	revoked, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(revoked), nil
}

// ListMCPReadOnlyMarks returns the person's markings in force, under any
// server identity, ordered by server and tool.
func (s *Store) ListMCPReadOnlyMarks(ctx context.Context, tenantID, personID string) ([]MCPReadOnlyMark, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("read-only marking store is unavailable")
	}
	if !s.markingsAvailable() {
		return nil, ErrMCPReadOnlyMarksUnavailable
	}
	return scanMCPReadOnlyMarks(s.db.QueryContext(ctx, mcpReadOnlyMarkColumns+` FROM mcp_read_only_marks
		WHERE tenant_id = ? AND person_id = ? AND revoked_at = 0
		ORDER BY server_name, tool_name, marked_at, id`, normalizeTenant(tenantID), strings.TrimSpace(personID)))
}

// MCPToolMarkedReadOnly reports whether the person's marking of this exact
// tool is in force for this server identity. notAfter, when set, counts only
// markings made at or before it: a schedule carries the person's standing
// decisions from when it was authorised, as with remembered approval classes.
func (s *Store) MCPToolMarkedReadOnly(ctx context.Context, tenantID, personID, serverName, serverIdentity, toolName string, notAfter time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("read-only marking store is unavailable")
	}
	if !s.markingsAvailable() {
		return false, ErrMCPReadOnlyMarksUnavailable
	}
	personID = strings.TrimSpace(personID)
	if personID == "" || serverName == "" || serverIdentity == "" || toolName == "" {
		return false, nil
	}
	cutoff := int64(0)
	if !notAfter.IsZero() {
		cutoff = notAfter.Unix()
	}
	var marked int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM mcp_read_only_marks
		WHERE tenant_id = ? AND person_id = ? AND server_name = ? AND server_identity = ? AND tool_name = ? AND revoked_at = 0
		  AND (? = 0 OR marked_at <= ?)
		LIMIT 1`, normalizeTenant(tenantID), personID, serverName, serverIdentity, toolName, cutoff, cutoff).Scan(&marked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

const mcpReadOnlyMarkColumns = `SELECT id, tenant_id, person_id, server_name, server_identity, tool_name, marked_at,
	marked_by_account_id, marked_channel, revoked_at, revoked_by_account_id, revoked_channel`

func scanMCPReadOnlyMarks(rows *sql.Rows, err error) ([]MCPReadOnlyMark, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MCPReadOnlyMark
	for rows.Next() {
		var mark MCPReadOnlyMark
		var marked, revoked int64
		if err := rows.Scan(&mark.ID, &mark.TenantID, &mark.PersonID, &mark.ServerName, &mark.ServerIdentity,
			&mark.ToolName, &marked, &mark.MarkedByAccountID, &mark.MarkedChannel, &revoked,
			&mark.RevokedByAccountID, &mark.RevokedChannel); err != nil {
			return nil, err
		}
		mark.MarkedAt = time.Unix(marked, 0)
		if revoked != 0 {
			mark.RevokedAt = time.Unix(revoked, 0)
		}
		out = append(out, mark)
	}
	return out, rows.Err()
}

func normalizeMCPReadOnlyMarkRequest(request MCPReadOnlyMarkRequest) MCPReadOnlyMarkRequest {
	request.TenantID = normalizeTenant(request.TenantID)
	request.PersonID = strings.TrimSpace(request.PersonID)
	request.ActorAccountID = strings.TrimSpace(request.ActorAccountID)
	request.ActorChannel = strings.TrimSpace(strings.ToValidUTF8(request.ActorChannel, ""))
	return request
}

// Server and tool names are exact: no trimming, folding or patterns.
func validMCPReadOnlyMarkName(name string) bool {
	if name == "" || len(name) > mcpReadOnlyMarkNameMaxBytes || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validateMCPReadOnlyMarkActorTx applies the person-settlement rule: the
// actor is the person's own active account, and an IM channel keyed by another
// account's platform user id is not this actor's endpoint.
func validateMCPReadOnlyMarkActorTx(ctx context.Context, tx *sql.Tx, request MCPReadOnlyMarkRequest) error {
	if request.PersonID == "" || request.ActorAccountID == "" || request.ActorChannel == "" ||
		len(request.ActorChannel) > mcpReadOnlyMarkChannelMaxBytes {
		return ErrMCPReadOnlyMarkActor
	}
	var accountPerson, accountStatus, accountKey string
	err := tx.QueryRowContext(ctx, `SELECT person_id, status, platform_user_id FROM accounts WHERE tenant_id = ? AND id = ?`,
		request.TenantID, request.ActorAccountID).Scan(&accountPerson, &accountStatus, &accountKey)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (accountPerson != request.PersonID || accountStatus != "active") {
		return ErrMCPReadOnlyMarkActor
	}
	if err != nil {
		return err
	}
	if request.ActorChannel != accountKey {
		var others int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE tenant_id = ? AND platform_user_id = ? AND id <> ?`,
			request.TenantID, request.ActorChannel, request.ActorAccountID).Scan(&others); err != nil {
			return err
		}
		if others != 0 {
			return ErrMCPReadOnlyMarkActor
		}
	}
	return nil
}

// isMCPReadOnlyMarkConflict recognizes a concurrent marking of the same tool
// committing first; the retry then returns that marking.
func isMCPReadOnlyMarkConflict(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint failed") && strings.Contains(message, "mcp_read_only_marks")
}

// migrateMCPReadOnlyMarks adds the person-partitioned marking table. It starts
// empty: no historical row implies that a person ever marked a tool.
func migrateMCPReadOnlyMarks(ctx context.Context, db migrationDB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS mcp_read_only_marks (
		id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		person_id TEXT NOT NULL,
		server_name TEXT NOT NULL,
		server_identity TEXT NOT NULL,
		tool_name TEXT NOT NULL,
		marked_at INTEGER NOT NULL,
		marked_by_account_id TEXT NOT NULL,
		marked_channel TEXT NOT NULL,
		revoked_at INTEGER NOT NULL DEFAULT 0,
		revoked_by_account_id TEXT NOT NULL DEFAULT '',
		revoked_channel TEXT NOT NULL DEFAULT ''
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_read_only_marks_active
		ON mcp_read_only_marks(tenant_id, person_id, server_name, server_identity, tool_name)
		WHERE revoked_at = 0;`)
	return err
}
