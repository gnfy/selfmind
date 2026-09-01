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
	VerificationState  string
	VerificationRefs   []string
	ClaimMismatch      bool
	NextSteps          []string
	Channel            string
	AssistantContent   string
	Handoff            Handoff
	AnalyzerVersion    int
	MaintenancePayload string
	Event              Event
	// EffectKey identifies one logical side effect across retry runs. Ordinary
	// turns leave it empty; durable watcher finalization uses its stable
	// watch+verdict-revision key.
	EffectKey string
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
	if strings.TrimSpace(input.MaintenancePayload) != "" && input.AnalyzerVersion <= 0 {
		return nil, fmt.Errorf("maintenance analyzer version is required when replay evidence is present")
	}
	input.EffectKey = strings.TrimSpace(input.EffectKey)
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
	duplicateEffect := false
	if input.EffectKey != "" {
		result, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO effect_receipts
			 (effect_key, tenant_id, task_id, run_id, kind, created_at)
			 VALUES (?, ?, ?, ?, 'run_finalization', ?)`,
			input.EffectKey, tenant, input.TaskID, input.RunID, now.Unix())
		if err != nil {
			return nil, fmt.Errorf("claim finalization effect: %w", err)
		}
		if n, _ := result.RowsAffected(); n == 0 {
			duplicateEffect = true
		}
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
	if err := finalizeRunSkillLifecycleTx(ctx, tx, input, personID, now, !duplicateEffect); err != nil {
		return nil, fmt.Errorf("finalize skill lifecycle: %w", err)
	}

	taskStatus := input.TaskStatus
	if !duplicateEffect {
		// Every finalization commits the DERIVED task status (simplification
		// P2): the reducer over pending human input, live runs, watches, and
		// open blockers is the single lifecycle authority. The old weak-label
		// deferral (park the status until the post-run labeler said KEEP) is
		// gone with the labeler routing itself.
		taskStatus, err = resolveFinalTaskStatusTx(ctx, tx, tenant, input.TaskID, input.RunID, input.TaskStatus)
		if err != nil {
			return nil, fmt.Errorf("reduce task status: %w", err)
		}
		result, err = tx.ExecContext(ctx,
			`UPDATE tasks SET status = ?,
		 current_summary = COALESCE(NULLIF(?, ''), current_summary),
		 next_steps_json = ?,
		 active_run_id = CASE WHEN active_run_id = ? THEN '' ELSE active_run_id END,
		 archived_at = CASE WHEN ? = 'archived' THEN COALESCE(archived_at, ?) ELSE NULL END,
		 last_activity_at = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
			taskStatus, input.Summary, string(nextJSON), input.RunID,
			taskStatus, now.Unix(), now.Unix(), now.Unix(), tenant, input.TaskID)
		if err != nil {
			return nil, fmt.Errorf("update task: %w", err)
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return nil, fmt.Errorf("update task affected %d rows", n)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tasks SET active_run_id = CASE WHEN active_run_id = ? THEN '' ELSE active_run_id END,
			 last_activity_at = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`,
			input.RunID, now.Unix(), now.Unix(), tenant, input.TaskID); err != nil {
			return nil, fmt.Errorf("settle duplicate finalization run: %w", err)
		}
	}

	if !duplicateEffect && input.AnalyzerVersion > 0 {
		if err := createMaintenanceJobTx(ctx, tx, tenant, input.RunID, input.AnalyzerVersion, input.MaintenancePayload); err != nil {
			return nil, fmt.Errorf("create maintenance job: %w", err)
		}
	}

	if !duplicateEffect && strings.TrimSpace(input.AssistantContent) != "" {
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
	if !duplicateEffect {
		_, err = tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO task_handoffs
		 (id, task_id, run_id, summary, done_items_json, next_steps_json, changed_files_json, test_status, risks_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"handoff_run_"+input.RunID, input.TaskID, input.RunID, input.Handoff.Summary, string(doneJSON),
			string(handoffNextJSON), string(filesJSON), input.Handoff.TestStatus, string(risksJSON), now.Unix())
		if err != nil {
			return nil, fmt.Errorf("save handoff: %w", err)
		}
	}
	if duplicateEffect {
		var payload map[string]interface{}
		if json.Unmarshal(input.Event.Payload, &payload) != nil || payload == nil {
			payload = map[string]interface{}{}
		}
		payload["effect_suppressed"] = true
		payload["effect_key"] = input.EffectKey
		input.Event.Payload, _ = json.Marshal(payload)
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

// ReconcileTaskAfterRun applies one finished run's lifecycle only after its
// display label is known. It is the second half of weak pre-labeling: storage
// is durable immediately, while task status waits until KEEP or MOVE is
// resolved so a guessed label cannot be closed by mistake.
func (s *Store) ReconcileTaskAfterRun(ctx context.Context, tenantID, taskID, runID, proposed string) error {
	tenant := normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	status, err := resolveFinalTaskStatusTx(ctx, tx, tenant, taskID, runID, proposed)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, last_activity_at = ?, updated_at = ?
		 WHERE tenant_id = ? AND id = ?`,
		status, now, now, tenant, taskID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("reconcile task affected %d rows", n)
	}
	return tx.Commit()
}

// EffectOwnedByRun reports whether runID won a logical effect claim. Delivery
// uses it to suppress duplicate endpoint results after storage has already
// settled the retry run itself.
func (s *Store) EffectOwnedByRun(ctx context.Context, tenantID, effectKey, runID string) (bool, error) {
	effectKey = strings.TrimSpace(effectKey)
	if effectKey == "" {
		return true, nil
	}
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT run_id FROM effect_receipts WHERE tenant_id = ? AND effect_key = ?`, normalizeTenant(tenantID), effectKey).Scan(&owner)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return owner == runID, err
}

// MarkEffectDeliveryEnqueued records that the logical effect's result crossed
// the durable outbox boundary. Queue recovery may settle the source row only
// after this bit is true.
func (s *Store) MarkEffectDeliveryEnqueued(ctx context.Context, tenantID, effectKey string) error {
	if strings.TrimSpace(effectKey) == "" {
		return nil
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE effect_receipts SET delivery_enqueued = 1
		 WHERE tenant_id = ? AND effect_key = ?`,
		normalizeTenant(tenantID), strings.TrimSpace(effectKey))
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("effect receipt not found")
	}
	return nil
}

func (s *Store) EffectDeliveryEnqueued(ctx context.Context, tenantID, effectKey string) (bool, error) {
	if strings.TrimSpace(effectKey) == "" {
		return true, nil
	}
	var value int
	err := s.db.QueryRowContext(ctx,
		`SELECT delivery_enqueued FROM effect_receipts WHERE tenant_id = ? AND effect_key = ?`,
		normalizeTenant(tenantID), strings.TrimSpace(effectKey)).Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return value != 0, err
}

// resolveFinalTaskStatusTx derives the task state from durable blockers before
// applying the run's proposed terminal status. This prevents a late run from
// overwriting a task that still has another active run, a pending user gate,
// an external watch, or a queued watcher finalization.
func resolveFinalTaskStatusTx(
	ctx context.Context,
	tx *sql.Tx,
	tenantID, taskID, finishingRunID, proposed string,
) (string, error) {
	// Explicit user lifecycle controls must win over derived wait evidence.
	// Otherwise /cancel and /task archive could be immediately undone by the
	// very pending approval, clarification, or run they are intended to close.
	switch strings.ToLower(strings.TrimSpace(proposed)) {
	case "cancelled", "archived":
		return proposed, nil
	}

	hasRows := func(query string, args ...interface{}) (bool, error) {
		var found int
		if err := tx.QueryRowContext(ctx, query, args...).Scan(&found); err != nil {
			return false, err
		}
		return found != 0, nil
	}

	pendingUser, err := hasRows(
		`SELECT EXISTS(
			SELECT 1 FROM approval_requests
			WHERE tenant_id = ? AND task_id = ? AND status = 'pending'
			UNION ALL
			SELECT 1 FROM clarify_requests
			WHERE tenant_id = ? AND task_id = ? AND status = 'pending'
		)`,
		tenantID, taskID, tenantID, taskID,
	)
	if err != nil {
		return "", err
	}
	if pendingUser {
		return "waiting_user", nil
	}

	activeRun, err := hasRows(
		`SELECT EXISTS(
			SELECT 1 FROM task_runs
			WHERE tenant_id = ? AND task_id = ? AND id <> ? AND status = 'running'
		)`,
		tenantID, taskID, finishingRunID,
	)
	if err != nil {
		return "", err
	}
	if activeRun {
		return "in_progress", nil
	}

	waitingExternal, err := hasRows(
		`SELECT EXISTS(
			SELECT 1 FROM external_watches
			WHERE tenant_id = ? AND task_id = ? AND status IN ('pending', 'running')
		)`,
		tenantID, taskID,
	)
	if err != nil {
		return "", err
	}
	if waitingExternal {
		return "waiting_external", nil
	}

	waitingFinalization, err := hasRows(
		`SELECT EXISTS(
			SELECT 1 FROM task_queue
			WHERE tenant_id = ? AND task_id = ?
			  AND status IN ('queued', 'started')
			  AND idempotency_key LIKE 'external-watch:%:finalization'
			  AND (COALESCE(run_id, '') = '' OR run_id <> ?)
		)`,
		tenantID, taskID, finishingRunID,
	)
	if err != nil {
		return "", err
	}
	if waitingFinalization {
		return "waiting_finalization", nil
	}

	waitStatus, err := unresolvedRunStatusTx(ctx, tx, tenantID, taskID)
	if err != nil {
		return "", err
	}
	if waitStatus != "" {
		return waitStatus, nil
	}
	return proposed, nil
}

// unresolvedRunStatusTx derives the task's generic wait state from its
// UNCLAIMED resumable runs (simplification §10.3): a run parked in
// waiting_user / verification_partial / blocked / interrupted that no child
// has claimed keeps the label in that state, strongest first. The legacy
// task_blockers rows lost this authority — runs themselves are the source of
// truth, and claiming a parent releases its wait atomically.
func unresolvedRunStatusTx(ctx context.Context, tx *sql.Tx, tenantID, taskID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT status FROM task_runs
		WHERE tenant_id = ? AND task_id = ?
		  AND status IN `+resumableRunStatusSQL+`
		  AND COALESCE(resumed_by_run_id, '') = ''
		  AND NOT EXISTS (
		      SELECT 1 FROM task_runs child
		       WHERE child.tenant_id = task_runs.tenant_id
		         AND child.parent_run_id = task_runs.id)`,
		tenantID, taskID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			return "", err
		}
		seen[status] = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, status := range []string{"waiting_user", "verification_partial", "blocked", "interrupted"} {
		if seen[status] {
			return status, nil
		}
	}
	return "", nil
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
