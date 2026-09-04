package control

import (
	"context"
	"encoding/json"
	"time"
)

// TaskCard is the compact recall "label card" for one task: title + current
// summary + the latest handoff's summary/changed files, joined in a single
// bounded read-only query. It backs the gateway's automatic semantic-recall
// slice (Work Timeline P2, docs/work-timeline.md "Semantic recall").
//
// Cards are queried live from control.db instead of being mirrored into the
// FTS store: control.db is the source of truth for task state, the per-person
// task set is small and already indexed by (tenant, person, updated_at), and a
// live query can never go stale — a duplicate FTS copy would need write hooks
// on every task/handoff mutation for marginal ranking gain at this scale. The
// v2 embedding tier plugs in behind the recall-source interface in
// gateway/httpapi without touching this query.
type TaskCard struct {
	TaskID         string
	WorkspaceID    string
	Title          string
	Status         string
	Summary        string
	HandoffSummary string
	ChangedFiles   []string
	UpdatedAt      time.Time
}

// ListTaskCards returns the person's most recently updated task cards, newest
// first, excluding archived tasks (abandoned work should not haunt recall).
// Status is the Attention-derived Thread projection ('settled' when nothing is
// executing, pending, or resumable). Read-only and bounded: one JOIN query,
// limit capped at 50.
func (s *Store) ListTaskCards(ctx context.Context, tenantID, personID string, limit int) ([]TaskCard, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, COALESCE(t.workspace_id, ''), t.title,
		        `+threadDerivedStatusSQL(ThreadActivitySettled)+`,
		        COALESCE(t.summary, ''), t.updated_at,
		        COALESCE(h.summary, ''), COALESCE(h.changed_files_json, '[]')
		 FROM threads t
		 LEFT JOIN task_handoffs h ON h.id = (
		     SELECT id FROM task_handoffs
		     WHERE thread_id = t.id
		     ORDER BY created_at DESC, rowid DESC LIMIT 1
		 )
		 WHERE t.tenant_id = ? AND t.person_id = ?
		   AND COALESCE(t.visibility, 'listed') != 'archived'
		 ORDER BY t.updated_at DESC LIMIT ?`,
		normalizeTenant(tenantID), personID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskCard
	for rows.Next() {
		var card TaskCard
		var updated int64
		var filesJSON string
		if err := rows.Scan(&card.TaskID, &card.WorkspaceID, &card.Title, &card.Status,
			&card.Summary, &updated, &card.HandoffSummary, &filesJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(filesJSON), &card.ChangedFiles)
		card.UpdatedAt = time.Unix(updated, 0)
		out = append(out, card)
	}
	return out, rows.Err()
}
