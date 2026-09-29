package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// NativeIMReplyEdge is a platform-proven reply to one previously delivered
// message. The gateway still rechecks the referenced object's live state.
type NativeIMReplyEdge struct {
	RunID      string
	ApprovalID string
	ClarifyID  string
}

// MarkDeliverySentWithNativeID commits the platform's message identity and the
// outbound state together. A platform message id is scoped to its chat; every
// duplicate send keeps its own reply edge rather than overwriting an older id.
func (s *Store) MarkDeliverySentWithNativeID(ctx context.Context, outboundID, messageID string) error {
	outboundID, messageID = strings.TrimSpace(outboundID), strings.TrimSpace(messageID)
	if outboundID == "" || messageID == "" {
		return fmt.Errorf("outbound and native message ids are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO native_im_reply_edges
		(platform, channel, message_id, outbound_id, tenant_id, person_id, run_id, approval_id, clarify_id, created_at)
		SELECT platform, channel, ?, id, tenant_id, person_id, COALESCE(run_id, ''),
			COALESCE(approval_id, ''), COALESCE(clarify_id, ''), ?
		FROM outbound_messages WHERE id = ? AND status = 'sending'`, messageID, time.Now().Unix(), outboundID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		var linkedID string
		var platform, channel string
		if err := tx.QueryRowContext(ctx, `SELECT platform, channel FROM outbound_messages WHERE id = ? AND status = 'sending'`, outboundID).Scan(&platform, &channel); err != nil {
			return fmt.Errorf("outbound delivery is not sending: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT outbound_id FROM native_im_reply_edges
			WHERE platform = ? AND channel = ? AND message_id = ?`, platform, channel, messageID).Scan(&linkedID); err != nil || linkedID != outboundID {
			return fmt.Errorf("native message id is already linked to another delivery")
		}
	}
	now := time.Now().Unix()
	result, err = tx.ExecContext(ctx, `UPDATE outbound_messages SET status = 'sent', attempts = attempts + 1,
		last_error = '', updated_at = ?, delivered_at = ? WHERE id = ? AND status = 'sending'`, now, now, outboundID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("outbound delivery is no longer sending")
	}
	return tx.Commit()
}

// NativeIMReplyTarget never resolves across a person or chat boundary. A
// missing mapping is an explicit invalid reply edge, not ordinary free text.
func (s *Store) NativeIMReplyTarget(ctx context.Context, tenantID, personID, platform, channel, messageID string) (*NativeIMReplyEdge, error) {
	if strings.TrimSpace(personID) == "" || strings.TrimSpace(platform) == "" ||
		strings.TrimSpace(channel) == "" || strings.TrimSpace(messageID) == "" {
		return nil, sql.ErrNoRows
	}
	var edge NativeIMReplyEdge
	err := s.db.QueryRowContext(ctx, `SELECT e.run_id, e.approval_id, e.clarify_id
		FROM native_im_reply_edges e JOIN outbound_messages o ON o.id = e.outbound_id
		WHERE e.tenant_id = ? AND e.person_id = ? AND e.platform = ? AND e.channel = ? AND e.message_id = ?
		  AND o.status = 'sent'`,
		normalizeTenant(tenantID), strings.TrimSpace(personID), strings.ToLower(strings.TrimSpace(platform)),
		strings.TrimSpace(channel), strings.TrimSpace(messageID)).Scan(&edge.RunID, &edge.ApprovalID, &edge.ClarifyID)
	if err != nil {
		return nil, err
	}
	return &edge, nil
}
