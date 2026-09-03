package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	ThreadKindInteraction = "interaction"
	ThreadKindWork        = "work"
	ThreadKindRecurring   = "recurring"

	ThreadVisibilityUnlisted = "unlisted"
	ThreadVisibilityListed   = "listed"
	ThreadVisibilityArchived = "archived"

	ThreadViewListed   = "listed"
	ThreadViewSettled  = "settled"
	ThreadViewArchived = "archived"
	ThreadViewAll      = "all"

	ThreadActivityActive         = "active"
	ThreadActivityNeedsAttention = "needs_attention"
	ThreadActivityMonitoring     = "monitoring"
	ThreadActivityResumable      = "resumable"
	ThreadActivitySettled        = "settled"
)

// Thread is the durable grouping and presentation identity for related Runs.
// It deliberately has no execution status, active-run pointer, blocked reason,
// or next-steps field: those facts belong to Runs and pending control objects.
type Thread struct {
	ID             string
	TenantID       string
	PersonID       string
	WorkspaceID    string
	Kind           string
	Visibility     string
	Title          string
	Summary        string
	Pinned         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastActivityAt time.Time
}

type ThreadCreate struct {
	TenantID    string
	PersonID    string
	WorkspaceID string
	Title       string
	Channel     string
}

type ThreadQuery struct {
	View        string
	WorkspaceID string
	Keyword     string
	Limit       int
	Offset      int
}

type ThreadPage struct {
	Threads []Thread
	Total   int
	Limit   int
	Offset  int
}

// AttentionItem is one exact Run that currently needs the person. RunStatus and
// Channel come from the Run row so clients can show the parked state and
// prefer same-channel work without a second lookup.
type AttentionItem struct {
	Thread     Thread
	RunID      string
	RunSummary string
	RunStatus  string
	Channel    string
	Activity   string
}

func (p ThreadPage) HasMore() bool {
	return p.Offset+len(p.Threads) < p.Total
}

// WorkTimeline is the deep module through which gateway and UI callers read
// and govern work history. SQLite remains an internal, locally substitutable
// dependency; no single-adapter interface is introduced.
type WorkTimeline struct {
	store *Store
}

func NewWorkTimeline(store *Store) *WorkTimeline {
	return &WorkTimeline{store: store}
}

// CreateInteraction retains an ordinary root interaction without putting it
// in the user's work list. Later durable evidence may promote it in place.
func (w *WorkTimeline) CreateInteraction(ctx context.Context, req ThreadCreate) (*Thread, error) {
	if w == nil || w.store == nil {
		return nil, fmt.Errorf("work timeline is unavailable")
	}
	task, err := w.store.CreateTask(ctx, TaskCreate{
		TenantID: req.TenantID, PersonID: req.PersonID, WorkspaceID: req.WorkspaceID,
		Title: req.Title, Channel: req.Channel, Kind: TaskKindInteraction,
		Visibility: TaskVisibilityUnlisted,
	})
	if err != nil {
		return nil, err
	}
	thread := threadFromTask(*task)
	return &thread, nil
}

// Promote makes a retained Interaction visible as ongoing work. Promotion is
// monotonic for automation: callers never demote a Thread implicitly.
func (w *WorkTimeline) Promote(ctx context.Context, tenantID, personID, threadID string) error {
	if w == nil || w.store == nil {
		return fmt.Errorf("work timeline is unavailable")
	}
	result, err := w.store.db.ExecContext(ctx, `UPDATE threads
		SET kind = ?, visibility = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ?
		  AND COALESCE(visibility, 'visible') != ?`,
		TaskKindWork, TaskVisibilityListed, time.Now().Unix(), normalizeTenant(tenantID),
		strings.TrimSpace(personID), strings.TrimSpace(threadID), TaskVisibilityArchived)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 1 {
		return nil
	}
	var exists int
	if err := w.store.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM threads WHERE tenant_id = ? AND person_id = ? AND id = ?
	)`, normalizeTenant(tenantID), strings.TrimSpace(personID), strings.TrimSpace(threadID)).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	return fmt.Errorf("thread not found: %s", threadID)
}

// Reopen is the explicit-control counterpart to Promote. It may restore an
// archived Thread because the user named the work they intend to continue;
// automatic evidence must keep using Promote so it cannot undo an archive.
func (w *WorkTimeline) Reopen(ctx context.Context, tenantID, personID, threadID string) error {
	if w == nil || w.store == nil {
		return fmt.Errorf("work timeline is unavailable")
	}
	result, err := w.store.db.ExecContext(ctx, `UPDATE threads
		SET kind = ?, visibility = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ?`,
		TaskKindWork, TaskVisibilityListed, time.Now().Unix(), normalizeTenant(tenantID),
		strings.TrimSpace(personID), strings.TrimSpace(threadID))
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return fmt.Errorf("thread not found: %s", threadID)
	}
	return nil
}

// Archive hides a Thread from ordinary presentation without resolving or
// cancelling any Run, approval, clarification, watcher, or queued work.
func (w *WorkTimeline) Archive(ctx context.Context, tenantID, personID, threadID string) error {
	if w == nil || w.store == nil {
		return fmt.Errorf("work timeline is unavailable")
	}
	now := time.Now().Unix()
	result, err := w.store.db.ExecContext(ctx, `UPDATE threads
		SET visibility = ?, updated_at = ?
		WHERE tenant_id = ? AND person_id = ? AND id = ?`,
		TaskVisibilityArchived, now, normalizeTenant(tenantID), strings.TrimSpace(personID), strings.TrimSpace(threadID))
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return fmt.Errorf("thread not found: %s", threadID)
	}
	return nil
}

// ErrAttentionPendingControl refuses to dismiss Attention that still carries a
// live control object. A pending approval, pending clarification, or active
// watcher is the person's own open decision; hiding it would strand the work
// instead of acknowledging it.
var ErrAttentionPendingControl = errors.New("attention cannot be dismissed while a pending approval, clarification, or watcher exists; answer, reject, or cancel it first")

// nonWorkToolNamesSQL lists the lifecycle, broker, and history tools whose
// ledger rows prove nothing about work: they steer a turn without touching
// the world.
const nonWorkToolNamesSQL = `('update_plan', 'finish_run', 'queue_user_input', 'work_select', 'work_search',
	'work_inspect', 'session_search', 'memory', 'clarify', 'set_delivery_target')`

// runWorkEvidenceSQL is the one definition of "this Run did work", written over
// the runs alias r: a multi-step plan, a dispatched non-read-only effect from a
// work tool, a control object, a deliberate continuation edge, or a handoff
// that carries next steps or changed files. Finalization promotion, the
// interrupted Attention gate, and interaction projection must agree on it.
//
// The plan clause requires MORE THAN ONE step: `update_plan` is meant for
// genuinely multi-step work, so a lone snapshotted step is not by itself proof
// of ongoing work. The ledger clause skips a `planned` row, which is a claimed
// call id that never reached dispatch and therefore touched nothing. A `failed`
// row still counts: a command that ran and exited non-zero (a failing build or
// test) is real work, and the ledger cannot currently distinguish that from a
// guardrail refusal without a durable effect-state column.
const runWorkEvidenceSQL = `(COALESCE(r.parent_run_id, '') != ''
	OR EXISTS (SELECT 1 FROM run_plan_steps s WHERE s.tenant_id = r.tenant_id AND s.run_id = r.id
	     GROUP BY s.plan_version HAVING COUNT(*) > 1)
	OR EXISTS (SELECT 1 FROM tool_ledger l WHERE l.tenant_id = r.tenant_id AND l.run_id = r.id
	     AND COALESCE(l.retry_class, '') != 'read_only' AND l.tool_name NOT IN ` + nonWorkToolNamesSQL + `
	     AND COALESCE(l.status, '') != 'planned')
	OR EXISTS (SELECT 1 FROM approval_requests a WHERE a.tenant_id = r.tenant_id AND a.run_id = r.id)
	OR EXISTS (SELECT 1 FROM clarify_requests c WHERE c.tenant_id = r.tenant_id AND c.run_id = r.id)
	OR EXISTS (SELECT 1 FROM external_watches w WHERE w.tenant_id = r.tenant_id AND w.run_id = r.id)
	OR EXISTS (SELECT 1 FROM task_handoffs h WHERE h.run_id = r.id
	     AND (COALESCE(h.next_steps_json, '') NOT IN ('', '[]', 'null')
	          OR COALESCE(h.changed_files_json, '') NOT IN ('', '[]', 'null'))))`

// resumableRunConditionSQL names, over the runs alias r, the parked Run that is
// current Attention: resumable, undismissed, unclaimed on both continuation
// edges, the latest Run of its Thread (a newer Run supersedes older parked
// state), and for an interrupted Run backed by work evidence. Attention, the
// settled work list, and dismissal must agree on it.
const resumableRunConditionSQL = `r.status IN ` + resumableRunStatusSQL + `
	AND COALESCE(r.attention_dismissed_at, 0) = 0
	AND COALESCE(r.resumed_by_run_id, '') = ''
	AND NOT EXISTS (SELECT 1 FROM runs child WHERE child.tenant_id = r.tenant_id AND child.parent_run_id = r.id)
	AND NOT EXISTS (SELECT 1 FROM runs newer WHERE newer.tenant_id = r.tenant_id AND newer.thread_id = r.thread_id
	     AND newer.id <> COALESCE(r.parent_run_id, '')
	     AND (newer.started_at > r.started_at OR (newer.started_at = r.started_at AND newer.rowid > r.rowid)))
	AND (r.status != 'interrupted' OR ` + runWorkEvidenceSQL + `)`

// runHasWorkEvidenceTx reports whether runID carries durable work evidence.
func runHasWorkEvidenceTx(ctx context.Context, tx *sql.Tx, tenantID, runID string) (bool, error) {
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM runs r WHERE r.tenant_id = ? AND r.id = ? AND `+runWorkEvidenceSQL+`
	)`, normalizeTenant(tenantID), strings.TrimSpace(runID)).Scan(&found); err != nil {
		return false, err
	}
	return found != 0, nil
}

// runStatusPromotesThread names the terminal Run states that list a Thread on
// their own: the Run stopped because the person must act, so the work is
// ongoing regardless of what the Run touched.
func runStatusPromotesThread(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "waiting_user", "verification_partial", "blocked":
		return true
	}
	return false
}

func execPromoteThreadForRunTx(ctx context.Context, tx *sql.Tx, tenantID, runID string) (int64, error) {
	result, err := tx.ExecContext(ctx, `UPDATE threads
		SET kind = ?, visibility = ?, updated_at = ?
		WHERE tenant_id = ?
		  AND visibility != ?
		  AND id = (SELECT thread_id FROM runs WHERE tenant_id = ? AND id = ?)`,
		TaskKindWork, TaskVisibilityListed, time.Now().Unix(), normalizeTenant(tenantID),
		TaskVisibilityArchived, normalizeTenant(tenantID), strings.TrimSpace(runID))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func promoteThreadForRunTx(ctx context.Context, tx *sql.Tx, tenantID, runID string) error {
	rows, err := execPromoteThreadForRunTx(ctx, tx, tenantID, runID)
	if err != nil {
		return err
	}
	if rows == 1 {
		return nil
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM runs r JOIN threads t ON t.id = r.thread_id AND t.tenant_id = r.tenant_id
		 WHERE r.tenant_id = ? AND r.id = ?
	)`, normalizeTenant(tenantID), strings.TrimSpace(runID)).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	return fmt.Errorf("run thread not found: %s", runID)
}

// promoteThreadForControlObjectTx lists a Run's Thread the moment an approval,
// clarification, or watcher is created for it, so the work list reflects the
// evidence before finalization. A control object may name a Run this store
// never recorded; that leaves nothing to promote and is not an error.
func promoteThreadForControlObjectTx(ctx context.Context, tx *sql.Tx, tenantID, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	_, err := execPromoteThreadForRunTx(ctx, tx, tenantID, runID)
	return err
}

// shouldPromoteThreadAtFinalization decides whether one finished Run proves
// ongoing work. A Run parked for the person and a handoff with next steps or
// changed files promote outright; an interrupted or completed Run promotes
// only with durable work evidence, so a crashed or answered tool-free turn
// never inflates the work list.
func shouldPromoteThreadAtFinalization(ctx context.Context, tx *sql.Tx, input RunFinalization) (bool, error) {
	if runStatusPromotesThread(input.RunStatus) {
		return true, nil
	}
	if len(input.NextSteps) > 0 || len(input.Handoff.NextSteps) > 0 || len(input.Handoff.ChangedFiles) > 0 {
		return true, nil
	}
	return runHasWorkEvidenceTx(ctx, tx, input.Identity.TenantID, input.RunID)
}

const (
	// attentionDefaultLimit is the page a caller that names no limit receives.
	attentionDefaultLimit = 20
	// attentionMaxPageLimit bounds the rows ONE Attention query returns. It is
	// not a bound on the person's Attention: AttentionPage reports the true
	// total and reaches every item through offset.
	attentionMaxPageLimit = 500
)

// attentionRankedSQL ranks every current Attention signal for one person —
// an executing Run, a pending approval or clarification, a live watcher, and a
// parked resumable Run — and keeps one row per exact Run. The caller appends
// either a COUNT over the ranked set or a page of it; both pass attentionArgs.
const attentionRankedSQL = `WITH signals AS (
	SELECT r.thread_id, r.id AS run_id, 'active' AS activity, 1 AS priority, r.started_at AS activity_at
	  FROM runs r
	 WHERE r.tenant_id = ? AND r.person_id = ? AND r.status = 'running'
	UNION ALL
	SELECT r.thread_id, r.id, 'needs_attention', 2, COALESCE(a.updated_at, a.created_at)
	  FROM approval_requests a JOIN runs r ON r.id = a.run_id AND r.thread_id = a.thread_id
	 WHERE a.tenant_id = ? AND a.person_id = ? AND a.status = 'pending' AND COALESCE(r.attention_dismissed_at, 0) = 0
	UNION ALL
	SELECT r.thread_id, r.id, 'needs_attention', 2, COALESCE(c.updated_at, c.created_at)
	  FROM clarify_requests c JOIN runs r ON r.id = c.run_id AND r.thread_id = c.thread_id
	 WHERE c.tenant_id = ? AND c.person_id = ? AND c.status = 'pending' AND COALESCE(r.attention_dismissed_at, 0) = 0
	UNION ALL
	SELECT r.thread_id, r.id, 'monitoring', 3, w.updated_at
	  FROM external_watches w JOIN runs r ON r.id = w.run_id AND r.thread_id = w.thread_id
	 WHERE w.tenant_id = ? AND w.person_id = ? AND w.status IN ('pending', 'running') AND COALESCE(r.attention_dismissed_at, 0) = 0
	UNION ALL
	SELECT r.thread_id, r.id, 'resumable', 4, r.started_at
	  FROM runs r
	 WHERE r.tenant_id = ? AND r.person_id = ? AND ` + resumableRunConditionSQL + `
), ranked AS (
	SELECT thread_id, run_id, activity, priority, activity_at,
	       ROW_NUMBER() OVER (PARTITION BY run_id ORDER BY priority, activity_at DESC, thread_id DESC) AS rank
	  FROM signals
)
`

const attentionSelectSQL = `SELECT t.id, t.tenant_id, t.person_id, COALESCE(t.workspace_id, ''),
       COALESCE(t.kind, 'work'), COALESCE(t.visibility, 'visible'), t.title,
       COALESCE(t.summary, ''), COALESCE(t.pinned, 0), t.created_at,
       t.updated_at, COALESCE(t.last_activity_at, t.updated_at), ranked.run_id,
       COALESCE(r.input_summary, ''), r.status, COALESCE(r.channel, ''), ranked.activity`

const attentionFromSQL = `
  FROM ranked JOIN threads t ON t.id = ranked.thread_id
  JOIN runs r ON r.tenant_id = t.tenant_id AND r.id = ranked.run_id
 WHERE ranked.rank = 1 AND COALESCE(t.visibility, 'visible') NOT IN ('hidden', 'archived')`

// attentionOrderSQL is a total order over the ranked set (run_id is unique
// within it), so counting, paging, and the ordinals a client binds to a page
// all agree. The two placeholders are the preferred channel.
const attentionOrderSQL = `
 ORDER BY COALESCE(t.pinned, 0) DESC, ranked.priority,
          CASE WHEN ? != '' AND r.channel = ? THEN 0 ELSE 1 END,
          ranked.activity_at DESC, ranked.run_id DESC`

func attentionArgs(tenantID, personID string) []any {
	tenant, person := normalizeTenant(tenantID), strings.TrimSpace(personID)
	return []any{tenant, person, tenant, person, tenant, person, tenant, person, tenant, person}
}

func normalizeAttentionLimit(limit int) int {
	if limit <= 0 {
		return attentionDefaultLimit
	}
	if limit > attentionMaxPageLimit {
		return attentionMaxPageLimit
	}
	return limit
}

// Attention derives actionable work from live control facts. A Thread status
// is neither read nor written, so stale aggregate state cannot keep history
// artificially open.
func (w *WorkTimeline) Attention(ctx context.Context, tenantID, personID string, limit int) ([]AttentionItem, error) {
	return w.AttentionForChannel(ctx, tenantID, personID, "", limit)
}

// AttentionForChannel is Attention with same-channel preference: within one
// pinned and priority band, Runs whose channel equals preferChannel sort
// before other channels, then recency decides. An empty preferChannel is plain
// Attention. It reads the first page only, bounded by attentionMaxPageLimit; a
// caller that must reach further, or that must report how much work exists,
// uses AttentionPage.
func (w *WorkTimeline) AttentionForChannel(ctx context.Context, tenantID, personID, preferChannel string, limit int) ([]AttentionItem, error) {
	if w == nil || w.store == nil {
		return nil, fmt.Errorf("work timeline is unavailable")
	}
	return w.attentionItems(ctx, tenantID, personID, preferChannel, normalizeAttentionLimit(limit), 0)
}

// AttentionPage is the paged Attention read: items is exactly the requested
// page in the order AttentionForChannel ranks, and total is the person's true
// Attention count over that same ranked set. A client can therefore render one
// page, report how much work remains, and bind ordinals to what it drew.
func (w *WorkTimeline) AttentionPage(ctx context.Context, tenantID, personID, preferChannel string, limit, offset int) ([]AttentionItem, int, error) {
	if w == nil || w.store == nil {
		return nil, 0, fmt.Errorf("work timeline is unavailable")
	}
	if offset < 0 {
		offset = 0
	}
	total, err := w.attentionTotal(ctx, tenantID, personID)
	if err != nil {
		return nil, 0, err
	}
	if offset >= total {
		return nil, total, nil
	}
	items, err := w.attentionItems(ctx, tenantID, personID, preferChannel, normalizeAttentionLimit(limit), offset)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// attentionTotal counts the ranked Attention set without materializing it, so
// statistics and page hints never report a page bound as a total.
func (w *WorkTimeline) attentionTotal(ctx context.Context, tenantID, personID string) (int, error) {
	if w == nil || w.store == nil {
		return 0, fmt.Errorf("work timeline is unavailable")
	}
	var total int
	err := w.store.db.QueryRowContext(ctx, attentionRankedSQL+`SELECT COUNT(*)`+attentionFromSQL,
		attentionArgs(tenantID, personID)...).Scan(&total)
	return total, err
}

func (w *WorkTimeline) attentionItems(ctx context.Context, tenantID, personID, preferChannel string, limit, offset int) ([]AttentionItem, error) {
	preferChannel = strings.TrimSpace(preferChannel)
	args := append(attentionArgs(tenantID, personID), preferChannel, preferChannel, limit, offset)
	rows, err := w.store.db.QueryContext(ctx,
		attentionRankedSQL+attentionSelectSQL+attentionFromSQL+attentionOrderSQL+` LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []AttentionItem
	for rows.Next() {
		var item AttentionItem
		var pinned int
		var created, updated, activity int64
		if err := rows.Scan(&item.Thread.ID, &item.Thread.TenantID, &item.Thread.PersonID,
			&item.Thread.WorkspaceID, &item.Thread.Kind, &item.Thread.Visibility,
			&item.Thread.Title, &item.Thread.Summary, &pinned, &created, &updated,
			&activity, &item.RunID, &item.RunSummary, &item.RunStatus, &item.Channel, &item.Activity); err != nil {
			return nil, err
		}
		item.Thread.Kind = normalizeThreadKind(item.Thread.Kind)
		item.Thread.Visibility = normalizeThreadVisibility(item.Thread.Visibility)
		item.Thread.Pinned = pinned != 0
		item.Thread.CreatedAt = time.Unix(created, 0)
		item.Thread.UpdatedAt = time.Unix(updated, 0)
		item.Thread.LastActivityAt = time.Unix(activity, 0)
		items = append(items, item)
	}
	return items, rows.Err()
}

// DismissAttention acknowledges a Thread's currently resumable Runs without
// closing, succeeding, cancelling, or otherwise rewriting those Runs. It
// refuses while a Run is executing or while a targeted Run still owns a
// pending approval, pending clarification, or live watcher: those are the
// person's open decisions and must be answered, rejected, or cancelled, never
// hidden.
func (w *WorkTimeline) DismissAttention(ctx context.Context, tenantID, personID, threadID string) (int, error) {
	if w == nil || w.store == nil {
		return 0, fmt.Errorf("work timeline is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	threadID = strings.TrimSpace(threadID)
	var running int
	if err := w.store.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM runs WHERE tenant_id = ? AND person_id = ? AND thread_id = ? AND status = 'running'
	)`, tenantID, personID, threadID).Scan(&running); err != nil {
		return 0, err
	}
	if running != 0 {
		return 0, ErrTaskHasLiveWork
	}
	if pending, err := w.pendingControlExists(ctx, tenantID, personID, threadID, ""); err != nil {
		return 0, err
	} else if pending {
		return 0, fmt.Errorf("thread %s: %w", threadID, ErrAttentionPendingControl)
	}
	now := time.Now().Unix()
	result, err := w.store.db.ExecContext(ctx, `UPDATE runs
		SET attention_dismissed_at = ?, attention_dismissed_by = ?
		WHERE id IN (SELECT r.id FROM runs r
		             WHERE r.tenant_id = ? AND r.person_id = ? AND r.thread_id = ? AND `+resumableRunConditionSQL+`)`,
		now, personID, tenantID, personID, threadID)
	if err != nil {
		return 0, err
	}
	changed, err := result.RowsAffected()
	return int(changed), err
}

// DismissAttentionRun acknowledges one exact Attention row. Numbered UI
// cards use this form so two unresolved Runs in one Thread remain independently
// actionable; Thread-scoped controls may deliberately use DismissAttention.
func (w *WorkTimeline) DismissAttentionRun(ctx context.Context, tenantID, personID, threadID, runID string) (bool, error) {
	if w == nil || w.store == nil {
		return false, fmt.Errorf("work timeline is unavailable")
	}
	tenantID = normalizeTenant(tenantID)
	personID = strings.TrimSpace(personID)
	threadID = strings.TrimSpace(threadID)
	runID = strings.TrimSpace(runID)
	if personID == "" || threadID == "" || runID == "" {
		return false, fmt.Errorf("person, thread, and run ids are required")
	}
	run, err := w.store.GetRun(ctx, tenantID, runID)
	if err != nil {
		return false, err
	}
	if run == nil || run.PersonID != personID || run.TaskID != threadID {
		return false, nil
	}
	if run.Status == "running" {
		return false, ErrTaskHasLiveWork
	}
	if pending, err := w.pendingControlExists(ctx, tenantID, personID, threadID, runID); err != nil {
		return false, err
	} else if pending {
		return false, fmt.Errorf("run %s: %w", runID, ErrAttentionPendingControl)
	}
	now := time.Now().Unix()
	result, err := w.store.db.ExecContext(ctx, `UPDATE runs
		SET attention_dismissed_at = ?, attention_dismissed_by = ?
		WHERE id IN (SELECT r.id FROM runs r
		             WHERE r.tenant_id = ? AND r.person_id = ? AND r.thread_id = ? AND r.id = ?
		               AND `+resumableRunConditionSQL+`)`,
		now, personID, tenantID, personID, threadID, runID)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// pendingControlExists reports whether an undismissed Run of the Thread, or
// the one exact Run when runID is set, still owns a pending approval, pending
// clarification, or live watcher.
func (w *WorkTimeline) pendingControlExists(ctx context.Context, tenantID, personID, threadID, runID string) (bool, error) {
	var found int
	if err := w.store.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM runs r
		 WHERE r.tenant_id = ? AND r.person_id = ? AND r.thread_id = ?
		   AND (? = '' OR r.id = ?)
		   AND COALESCE(r.attention_dismissed_at, 0) = 0
		   AND (EXISTS (SELECT 1 FROM approval_requests a
		                 WHERE a.tenant_id = r.tenant_id AND a.run_id = r.id AND a.status = 'pending')
		        OR EXISTS (SELECT 1 FROM clarify_requests c
		                 WHERE c.tenant_id = r.tenant_id AND c.run_id = r.id AND c.status = 'pending')
		        OR EXISTS (SELECT 1 FROM external_watches x
		                 WHERE x.tenant_id = r.tenant_id AND x.run_id = r.id AND x.status IN ('pending', 'running')))
	)`, tenantID, personID, threadID, runID, runID).Scan(&found); err != nil {
		return false, err
	}
	return found != 0, nil
}

// List returns work-list presentation only. Unlisted interactions remain
// searchable but never inflate the ordinary list.
func (w *WorkTimeline) List(ctx context.Context, tenantID, personID string, query ThreadQuery) (ThreadPage, error) {
	if w == nil || w.store == nil {
		return ThreadPage{}, fmt.Errorf("work timeline is unavailable")
	}
	query = normalizeThreadQuery(query)
	where := strings.Builder{}
	where.WriteString(` WHERE tenant_id = ? AND person_id = ?`)
	args := []any{normalizeTenant(tenantID), strings.TrimSpace(personID)}
	switch query.View {
	case ThreadViewListed:
		where.WriteString(` AND kind IN ('work', 'recurring') AND visibility IN ('listed', 'visible')`)
	case ThreadViewSettled:
		where.WriteString(` AND kind IN ('work', 'recurring') AND visibility IN ('listed', 'visible')
			AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.thread_id = threads.id AND r.status = 'running')
			AND NOT EXISTS (SELECT 1 FROM approval_requests a JOIN runs r ON r.id = a.run_id
				WHERE a.thread_id = threads.id AND a.status = 'pending' AND COALESCE(r.attention_dismissed_at, 0) = 0)
			AND NOT EXISTS (SELECT 1 FROM clarify_requests c JOIN runs r ON r.id = c.run_id
				WHERE c.thread_id = threads.id AND c.status = 'pending' AND COALESCE(r.attention_dismissed_at, 0) = 0)
			AND NOT EXISTS (SELECT 1 FROM external_watches w JOIN runs r ON r.id = w.run_id
				WHERE w.thread_id = threads.id AND w.status IN ('pending', 'running') AND COALESCE(r.attention_dismissed_at, 0) = 0)
			AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.tenant_id = threads.tenant_id AND r.thread_id = threads.id
				AND ` + resumableRunConditionSQL + `)`)
	case ThreadViewArchived:
		where.WriteString(` AND visibility = 'archived'`)
	case ThreadViewAll:
	default:
		return ThreadPage{}, fmt.Errorf("unsupported thread view: %s", query.View)
	}
	appendThreadFilters(&where, &args, query)

	page := ThreadPage{Limit: query.Limit, Offset: query.Offset}
	if err := w.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads`+where.String(), args...).Scan(&page.Total); err != nil {
		return ThreadPage{}, err
	}
	selectArgs := append(append([]any(nil), args...), query.Limit, query.Offset)
	rows, err := w.store.db.QueryContext(ctx, threadSelectSQL+where.String()+`
		ORDER BY COALESCE(pinned, 0) DESC, COALESCE(last_activity_at, updated_at) DESC, id ASC
		LIMIT ? OFFSET ?`, selectArgs...)
	if err != nil {
		return ThreadPage{}, err
	}
	defer rows.Close()
	page.Threads, err = scanThreads(rows)
	return page, err
}

// Search covers all retained history, including unlisted interactions and
// archived work. Visibility affects presentation, not recall.
func (w *WorkTimeline) Search(ctx context.Context, tenantID, personID, query string, limit int) ([]Thread, error) {
	if w == nil || w.store == nil {
		return nil, fmt.Errorf("work timeline is unavailable")
	}
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	where := strings.Builder{}
	where.WriteString(` WHERE tenant_id = ? AND person_id = ?`)
	args := []any{normalizeTenant(tenantID), strings.TrimSpace(personID)}
	for _, term := range terms {
		where.WriteString(` AND (
			instr(lower(COALESCE(threads.id, '') || char(10) || COALESCE(threads.title, '') || char(10) ||
				COALESCE(threads.workspace_id, '') || char(10) || COALESCE(threads.summary, '')), ?) > 0
			OR EXISTS (SELECT 1 FROM runs r WHERE r.tenant_id = threads.tenant_id AND r.thread_id = threads.id
				AND instr(lower(COALESCE(r.input_summary, '')), ?) > 0)
			OR EXISTS (SELECT 1 FROM task_handoffs h WHERE h.thread_id = threads.id
				AND instr(lower(COALESCE(h.summary, '') || char(10) || COALESCE(h.changed_files_json, '')), ?) > 0)
		)`)
		args = append(args, term, term, term)
	}
	args = append(args, limit)
	rows, err := w.store.db.QueryContext(ctx, threadSelectSQL+where.String()+`
		ORDER BY COALESCE(last_activity_at, updated_at) DESC, id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanThreads(rows)
}

const threadSelectSQL = `SELECT id, tenant_id, person_id, COALESCE(workspace_id, ''),
	COALESCE(kind, 'work'), COALESCE(visibility, 'visible'), title,
	COALESCE(summary, ''), COALESCE(pinned, 0), created_at, updated_at,
	COALESCE(last_activity_at, updated_at) FROM threads`

func normalizeThreadQuery(query ThreadQuery) ThreadQuery {
	query.View = strings.ToLower(strings.TrimSpace(query.View))
	if query.View == "" {
		query.View = ThreadViewListed
	}
	if query.Limit <= 0 {
		query.Limit = 20
	}
	if query.Limit > 200 {
		query.Limit = 200
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	return query
}

func appendThreadFilters(where *strings.Builder, args *[]any, query ThreadQuery) {
	if workspace := strings.TrimSpace(query.WorkspaceID); workspace != "" {
		where.WriteString(` AND COALESCE(workspace_id, '') = ?`)
		*args = append(*args, workspace)
	}
	for _, term := range strings.Fields(strings.ToLower(strings.TrimSpace(query.Keyword))) {
		where.WriteString(` AND (instr(lower(COALESCE(title, '') || char(10) || COALESCE(summary, '')), ?) > 0
			OR EXISTS (SELECT 1 FROM runs r WHERE r.thread_id = threads.id AND instr(lower(COALESCE(r.input_summary, '')), ?) > 0))`)
		*args = append(*args, term, term)
	}
}

func scanThreads(rows *sql.Rows) ([]Thread, error) {
	var threads []Thread
	for rows.Next() {
		var thread Thread
		var pinned int
		var created, updated, activity int64
		if err := rows.Scan(&thread.ID, &thread.TenantID, &thread.PersonID, &thread.WorkspaceID,
			&thread.Kind, &thread.Visibility, &thread.Title, &thread.Summary, &pinned,
			&created, &updated, &activity); err != nil {
			return nil, err
		}
		thread.Kind = normalizeThreadKind(thread.Kind)
		thread.Visibility = normalizeThreadVisibility(thread.Visibility)
		thread.Pinned = pinned != 0
		thread.CreatedAt = time.Unix(created, 0)
		thread.UpdatedAt = time.Unix(updated, 0)
		thread.LastActivityAt = time.Unix(activity, 0)
		threads = append(threads, thread)
	}
	return threads, rows.Err()
}

func normalizeThreadKind(kind string) string {
	switch normalizeTaskKind(kind) {
	case TaskKindInteraction, TaskKindInbox:
		return ThreadKindInteraction
	case TaskKindRecurring:
		return ThreadKindRecurring
	default:
		return ThreadKindWork
	}
}

func normalizeThreadVisibility(visibility string) string {
	switch normalizeTaskVisibility(visibility) {
	case TaskVisibilityArchived:
		return ThreadVisibilityArchived
	case TaskVisibilityUnlisted:
		return ThreadVisibilityUnlisted
	default:
		return ThreadVisibilityListed
	}
}

// legacyTask is temporary glue while execution callers migrate from Task to
// Thread. It is intentionally private so new gateway surfaces cannot acquire a
// second lifecycle contract from the legacy type.
func (t Thread) legacyTask() *Task {
	return &Task{
		ID: t.ID, TenantID: t.TenantID, PersonID: t.PersonID,
		WorkspaceID: t.WorkspaceID, Title: t.Title, Kind: t.Kind,
		Visibility: t.Visibility, Pinned: t.Pinned, CurrentSummary: t.Summary,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, LastActivityAt: t.LastActivityAt,
	}
}

func threadFromTask(task Task) Thread {
	return Thread{
		ID: task.ID, TenantID: task.TenantID, PersonID: task.PersonID,
		WorkspaceID: task.WorkspaceID, Kind: normalizeThreadKind(task.Kind),
		Visibility: normalizeThreadVisibility(task.Visibility), Title: task.Title,
		Summary: task.CurrentSummary, Pinned: task.Pinned, CreatedAt: task.CreatedAt,
		UpdatedAt: task.UpdatedAt, LastActivityAt: task.LastActivityAt,
	}
}
