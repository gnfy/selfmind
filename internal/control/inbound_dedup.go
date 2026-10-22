package control

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
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
	// ClaimGeneration is the generation of the receipt's latest claim. 0 means
	// it has not been claimed since generations were recorded; a dispatching
	// receipt at 0 was claimed before they existed.
	ClaimGeneration int64
}

// BeginInbound durably saves the original input before an adapter can advance
// its cursor or acknowledge a webhook. A dispatching receipt is deliberately
// not replayable: the previous process may already have caused an effect. A
// redelivery of a never-started receipt must carry byte-identical input.
func (s *Store) BeginInbound(ctx context.Context, platform, messageID string, payload []byte, owner InboundOwner) (InboundState, error) {
	return s.BeginInboundMatching(ctx, platform, messageID, payload, owner, nil)
}

// InboundEssential reduces an adapter's raw payload to the fields that
// identify its message — platform message id, sender, chat, text, and media
// identity — leaving out transport fields a platform may re-issue when it
// delivers the same message again.
type InboundEssential func(raw []byte) ([]byte, error)

// BeginInboundMatching is BeginInbound with the adapter's notion of message
// identity. The receipt's state answers first: an accepted receipt is a
// duplicate and a dispatching one stays uncertain whatever the redelivery
// carries. Only a never-started receipt compares the essential fields; a
// difference there is another message reusing the ID. Nil essential compares
// raw bytes.
func (s *Store) BeginInboundMatching(ctx context.Context, platform, messageID string, payload []byte, owner InboundOwner, essential InboundEssential) (InboundState, error) {
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
	// Ownership is checked first: another person's receipt never answers for a
	// different sender, whatever its state.
	if tenantID != "" && (tenantID != owner.TenantID || personID != owner.PersonID) {
		return "", fmt.Errorf("inbound message %s/%s belongs to another identity: %w", platform, messageID, ErrInboundConflict)
	}
	if InboundState(state) != InboundPending || len(stored) == 0 || sameInboundMessage(stored, payload, essential) {
		return InboundState(state), nil
	}
	// The owner's never-started receipt records why its redelivery was refused.
	_, _ = s.db.ExecContext(ctx, `UPDATE inbound_dedup SET last_error = ?, updated_at = ?
		WHERE platform = ? AND message_id = ? AND state = 'pending'`,
		"redelivery rejected: content changed", now, platform, messageID)
	return "", fmt.Errorf("inbound message %s/%s changed payload on redelivery: %w", platform, messageID, ErrInboundConflict)
}

func sameInboundMessage(stored, incoming []byte, essential InboundEssential) bool {
	if bytes.Equal(stored, incoming) {
		return true
	}
	if essential == nil {
		return false
	}
	saved, err := essential(stored)
	if err != nil {
		return false
	}
	redelivered, err := essential(incoming)
	return err == nil && bytes.Equal(saved, redelivered)
}

// ErrInboundConflict marks a redelivered platform message ID that no longer
// matches its saved receipt: another identity owns it, or its content changed.
// No retry can reconcile it.
var ErrInboundConflict = errors.New("inbound redelivery conflicts with its saved receipt")

func (s *Store) ListUncertainInboundForPerson(ctx context.Context, tenantID, personID string, limit int) ([]UncertainInbound, error) {
	if tenantID == "" || personID == "" {
		return nil, fmt.Errorf("inbound owner is required")
	}
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT platform, message_id, state, preview, last_error, updated_at, claim_generation
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
		if err := rows.Scan(&row.Platform, &row.MessageID, &row.State, &row.Preview, &row.LastError, &updatedAt, &row.ClaimGeneration); err != nil {
			return nil, err
		}
		row.UpdatedAt = time.Unix(updatedAt, 0)
		out = append(out, row)
	}
	return out, rows.Err()
}

// ClaimInbound is the last durable boundary before calling the gateway. A
// crash after this transition leaves an uncertain receipt for inspection,
// never a blind second call to a potentially effectful handler. The same
// statement advances the receipt's claim generation and returns it; every
// later transition names it, so a transition sent late by an earlier claim
// cannot touch a later one. 0 means nothing was claimed. A receipt saved
// before generations existed claims from 0 like any never-claimed receipt.
// Like every receipt transition after it, the claim is an exact write: an
// error means it did not commit.
func (s *Store) ClaimInbound(ctx context.Context, platform, messageID string) (int64, error) {
	var generation int64
	err := s.exactReceiptWrite(ctx, func(exact context.Context, conn *sql.Conn) error {
		return conn.QueryRowContext(exact, `UPDATE inbound_dedup SET state = 'dispatching', updated_at = ?,
			claim_generation = claim_generation + 1
			WHERE platform = ? AND message_id = ? AND state = 'pending'
			RETURNING claim_generation`, time.Now().Unix(), platform, messageID).Scan(&generation)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return generation, nil
}

// ErrInboundNotOwned means a receipt transition named a claim that does not
// hold the receipt: a later claim took it, it already settled, or the caller
// holds no claim at all — generation 0 never does. Nothing changed.
var ErrInboundNotOwned = errors.New("inbound receipt is not held by this claim")

// InboundReceiptClaim reads a receipt's state and the generation of its
// latest claim. A dispatcher whose transition failed compares that generation
// with its own: a receipt under another generation no longer speaks for it.
func (s *Store) InboundReceiptClaim(ctx context.Context, platform, messageID string) (InboundState, int64, error) {
	var state string
	var generation int64
	err := s.db.QueryRowContext(ctx, `SELECT state, claim_generation FROM inbound_dedup WHERE platform = ? AND message_id = ?`,
		platform, messageID).Scan(&state, &generation)
	return InboundState(state), generation, err
}

// AcceptInbound settles a claim whose input the gateway handled.
func (s *Store) AcceptInbound(ctx context.Context, platform, messageID string, generation int64) error {
	return s.transitionInboundClaim(ctx, platform, messageID, generation,
		`UPDATE inbound_dedup SET state = 'accepted', last_error = '', updated_at = ?`, time.Now().Unix())
}

// ReleaseInbound returns a claimed receipt to pending when the gateway proved
// it refused the input before anything acted on it. The source may redeliver
// it and ClaimInbound again admits exactly one dispatcher. The reason stays on
// the row until a later outcome replaces it.
func (s *Store) ReleaseInbound(ctx context.Context, platform, messageID string, generation int64, reason string) error {
	if runes := []rune(strings.TrimSpace(reason)); len(runes) > 240 {
		reason = string(runes[:240])
	} else {
		reason = string(runes)
	}
	return s.transitionInboundClaim(ctx, platform, messageID, generation,
		`UPDATE inbound_dedup SET state = 'pending', last_error = ?, updated_at = ?`, reason, time.Now().Unix())
}

// NoteInboundFailure records why a claimed receipt stays dispatching. Only the
// claim's holder calls it, so its reason replaces any earlier one.
func (s *Store) NoteInboundFailure(ctx context.Context, platform, messageID string, generation int64, failure error) error {
	if failure == nil {
		return nil
	}
	return s.transitionInboundClaim(ctx, platform, messageID, generation,
		`UPDATE inbound_dedup SET last_error = ?, updated_at = ?`, inboundReason(failure), time.Now().Unix())
}

// NoteUnexplainedInbound records a reason on a dispatching receipt for a
// caller that does not hold its claim, such as a delivery whose own claim
// failed. It lands only while the receipt is dispatching under generation
// and that claim has recorded no reason, so the holder's reason is never
// replaced, while the holder's own later note replaces this one. A reason
// that does not land is not an error.
func (s *Store) NoteUnexplainedInbound(ctx context.Context, platform, messageID string, generation int64, failure error) error {
	if failure == nil || generation <= 0 {
		return nil
	}
	return s.exactReceiptWrite(ctx, func(exact context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(exact, `UPDATE inbound_dedup SET last_error = ?, updated_at = ?
			WHERE `+inboundClaimHolds+` AND last_error = ''`,
			inboundReason(failure), time.Now().Unix(), platform, messageID, generation)
		return err
	})
}

func inboundReason(failure error) string {
	message := failure.Error()
	if len(message) > 240 {
		message = message[:240]
	}
	return message
}

// inboundClaimHolds is the condition every transition after a claim shares:
// the receipt is still dispatching under the caller's generation.
const inboundClaimHolds = `platform = ? AND message_id = ? AND state = 'dispatching' AND claim_generation = ?`

// transitionInboundClaim runs one receipt transition as a single conditional
// statement that also requires the caller's claim to hold the receipt: still
// dispatching, under the caller's generation. A statement that matches no row
// changed nothing and reports ErrInboundNotOwned.
func (s *Store) transitionInboundClaim(ctx context.Context, platform, messageID string, generation int64, update string, args ...any) error {
	// Generation 0 marks a receipt claimed before generations existed, or no
	// claim at all; it never holds a receipt, so it cannot settle one.
	if generation <= 0 {
		return fmt.Errorf("inbound %s/%s: generation %d names no claim: %w", platform, messageID, generation, ErrInboundNotOwned)
	}
	var n int64
	err := s.exactReceiptWrite(ctx, func(exact context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(exact, update+` WHERE `+inboundClaimHolds, append(args, platform, messageID, generation)...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("inbound %s/%s is not dispatching under claim %d: %w", platform, messageID, generation, ErrInboundNotOwned)
	}
	return nil
}

// exactReceiptWrite runs one receipt statement so that an error it returns
// means the statement did not commit. ctx bounds only the wait for a store
// connection, before anything is written; the statement then runs without
// cancellation or deadline, because the SQLite driver reports an ended
// context even after a statement committed. The busy timeout still bounds
// its lock wait.
func (s *Store) exactReceiptWrite(ctx context.Context, write func(exact context.Context, conn *sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return write(context.WithoutCancel(ctx), conn)
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
