package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PeekPendingTurnChoice is a read-only snapshot for preparing managed
// attachments and execution scope. RoutePendingTurnChoice rechecks this exact
// snapshot inside its write transaction before it accepts the answer.
func (s *Store) PeekPendingTurnChoice(ctx context.Context, tenantID, personID, choiceID string, now time.Time, bareWindow time.Duration) (*PendingTurnChoice, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	tenantID, personID, choiceID = normalizeTenant(tenantID), strings.TrimSpace(personID), strings.TrimSpace(choiceID)
	if personID == "" {
		return nil, ErrTurnChoiceNotFound
	}
	if now.IsZero() {
		now = time.Now()
	}
	if bareWindow <= 0 {
		bareWindow = 30 * time.Minute
	}
	if choiceID != "" {
		choice, err := scanPendingTurnChoice(s.db.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, account_id, channel, resolution_id,
			request_json, options_json, status, chosen_key, created_at, expires_at, claimed_at
			FROM pending_turn_choices WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = ? AND expires_at > ?`,
			tenantID, personID, choiceID, TurnChoicePending, now.Unix()))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrTurnChoiceNotFound
		}
		return choice, err
	}
	cutoff := now.Add(-bareWindow).Unix()
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_turn_choices
		WHERE tenant_id = ? AND person_id = ? AND status = ? AND expires_at > ? AND created_at >= ?`,
		tenantID, personID, TurnChoicePending, now.Unix(), cutoff).Scan(&count); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, ErrTurnChoiceNotFound
	}
	if count != 1 {
		return nil, ErrTurnChoiceAmbiguous
	}
	choice, err := scanPendingTurnChoice(s.db.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, account_id, channel, resolution_id,
		request_json, options_json, status, chosen_key, created_at, expires_at, claimed_at
		FROM pending_turn_choices WHERE tenant_id = ? AND person_id = ? AND status = ? AND expires_at > ? AND created_at >= ?
		ORDER BY created_at DESC LIMIT 1`, tenantID, personID, TurnChoicePending, now.Unix(), cutoff))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTurnChoiceNotFound
	}
	return choice, err
}

type TurnChoiceWorkRoute struct {
	TenantID    string
	PersonID    string
	ChoiceID    string
	OptionKey   string
	RequestJSON string
	Queued      QueuedTask
	Steering    SteeringMessage
}

type TurnChoiceWorkResult struct {
	Choice   *PendingTurnChoice
	Option   *TurnChoiceOption
	Queued   *QueuedTask
	Steering *SteeringMessage
}

// RoutedTurnChoice returns the durable receipt for a repeated exact answer.
// A lost HTTP/IM acknowledgement must not invite the user to submit the same
// work again; the destination row, rather than the choice status alone, proves
// that the first answer was accepted.
func (s *Store) RoutedTurnChoice(ctx context.Context, tenantID, personID, choiceID, optionKey string) (*TurnChoiceWorkResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	tenantID, personID, choiceID = normalizeTenant(tenantID), strings.TrimSpace(personID), strings.TrimSpace(choiceID)
	if personID == "" || choiceID == "" || optionKey == "" {
		return nil, ErrTurnChoiceNotFound
	}
	choice, err := scanPendingTurnChoice(s.db.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, account_id, channel, resolution_id,
		request_json, options_json, status, chosen_key, created_at, expires_at, claimed_at
		FROM pending_turn_choices WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = ? AND chosen_key = ?`,
		tenantID, personID, choiceID, TurnChoiceClaimed, optionKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTurnChoiceNotFound
	}
	if err != nil {
		return nil, err
	}
	option := selectedTurnChoiceOption(choice, optionKey)
	if option == nil || (option.Action != "new" && option.Action != "steer") {
		return nil, ErrTurnChoiceNotFound
	}
	result := &TurnChoiceWorkResult{Choice: choice, Option: option}
	queued, err := s.GetQueuedByIdempotencyKey(ctx, tenantID, "choice:"+choiceID)
	if err != nil {
		return nil, err
	}
	if queued != nil && queued.PersonID == personID {
		result.Queued = queued
		return result, nil
	}
	m, err := scanSteering(s.db.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, run_id, COALESCE(thread_id, ''), channel,
		COALESCE(platform, ''), COALESCE(platform_user_id, ''), COALESCE(workspace_id, ''), COALESCE(approval_mode, ''),
		content, content_hash, status, created_at, updated_at, COALESCE(attachments_json, '[]'), execution_roots_json
		FROM steering_mailbox WHERE tenant_id = ? AND person_id = ? AND id = ?`,
		tenantID, personID, "steer_choice_"+choiceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTurnChoiceNotFound
	}
	if err != nil {
		return nil, err
	}
	if m.Status == SteeringDeferred {
		deferred, err := s.GetQueuedByIdempotencyKey(ctx, tenantID, "steering:"+m.ID)
		if err != nil {
			return nil, err
		}
		if deferred != nil && deferred.PersonID == personID {
			result.Queued = deferred
			return result, nil
		}
	}
	result.Steering = &m
	return result, nil
}

// RoutePendingTurnChoice commits a choice answer and its durable destination
// together. A crash can leave either both records or neither; it cannot erase
// an accepted message between the choice claim and mailbox/queue insertion.
// The current Run status is checked under the same transaction: if the target
// already ended, guidance becomes exact task-pinned queued work.
func (s *Store) RoutePendingTurnChoice(ctx context.Context, route TurnChoiceWorkRoute) (*TurnChoiceWorkResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is unavailable")
	}
	route.TenantID = normalizeTenant(route.TenantID)
	route.PersonID = strings.TrimSpace(route.PersonID)
	if route.PersonID == "" || strings.TrimSpace(route.ChoiceID) == "" || strings.TrimSpace(route.OptionKey) == "" {
		return nil, ErrTurnChoiceNotFound
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		result, err := s.routePendingTurnChoiceOnce(ctx, route)
		if err == nil || !isSQLiteBusy(err) {
			return result, err
		}
		last = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(25*(attempt+1)) * time.Millisecond):
		}
	}
	return nil, last
}

func (s *Store) routePendingTurnChoiceOnce(ctx context.Context, route TurnChoiceWorkRoute) (*TurnChoiceWorkResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now()
	choice, err := scanPendingTurnChoice(tx.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, account_id, channel, resolution_id,
		request_json, options_json, status, chosen_key, created_at, expires_at, claimed_at
		FROM pending_turn_choices WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = ? AND expires_at > ?`,
		route.TenantID, route.PersonID, route.ChoiceID, TurnChoicePending, now.Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTurnChoiceNotFound
	}
	if err != nil {
		return nil, err
	}
	if choice.RequestJSON != route.RequestJSON {
		return nil, ErrTurnChoiceNotFound
	}
	option := selectedTurnChoiceOption(choice, route.OptionKey)
	if option == nil {
		return nil, ErrTurnChoiceOption
	}
	result := &TurnChoiceWorkResult{Choice: choice, Option: option}
	q := route.Queued
	if q.TenantID != route.TenantID || q.PersonID != route.PersonID || strings.TrimSpace(q.Content) == "" {
		return nil, fmt.Errorf("choice work owner or content is invalid")
	}
	if option.Action != "new" && option.Action != "steer" {
		return nil, ErrTurnChoiceOption
	}
	if option.Action == "steer" {
		var runPerson, runTask, runStatus, runWorkspace string
		var rootsJSON string
		err = tx.QueryRowContext(ctx, `SELECT person_id, thread_id, status, COALESCE(workspace_id, ''), COALESCE(execution_roots_json, '[]')
			FROM runs WHERE tenant_id = ? AND id = ?`, route.TenantID, option.RunID).
			Scan(&runPerson, &runTask, &runStatus, &runWorkspace, &rootsJSON)
		if errors.Is(err, sql.ErrNoRows) || runPerson != route.PersonID || runTask != option.TaskID {
			return nil, ErrTurnChoiceNotFound
		}
		if err != nil {
			return nil, err
		}
		if runStatus == "running" {
			m, err := insertChoiceSteeringTx(ctx, tx, route.Steering, q.Content, route, choice.ID, option, now)
			if err != nil {
				return nil, err
			}
			result.Steering = m
		} else {
			q.TaskID, q.WorkspaceID = runTask, runWorkspace
			if err := json.Unmarshal([]byte(rootsJSON), &q.ExecutionRoots); err != nil {
				return nil, err
			}
			if continuityRunResumableForQueue(runStatus) {
				q.ReplyToRunID = option.RunID
			}
		}
	}
	if result.Steering == nil {
		queued, err := insertChoiceQueueTx(ctx, tx, q, choice.ID, now)
		if err != nil {
			return nil, err
		}
		result.Queued = queued
	}
	updated, err := tx.ExecContext(ctx, `UPDATE pending_turn_choices SET status = ?, chosen_key = ?, claimed_at = ?, request_json = '{}'
		WHERE tenant_id = ? AND person_id = ? AND id = ? AND status = ? AND expires_at > ?`,
		TurnChoiceClaimed, route.OptionKey, now.Unix(), route.TenantID, route.PersonID, choice.ID, TurnChoicePending, now.Unix())
	if err != nil {
		return nil, err
	}
	if n, _ := updated.RowsAffected(); n != 1 {
		return nil, ErrTurnChoiceNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	choice.Status, choice.ChosenKey, choice.RequestJSON = TurnChoiceClaimed, route.OptionKey, "{}"
	return result, nil
}

func selectedTurnChoiceOption(choice *PendingTurnChoice, key string) *TurnChoiceOption {
	if choice == nil {
		return nil
	}
	for i := range choice.Options {
		if choice.Options[i].Key == key {
			copy := choice.Options[i]
			return &copy
		}
	}
	return nil
}

func insertChoiceSteeringTx(ctx context.Context, tx *sql.Tx, m SteeringMessage, content string, route TurnChoiceWorkRoute, choiceID string, option *TurnChoiceOption, now time.Time) (*SteeringMessage, error) {
	if m.TenantID != route.TenantID || m.PersonID != route.PersonID || m.RunID != option.RunID || m.TaskID != option.TaskID || strings.TrimSpace(m.Content) != content {
		return nil, fmt.Errorf("choice steering target is invalid")
	}
	m.ID = "steer_choice_" + choiceID
	m.ContentHash = SteeringContentHash(m.Content)
	m.Status, m.CreatedAt, m.UpdatedAt = SteeringAccepted, now, now
	attachmentsJSON, err := encodeAttachmentRefs(m.Attachments)
	if err != nil {
		return nil, err
	}
	var bindingJSON sql.NullString
	if len(m.ExecutionRoots) > 0 {
		encoded, err := encodeExecutionRoots(m.ExecutionRoots)
		if err != nil {
			return nil, err
		}
		bindingJSON = sql.NullString{String: encoded, Valid: true}
	}
	m.RootsRecorded = bindingJSON.Valid
	_, err = tx.ExecContext(ctx, `INSERT INTO steering_mailbox
		(id, tenant_id, person_id, run_id, thread_id, channel, platform, platform_user_id, workspace_id, approval_mode,
		 content, content_hash, status, created_at, updated_at, attachments_json, execution_roots_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.TenantID, m.PersonID, m.RunID, m.TaskID, m.Channel, m.Platform, m.PlatformUserID, m.WorkspaceID,
		m.ApprovalMode, m.Content, m.ContentHash, m.Status, now.Unix(), now.Unix(), attachmentsJSON, bindingJSON)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func insertChoiceQueueTx(ctx context.Context, tx *sql.Tx, q QueuedTask, choiceID string, now time.Time) (*QueuedTask, error) {
	q.ID = "queue_" + uuid.NewString()
	q.Status, q.CreatedAt = QueueStatusQueued, now
	q.Class, q.Priority = QueueClassForeground, QueuePriorityForeground
	q.IdempotencyKey = "choice:" + choiceID
	rootsJSON, err := encodeExecutionRoots(q.ExecutionRoots)
	if err != nil {
		return nil, err
	}
	attachmentsJSON, err := encodeAttachmentRefs(q.Attachments)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO task_queue
		(id, tenant_id, person_id, channel, platform, platform_user_id, content, approval_mode, workspace_id, execution_roots_json,
		 thread_id, reply_to_run_id, approval_id, clarify_id, idempotency_key, class, priority, not_before, status, created_at, attachments_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, ?, 0, ?, ?, ?)`,
		q.ID, q.TenantID, q.PersonID, q.Channel, q.Platform, q.PlatformUserID, q.Content, q.ApprovalMode, q.WorkspaceID, rootsJSON,
		q.TaskID, q.ReplyToRunID, q.IdempotencyKey, q.Class, q.Priority, q.Status, now.Unix(), attachmentsJSON)
	if err != nil {
		return nil, err
	}
	return &q, nil
}

func continuityRunResumableForQueue(status string) bool {
	switch status {
	case "interrupted", "waiting_user", "verification_partial", "blocked":
		return true
	default:
		return false
	}
}
