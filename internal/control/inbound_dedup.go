package control

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"
)

// inboundDedupRetention bounds how long inbound message ids are remembered.
// IM platforms redeliver within minutes; 48h comfortably covers every retry
// schedule plus the weixin sync-buffer replay window after a restart.
const inboundDedupRetention = 48 * time.Hour

type InboundState string

const (
	InboundPending     InboundState = "pending"
	InboundDispatching InboundState = "dispatching"
	InboundAccepted    InboundState = "accepted"
)

type InboundOwner struct {
	TenantID string
	PersonID string
	Preview  string
}

type UncertainInbound struct {
	Platform  string
	MessageID string
	State     InboundState
	Preview   string
	LastError string
	UpdatedAt time.Time
}

// BeginInbound durably saves the original input before an adapter can advance
// its cursor or acknowledge a webhook. A dispatching receipt is deliberately
// not replayable: the previous process may already have caused an effect.
func (s *Store) BeginInbound(ctx context.Context, platform, messageID string, payload []byte, owner InboundOwner) (InboundState, error) {
	platform, messageID = strings.TrimSpace(platform), strings.TrimSpace(messageID)
	if platform == "" || messageID == "" || len(payload) == 0 || owner.TenantID == "" || owner.PersonID == "" {
		return "", fmt.Errorf("inbound platform, message id, payload, and owner are required")
	}
	preview := []rune(strings.TrimSpace(owner.Preview))
	if len(preview) > 120 {
		preview = preview[:120]
	}
	now := time.Now().Unix()
	// Keep unresolved input until it has an explicit outcome, regardless of age.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM inbound_dedup WHERE state = 'accepted' AND created_at < ?`,
		now-int64(inboundDedupRetention/time.Second))
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO inbound_dedup
		(platform, message_id, created_at, state, payload, updated_at, tenant_id, person_id, preview)
		VALUES(?,?,?,'pending',?,?,?,?,?)`,
		platform, messageID, now, payload, now, owner.TenantID, owner.PersonID, string(preview)); err != nil {
		return "", err
	}
	var state string
	var stored []byte
	var tenantID, personID string
	if err := s.db.QueryRowContext(ctx, `SELECT state, payload, tenant_id, person_id FROM inbound_dedup WHERE platform = ? AND message_id = ?`,
		platform, messageID).Scan(&state, &stored, &tenantID, &personID); err != nil {
		return "", err
	}
	if tenantID != "" && (tenantID != owner.TenantID || personID != owner.PersonID) {
		return "", fmt.Errorf("inbound message %s/%s belongs to another identity", platform, messageID)
	}
	if len(stored) > 0 && !bytes.Equal(stored, payload) {
		return "", fmt.Errorf("inbound message %s/%s changed payload on redelivery", platform, messageID)
	}
	return InboundState(state), nil
}

func (s *Store) ListUncertainInboundForPerson(ctx context.Context, tenantID, personID string, limit int) ([]UncertainInbound, error) {
	if tenantID == "" || personID == "" {
		return nil, fmt.Errorf("inbound owner is required")
	}
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT platform, message_id, state, preview, last_error, updated_at
		FROM inbound_dedup WHERE tenant_id = ? AND person_id = ? AND state <> 'accepted'
		ORDER BY updated_at DESC, platform, message_id LIMIT ?`, tenantID, personID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UncertainInbound
	for rows.Next() {
		var row UncertainInbound
		var updatedAt int64
		if err := rows.Scan(&row.Platform, &row.MessageID, &row.State, &row.Preview, &row.LastError, &updatedAt); err != nil {
			return nil, err
		}
		row.UpdatedAt = time.Unix(updatedAt, 0)
		out = append(out, row)
	}
	return out, rows.Err()
}

// ClaimInbound is the last durable boundary before calling the gateway. A
// crash after this transition leaves an uncertain receipt for inspection,
// never a blind second call to a potentially effectful handler.
func (s *Store) ClaimInbound(ctx context.Context, platform, messageID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE inbound_dedup SET state = 'dispatching', updated_at = ?
		WHERE platform = ? AND message_id = ? AND state = 'pending'`, time.Now().Unix(), platform, messageID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) AcceptInbound(ctx context.Context, platform, messageID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE inbound_dedup SET state = 'accepted', updated_at = ?, last_error = ''
		WHERE platform = ? AND message_id = ? AND state = 'dispatching'`, time.Now().Unix(), platform, messageID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("inbound %s/%s is not dispatching", platform, messageID)
	}
	return nil
}

func (s *Store) NoteInboundFailure(ctx context.Context, platform, messageID string, failure error) error {
	if failure == nil {
		return nil
	}
	message := failure.Error()
	if len(message) > 240 {
		message = message[:240]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE inbound_dedup SET last_error = ?, updated_at = ?
		WHERE platform = ? AND message_id = ? AND state = 'dispatching'`,
		message, time.Now().Unix(), platform, messageID)
	return err
}

// InboundReceipt returns the durable state and original bytes so an uncertain
// delivery can be inspected without guessing from a platform retry.
func (s *Store) InboundReceipt(ctx context.Context, platform, messageID string) (InboundState, []byte, error) {
	var state string
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT state, payload FROM inbound_dedup WHERE platform = ? AND message_id = ?`,
		platform, messageID).Scan(&state, &payload)
	return InboundState(state), payload, err
}
