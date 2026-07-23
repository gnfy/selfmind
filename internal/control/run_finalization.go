package control

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RunFinalization is the complete durable result of one agent run. Keeping
// these fields in one control-layer transaction prevents a crash from leaving
// a terminal run with a running task, a missing handoff, or no terminal event.
type RunFinalization struct {
	Identity           IdentityContext
	RunID              string
	RunStatus          string
	TaskID             string
	TaskStatus         string
	Summary            string
	NextSteps          []string
	Channel            string
	AssistantContent   string
	Handoff            Handoff
	AnalyzerVersion    int
	MaintenancePayload string
	Event              Event
}

// MaterializeRunFinalization commits the run, task, assistant message,
// handoff, maintenance slot, and terminal event as one replay-safe unit. The
// event idempotency key is the transaction's durable commit marker.
func (s *Store) MaterializeRunFinalization(ctx context.Context, input RunFinalization) (*Event, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("control store is required")
	}
	input.RunID = strings.TrimSpace(input.RunID)
	input.TaskID = strings.TrimSpace(input.TaskID)
	if input.RunID == "" || input.TaskID == "" {
		return nil, fmt.Errorf("run id and task id are required")
	}
	tenant := normalizeTenant(input.Identity.TenantID)
	if input.RunStatus == "" || input.RunStatus == "running" || input.RunStatus == "in_progress" {
		input.RunStatus = "done"
	}
	if input.TaskStatus == "" || input.TaskStatus == "running" {
		input.TaskStatus = "in_progress"
	}
	if input.AnalyzerVersion <= 0 {
		input.AnalyzerVersion = 1
	}
	input.Event.TaskID = input.TaskID
	input.Event.RunID = input.RunID
	if input.Event.Type == "" {
		input.Event.Type = "run.finished"
	}
	if input.Event.Visibility == "" {
		input.Event.Visibility = "task"
	}
	if input.Event.Channel == "" {
		input.Event.Channel = input.Channel
	}
	input.Event.IdempotencyKey = strings.TrimSpace(input.Event.IdempotencyKey)
	if input.Event.IdempotencyKey == "" {
		input.Event.IdempotencyKey = "run:" + input.RunID + ":terminal"
	}
	if existing, err := s.eventByIdempotencyKey(ctx, input.Event.IdempotencyKey); err == nil {
		return existing, nil
	} else if err != sql.ErrNoRows {
		return nil, err
	}

	now := time.Now()
	nextJSON, _ := json.Marshal(input.NextSteps)
	doneJSON, _ := json.Marshal(input.Handoff.DoneItems)
	handoffNextJSON, _ := json.Marshal(input.Handoff.NextSteps)
	filesJSON, _ := json.Marshal(input.Handoff.ChangedFiles)
	risksJSON, _ := json.Marshal(input.Handoff.Risks)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var personID string
	if err := tx.QueryRowContext(ctx,
		`SELECT person_id FROM tasks WHERE tenant_id = ? AND id = ?`, tenant, input.TaskID,
	).Scan(&personID); err != nil {
		return nil, fmt.Errorf("load finalization task: %w", err)
	}
	if input.Identity.PersonID != "" && input.Identity.PersonID != personID {
		return nil, fmt.Errorf("finalization identity does not own task")
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE task_runs SET status = ?, finished_at = ?, heartbeat_at = ?
		 WHERE tenant_id = ? AND id = ? AND task_id = ?`,
		input.RunStatus, now.Unix(), now.Unix(), tenant, input.RunID, input.TaskID)
	if err != nil {
		return nil, fmt.Errorf("finish run: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("finish run affected %d rows", n)
	}

	result, err = tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, current_summary = COALESCE(NULLIF(?, ''), current_summary),
		 next_steps_json = ?, active_run_id = CASE WHEN active_run_id = ? THEN '' ELSE active_run_id END,
		 archived_at = CASE WHEN ? = 'archived' THEN COALESCE(archived_at, ?) ELSE NULL END,
		 last_activity_at = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
		input.TaskStatus, input.Summary, string(nextJSON), input.RunID,
		input.TaskStatus, now.Unix(), now.Unix(), now.Unix(), tenant, input.TaskID)
	if err != nil {
		return nil, fmt.Errorf("update task: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("update task affected %d rows", n)
	}

	if err := createMaintenanceJobTx(ctx, tx, tenant, input.RunID, input.AnalyzerVersion, input.MaintenancePayload); err != nil {
		return nil, fmt.Errorf("create maintenance job: %w", err)
	}

	if strings.TrimSpace(input.AssistantContent) != "" {
		_, err = tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO channel_messages
			 (id, tenant_id, person_id, account_id, channel, task_id, role, content, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, 'assistant', ?, ?)`,
			"msg_run_"+input.RunID+"_assistant", tenant, personID, input.Identity.AccountID,
			normalizeName(input.Channel, input.Identity.Platform), input.TaskID, input.AssistantContent, now.Unix())
		if err != nil {
			return nil, fmt.Errorf("record assistant message: %w", err)
		}
	}

	input.Handoff.TaskID = input.TaskID
	if input.Handoff.Summary == "" {
		input.Handoff.Summary = input.Summary
	}
	if input.Handoff.NextSteps == nil {
		input.Handoff.NextSteps = input.NextSteps
		handoffNextJSON, _ = json.Marshal(input.Handoff.NextSteps)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO task_handoffs
		 (id, task_id, summary, done_items_json, next_steps_json, changed_files_json, test_status, risks_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"handoff_run_"+input.RunID, input.TaskID, input.Handoff.Summary, string(doneJSON),
		string(handoffNextJSON), string(filesJSON), input.Handoff.TestStatus, string(risksJSON), now.Unix())
	if err != nil {
		return nil, fmt.Errorf("save handoff: %w", err)
	}

	if len(input.Event.Payload) == 0 {
		input.Event.Payload = json.RawMessage(`{}`)
	}
	var outcomeEvent *Event
	if payload := outcomeFromTerminalPayload(input.Event.Payload); len(payload) > 0 {
		var existing int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM task_events WHERE run_id = ? AND type = 'run.outcome'`, input.RunID,
		).Scan(&existing); err != nil {
			return nil, fmt.Errorf("check run outcome event: %w", err)
		}
		if existing == 0 {
			event := Event{
				ID:             "event_" + uuid.NewString(),
				TenantID:       tenant,
				PersonID:       personID,
				TaskID:         input.TaskID,
				RunID:          input.RunID,
				Type:           "run.outcome",
				Visibility:     input.Event.Visibility,
				Channel:        input.Event.Channel,
				Payload:        payload,
				IdempotencyKey: "run:" + input.RunID + ":outcome",
				CreatedAt:      now,
			}
			if err := tx.QueryRowContext(ctx,
				`UPDATE event_sequence SET next_cursor = next_cursor + 1 WHERE id = 1 RETURNING next_cursor`,
			).Scan(&event.Cursor); err != nil {
				return nil, fmt.Errorf("allocate outcome event cursor: %w", err)
			}
			result, err := tx.ExecContext(ctx,
				`INSERT INTO task_events
				 (id, cursor, task_id, run_id, type, visibility, channel, payload_json, idempotency_key, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT(idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '' DO NOTHING`,
				event.ID, event.Cursor, event.TaskID, event.RunID, event.Type, event.Visibility,
				event.Channel, string(event.Payload), event.IdempotencyKey, now.Unix())
			if err != nil {
				return nil, fmt.Errorf("append outcome event: %w", err)
			}
			if n, _ := result.RowsAffected(); n == 1 {
				outcomeEvent = &event
			}
		}
	}

	input.Event.ID = "event_" + uuid.NewString()
	input.Event.CreatedAt = now
	input.Event.TenantID = tenant
	input.Event.PersonID = personID
	if err := tx.QueryRowContext(ctx,
		`UPDATE event_sequence SET next_cursor = next_cursor + 1 WHERE id = 1 RETURNING next_cursor`,
	).Scan(&input.Event.Cursor); err != nil {
		return nil, fmt.Errorf("allocate terminal event cursor: %w", err)
	}
	result, err = tx.ExecContext(ctx,
		`INSERT INTO task_events
		 (id, cursor, task_id, run_id, type, visibility, channel, payload_json, idempotency_key, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(idempotency_key) WHERE idempotency_key IS NOT NULL AND idempotency_key != '' DO NOTHING`,
		input.Event.ID, input.Event.Cursor, input.TaskID, input.RunID, input.Event.Type,
		input.Event.Visibility, input.Event.Channel, string(input.Event.Payload), input.Event.IdempotencyKey, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("append terminal event: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		if err := tx.Rollback(); err != nil {
			return nil, err
		}
		return s.eventByIdempotencyKey(ctx, input.Event.IdempotencyKey)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if s.events != nil {
		if outcomeEvent != nil {
			s.events.publish(*outcomeEvent)
		}
		s.events.publish(input.Event)
	}
	return &input.Event, nil
}

func outcomeFromTerminalPayload(payload json.RawMessage) json.RawMessage {
	var envelope struct {
		Outcome json.RawMessage `json:"outcome"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &envelope) != nil || len(envelope.Outcome) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(envelope.Outcome)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || !json.Valid(trimmed) {
		return nil
	}
	return append(json.RawMessage(nil), trimmed...)
}
