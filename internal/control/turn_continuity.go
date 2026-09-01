package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	TurnChoicePending = "pending"
	TurnChoiceClaimed = "claimed"
	TurnChoiceExpired = "expired"
)

// TurnChoiceOption is one gateway-issued answer to a continuity
// disambiguation. Action is interpreted by the gateway only after the row is
// atomically claimed and the target is revalidated against the person.
type TurnChoiceOption struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Action string `json:"action"`
	TaskID string `json:"task_id,omitempty"`
	RunID  string `json:"run_id,omitempty"`
}

// PendingTurnChoice is durable because the question may be answered from a
// different bound endpoint or after a daemon restart. RequestJSON is a bounded,
// short-lived gateway snapshot, not a transcript or model prompt.
type PendingTurnChoice struct {
	ID           string
	TenantID     string
	PersonID     string
	AccountID    string
	Channel      string
	ResolutionID string
	RequestJSON  string
	Options      []TurnChoiceOption
	Status       string
	ChosenKey    string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ClaimedAt    *time.Time
}

type PendingTurnChoiceCreate struct {
	TenantID     string
	PersonID     string
	AccountID    string
	Channel      string
	ResolutionID string
	RequestJSON  string
	Options      []TurnChoiceOption
	ExpiresAt    time.Time
}

// CreatePendingTurnChoice persists one person-scoped choice. It intentionally
// does not supersede another live choice: a bare number is accepted only when
// exactly one row is eligible, while reply metadata or /choose can name either
// row precisely.
func (s *Store) CreatePendingTurnChoice(ctx context.Context, input PendingTurnChoiceCreate) (*PendingTurnChoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	input.TenantID = normalizeTenant(input.TenantID)
	input.PersonID = strings.TrimSpace(input.PersonID)
	input.RequestJSON = strings.TrimSpace(input.RequestJSON)
	if input.PersonID == "" || input.RequestJSON == "" {
		return nil, fmt.Errorf("person id and request snapshot are required")
	}
	if len(input.Options) < 2 || len(input.Options) > 4 {
		return nil, fmt.Errorf("turn choice requires 2 to 4 options")
	}
	seen := make(map[string]struct{}, len(input.Options))
	for i := range input.Options {
		input.Options[i].Key = strings.TrimSpace(input.Options[i].Key)
		input.Options[i].Label = strings.TrimSpace(input.Options[i].Label)
		input.Options[i].Action = strings.ToLower(strings.TrimSpace(input.Options[i].Action))
		if input.Options[i].Key == "" || input.Options[i].Label == "" || input.Options[i].Action == "" {
			return nil, fmt.Errorf("turn choice option key, label, and action are required")
		}
		switch input.Options[i].Action {
		case "new":
		case "resume", "steer", "observe":
			if strings.TrimSpace(input.Options[i].TaskID) == "" || strings.TrimSpace(input.Options[i].RunID) == "" {
				return nil, fmt.Errorf("turn choice action %s requires task and run ids", input.Options[i].Action)
			}
		default:
			return nil, fmt.Errorf("unsupported turn choice action %q", input.Options[i].Action)
		}
		if _, duplicate := seen[input.Options[i].Key]; duplicate {
			return nil, fmt.Errorf("duplicate turn choice option %q", input.Options[i].Key)
		}
		seen[input.Options[i].Key] = struct{}{}
	}
	optionsJSON, err := json.Marshal(input.Options)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if input.ExpiresAt.IsZero() || !input.ExpiresAt.After(now) {
		input.ExpiresAt = now.Add(24 * time.Hour)
	}
	choice := &PendingTurnChoice{
		ID:           "choice_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		TenantID:     input.TenantID,
		PersonID:     input.PersonID,
		AccountID:    strings.TrimSpace(input.AccountID),
		Channel:      strings.TrimSpace(input.Channel),
		ResolutionID: strings.TrimSpace(input.ResolutionID),
		RequestJSON:  input.RequestJSON,
		Options:      append([]TurnChoiceOption(nil), input.Options...),
		Status:       TurnChoicePending,
		CreatedAt:    now,
		ExpiresAt:    input.ExpiresAt,
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO pending_turn_choices
		(id, tenant_id, person_id, account_id, channel, resolution_id, request_json, options_json,
		 status, chosen_key, created_at, expires_at, claimed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, 0)`,
		choice.ID, choice.TenantID, choice.PersonID, choice.AccountID, choice.Channel,
		strings.TrimSpace(input.ResolutionID), choice.RequestJSON, string(optionsJSON), choice.Status, choice.CreatedAt.Unix(), choice.ExpiresAt.Unix())
	if err != nil {
		return nil, err
	}
	return choice, nil
}

var (
	ErrTurnChoiceNotFound  = errors.New("turn choice is not available")
	ErrTurnChoiceAmbiguous = errors.New("more than one turn choice is waiting")
	ErrTurnChoiceOption    = errors.New("turn choice option is invalid")
)

// ClaimPendingTurnChoice atomically consumes one choice. An empty choiceID is
// the bare-number path and is deliberately narrower: exactly one pending row
// created within bareWindow must exist for the person. A named choice remains
// claimable until its durable expiry.
func (s *Store) ClaimPendingTurnChoice(ctx context.Context, tenantID, personID, choiceID, optionKey string, now time.Time, bareWindow time.Duration) (*PendingTurnChoice, *TurnChoiceOption, error) {
	if s == nil || s.db == nil {
		return nil, nil, fmt.Errorf("control store is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	choiceID = strings.TrimSpace(choiceID)
	optionKey = strings.TrimSpace(optionKey)
	if personID == "" || optionKey == "" {
		return nil, nil, ErrTurnChoiceNotFound
	}
	if now.IsZero() {
		now = time.Now()
	}
	if bareWindow <= 0 {
		bareWindow = 30 * time.Minute
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE pending_turn_choices SET status = ?, request_json = '{}'
		WHERE tenant_id = ? AND person_id = ? AND status = ? AND expires_at <= ?`,
		TurnChoiceExpired, tenantID, personID, TurnChoicePending, now.Unix()); err != nil {
		return nil, nil, err
	}

	var row *sql.Row
	if choiceID != "" {
		row = tx.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, account_id, channel, resolution_id,
			request_json, options_json, status, chosen_key, created_at, expires_at, claimed_at
			FROM pending_turn_choices
			WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = ? AND expires_at > ?`,
			tenantID, personID, choiceID, TurnChoicePending, now.Unix())
	} else {
		var count int
		cutoff := now.Add(-bareWindow).Unix()
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_turn_choices
			WHERE tenant_id = ? AND person_id = ? AND status = ? AND expires_at > ? AND created_at >= ?`,
			tenantID, personID, TurnChoicePending, now.Unix(), cutoff).Scan(&count); err != nil {
			return nil, nil, err
		}
		if count == 0 {
			return nil, nil, ErrTurnChoiceNotFound
		}
		if count != 1 {
			return nil, nil, ErrTurnChoiceAmbiguous
		}
		row = tx.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, account_id, channel, resolution_id,
			request_json, options_json, status, chosen_key, created_at, expires_at, claimed_at
			FROM pending_turn_choices
			WHERE tenant_id = ? AND person_id = ? AND status = ? AND expires_at > ? AND created_at >= ?
			ORDER BY created_at DESC LIMIT 1`, tenantID, personID, TurnChoicePending, now.Unix(), cutoff)
	}
	choice, err := scanPendingTurnChoice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrTurnChoiceNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	var selected *TurnChoiceOption
	for i := range choice.Options {
		if choice.Options[i].Key == optionKey {
			copy := choice.Options[i]
			selected = &copy
			break
		}
	}
	if selected == nil {
		return nil, nil, ErrTurnChoiceOption
	}
	result, err := tx.ExecContext(ctx, `UPDATE pending_turn_choices
		SET status = ?, chosen_key = ?, claimed_at = ?, request_json = '{}'
		WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = ? AND expires_at > ?`,
		TurnChoiceClaimed, optionKey, now.Unix(), tenantID, personID, choice.ID, TurnChoicePending, now.Unix())
	if err != nil {
		return nil, nil, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return nil, nil, ErrTurnChoiceNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	choice.Status = TurnChoiceClaimed
	choice.ChosenKey = optionKey
	claimed := now
	choice.ClaimedAt = &claimed
	return choice, selected, nil
}

func scanPendingTurnChoice(row rowScanner) (*PendingTurnChoice, error) {
	var choice PendingTurnChoice
	var optionsJSON string
	var created, expires, claimed int64
	if err := row.Scan(&choice.ID, &choice.TenantID, &choice.PersonID, &choice.AccountID, &choice.Channel, &choice.ResolutionID,
		&choice.RequestJSON, &optionsJSON, &choice.Status, &choice.ChosenKey, &created, &expires, &claimed); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(optionsJSON), &choice.Options); err != nil {
		return nil, err
	}
	choice.CreatedAt = time.Unix(created, 0)
	choice.ExpiresAt = time.Unix(expires, 0)
	if claimed > 0 {
		at := time.Unix(claimed, 0)
		choice.ClaimedAt = &at
	}
	return &choice, nil
}

type TurnResolutionRecord struct {
	ID           string
	TenantID     string
	PersonID     string
	AccountID    string
	Channel      string
	Input        string
	Mode         string
	Decision     string
	Certainty    string
	TargetTaskID string
	TargetRunID  string
	CandidateIDs []string
	Evidence     []string
	Provider     string
	Model        string
	Latency      time.Duration
	ErrorClass   string
	CorrectionOf string
	CreatedAt    time.Time
}

// RecordTurnResolution stores content-free decision evidence. The original
// message remains on its channel path; only its hash is duplicated here.
func (s *Store) RecordTurnResolution(ctx context.Context, record TurnResolutionRecord) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("control store is unavailable")
	}
	if strings.TrimSpace(record.PersonID) == "" {
		return "", fmt.Errorf("person id is required")
	}
	if strings.TrimSpace(record.ID) == "" {
		record.ID = "turnres_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	candidateJSON, _ := json.Marshal(record.CandidateIDs)
	evidenceJSON, _ := json.Marshal(record.Evidence)
	sum := sha256.Sum256([]byte(record.Input))
	_, err := s.db.ExecContext(ctx, `INSERT INTO turn_resolution_events
		(id, tenant_id, person_id, account_id, channel, input_hash, mode, decision,
		 certainty, target_task_id, target_run_id, candidate_ids_json, evidence_json,
		 provider, model, latency_ms, error_class, correction_of, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, normalizeTenant(record.TenantID), strings.TrimSpace(record.PersonID),
		strings.TrimSpace(record.AccountID), strings.TrimSpace(record.Channel), hex.EncodeToString(sum[:]),
		strings.TrimSpace(record.Mode), strings.ToLower(strings.TrimSpace(record.Decision)),
		strings.ToLower(strings.TrimSpace(record.Certainty)), strings.TrimSpace(record.TargetTaskID),
		strings.TrimSpace(record.TargetRunID), string(candidateJSON), string(evidenceJSON),
		strings.TrimSpace(record.Provider), strings.TrimSpace(record.Model), record.Latency.Milliseconds(),
		strings.TrimSpace(record.ErrorClass), strings.TrimSpace(record.CorrectionOf), record.CreatedAt.Unix())
	return record.ID, err
}

func (s *Store) CountTurnResolutionCorrections(ctx context.Context, tenantID, personID, sourceID string) (int, error) {
	if s == nil || s.db == nil || strings.TrimSpace(personID) == "" || strings.TrimSpace(sourceID) == "" {
		return 0, nil
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_resolution_events
		WHERE tenant_id = ? AND person_id = ? AND correction_of = ?`,
		normalizeTenant(tenantID), strings.TrimSpace(personID), strings.TrimSpace(sourceID)).Scan(&count)
	return count, err
}

// PruneTurnContinuity expires abandoned request snapshots and bounds the
// content-free decision audit. Claimed snapshots are erased in the claim
// transaction; this sweep covers unanswered rows and old metadata.
func (s *Store) PruneTurnContinuity(ctx context.Context, now time.Time) (choices, resolutions int64, err error) {
	if s == nil || s.db == nil {
		return 0, 0, fmt.Errorf("control store is unavailable")
	}
	if now.IsZero() {
		now = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE pending_turn_choices
		SET status = ?, request_json = '{}'
		WHERE status = ? AND expires_at <= ?`, TurnChoiceExpired, TurnChoicePending, now.Unix()); err != nil {
		return 0, 0, err
	}
	choiceResult, err := tx.ExecContext(ctx, `DELETE FROM pending_turn_choices
		WHERE status != ? AND created_at < ?`, TurnChoicePending, now.Add(-7*24*time.Hour).Unix())
	if err != nil {
		return 0, 0, err
	}
	resolutionResult, err := tx.ExecContext(ctx, `DELETE FROM turn_resolution_events
		WHERE created_at < ?`, now.Add(-90*24*time.Hour).Unix())
	if err != nil {
		return 0, 0, err
	}
	if choices, err = choiceResult.RowsAffected(); err != nil {
		return 0, 0, err
	}
	if resolutions, err = resolutionResult.RowsAffected(); err != nil {
		return 0, 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return choices, resolutions, nil
}
