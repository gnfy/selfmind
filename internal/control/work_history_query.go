package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// WorkHistoryQuery selects observed work in [Since, Until), not Threads whose
// latest update happens to fall there. Cursor freezes the insertion boundary;
// reading a page never selects, resumes or grants authority to a Run.
type WorkHistoryQuery struct {
	Since, Until                               time.Time
	WorkspaceID, Keyword, Cursor, ExcludeRunID string
	Limit                                      int
}

type WorkHistoryPage struct {
	Runs       []Run
	HasMore    bool
	NextCursor string
}

type workHistoryCursor struct {
	Before  int64  `json:"before"`
	ID      string `json:"id"`
	Ceiling int64  `json:"ceiling"`
	Query   string `json:"query"`
}

func (w *WorkTimeline) QueryHistory(ctx context.Context, tenant, person string, q WorkHistoryQuery) (WorkHistoryPage, error) {
	page := WorkHistoryPage{}
	if w == nil || w.store == nil {
		return page, fmt.Errorf("work history is unavailable")
	}
	if strings.TrimSpace(person) == "" || q.Since.IsZero() || q.Until.IsZero() || !q.Since.Before(q.Until) {
		return page, fmt.Errorf("person and a nonempty explicit time interval are required")
	}
	if q.Limit <= 0 {
		q.Limit = 8
	}
	if q.Limit > 50 {
		q.Limit = 50
	}
	tenant = normalizeTenant(tenant)
	signature := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s\x00%s\x00%s", tenant, person, q.Since.Unix(), q.Until.Unix(), q.WorkspaceID, q.Keyword, q.ExcludeRunID))))
	tx, err := w.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	cursor := workHistoryCursor{Query: signature}
	if q.Cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || len(data) > 1024 || json.Unmarshal(data, &cursor) != nil || cursor.Query != signature || cursor.ID == "" || cursor.Ceiling <= 0 {
			return page, fmt.Errorf("history cursor does not match this query")
		}
	} else if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid),0) FROM runs WHERE tenant_id=? AND person_id=?`, tenant, person).Scan(&cursor.Ceiling); err != nil {
		return page, err
	}
	where := ` WHERE r.tenant_id=? AND r.person_id=? AND r.rowid<=? AND r.id<>?
	 AND (r.started_at>=? AND r.started_at<? OR r.finished_at>=? AND r.finished_at<?
	 OR EXISTS (SELECT 1 FROM task_events e WHERE e.run_id=r.id AND e.created_at>=? AND e.created_at<?
	 AND e.type IN ('run.started','tool.started','run.outcome','run.finished','run.interrupted','turn.completed')))`
	args := []any{tenant, person, cursor.Ceiling, q.ExcludeRunID, q.Since.Unix(), q.Until.Unix(), q.Since.Unix(), q.Until.Unix(), q.Since.Unix(), q.Until.Unix()}
	if q.WorkspaceID != "" {
		where += ` AND COALESCE(r.workspace_id,'')=?`
		args = append(args, q.WorkspaceID)
	}
	for _, term := range strings.Fields(strings.ToLower(q.Keyword)) {
		where += ` AND (instr(lower(COALESCE(t.title,'') || char(10) || COALESCE(r.input_summary,'')),?)>0
		 OR EXISTS (SELECT 1 FROM task_handoffs h WHERE h.run_id=r.id AND h.thread_id=r.thread_id
		 AND instr(lower(COALESCE(h.summary,'') || char(10) || COALESCE(h.changed_files_json,'')),?)>0))`
		args = append(args, term, term)
	}
	if cursor.ID != "" {
		where += ` AND (r.started_at<? OR r.started_at=? AND r.id<?)`
		args = append(args, cursor.Before, cursor.Before, cursor.ID)
	}
	args = append(args, q.Limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT r.id,r.thread_id,r.tenant_id,r.person_id,COALESCE(r.workspace_id,''),r.channel,
	 COALESCE(r.input_summary,''),COALESCE(r.resumes_run_id,''),r.status,r.started_at,r.finished_at
	 FROM runs r JOIN threads t ON t.id=r.thread_id AND t.tenant_id=r.tenant_id AND t.person_id=r.person_id`+where+` ORDER BY r.started_at DESC,r.id DESC LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var run Run
		var started int64
		var finished sql.NullInt64
		if err = rows.Scan(&run.ID, &run.TaskID, &run.TenantID, &run.PersonID, &run.WorkspaceID, &run.Channel, &run.InputSummary, &run.ResumesRunID, &run.Status, &started, &finished); err != nil {
			return page, err
		}
		run.StartedAt = time.Unix(started, 0)
		if finished.Valid {
			t := time.Unix(finished.Int64, 0)
			run.FinishedAt = &t
		}
		page.Runs = append(page.Runs, run)
	}
	if err = rows.Err(); err != nil {
		return page, err
	}
	if len(page.Runs) > q.Limit {
		page.HasMore = true
		page.Runs = page.Runs[:q.Limit]
		last := page.Runs[len(page.Runs)-1]
		cursor.Before, cursor.ID = last.StartedAt.Unix(), last.ID
		data, _ := json.Marshal(cursor)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return page, nil
}
