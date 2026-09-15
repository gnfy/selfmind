package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ExternalWatchGroupAll = "all"
	ExternalWatchGroupAny = "any"
)

type ExternalWatchGroup struct {
	ID            string
	TenantID      string
	PersonID      string
	TaskID        string
	RunID         string
	GroupKey      string
	Mode          string
	ExpectedCount int
	Status        string
	WinnerWatchID string
}

type ExternalWatchGroupResolution struct {
	Terminal bool
	Won      bool
	Status   string
	Group    ExternalWatchGroup
}

func (s *Store) ResolveOrCreateExternalWatchGroup(ctx context.Context, tenantID, personID, taskID, runID, key, mode string, expected int) (*ExternalWatchGroup, error) {
	tenantID = normalizeTenant(tenantID)
	personID, taskID, runID, key = strings.TrimSpace(personID), strings.TrimSpace(taskID), strings.TrimSpace(runID), strings.TrimSpace(key)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = ExternalWatchGroupAll
	}
	if personID == "" || taskID == "" || runID == "" || key == "" {
		return nil, fmt.Errorf("watch group requires person, task, run, and group key")
	}
	if mode != ExternalWatchGroupAll && mode != ExternalWatchGroupAny {
		return nil, fmt.Errorf("watch group mode must be all or any")
	}
	if expected < 2 || expected > 8 {
		return nil, fmt.Errorf("watch group size must be between 2 and 8")
	}
	now := time.Now().Unix()
	id := "watchgroup_" + uuid.NewString()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO external_watch_groups
		(id, tenant_id, person_id, thread_id, run_id, group_key, mode, expected_count, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)
		ON CONFLICT(tenant_id, run_id, group_key) DO NOTHING`,
		id, tenantID, personID, taskID, runID, key, mode, expected, now, now); err != nil {
		return nil, err
	}
	group, err := s.externalWatchGroupByKey(ctx, tenantID, runID, key)
	if err != nil {
		return nil, err
	}
	if group.PersonID != personID || group.TaskID != taskID || group.Mode != mode || group.ExpectedCount != expected {
		return nil, fmt.Errorf("watch group %q was already declared with a different owner or contract", key)
	}
	if group.Status != ExternalWatchPending {
		return nil, fmt.Errorf("watch group %q is already terminal", key)
	}
	var members int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_watches WHERE tenant_id=? AND wait_group_id=?`, tenantID, group.ID).Scan(&members); err != nil {
		return nil, err
	}
	if members >= expected {
		return nil, fmt.Errorf("watch group %q already has its declared %d members", key, expected)
	}
	return group, nil
}

func (s *Store) externalWatchGroupByKey(ctx context.Context, tenantID, runID, key string) (*ExternalWatchGroup, error) {
	var group ExternalWatchGroup
	err := s.db.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, thread_id, run_id, group_key, mode,
		expected_count, status, COALESCE(winner_watch_id, '') FROM external_watch_groups
		WHERE tenant_id=? AND run_id=? AND group_key=?`, tenantID, runID, key).Scan(
		&group.ID, &group.TenantID, &group.PersonID, &group.TaskID, &group.RunID, &group.GroupKey,
		&group.Mode, &group.ExpectedCount, &group.Status, &group.WinnerWatchID)
	if err != nil {
		return nil, err
	}
	return &group, nil
}

func (s *Store) ResolveExternalWatchGroup(ctx context.Context, tenantID, groupID, triggerWatchID string) (ExternalWatchGroupResolution, error) {
	resolution := ExternalWatchGroupResolution{}
	if strings.TrimSpace(groupID) == "" {
		return resolution, nil
	}
	tenantID = normalizeTenant(tenantID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return resolution, err
	}
	defer tx.Rollback()
	var group ExternalWatchGroup
	if err := tx.QueryRowContext(ctx, `SELECT id, tenant_id, person_id, thread_id, run_id, group_key, mode,
		expected_count, status, COALESCE(winner_watch_id, '') FROM external_watch_groups
		WHERE tenant_id=? AND id=?`, tenantID, groupID).Scan(
		&group.ID, &group.TenantID, &group.PersonID, &group.TaskID, &group.RunID, &group.GroupKey,
		&group.Mode, &group.ExpectedCount, &group.Status, &group.WinnerWatchID); err != nil {
		if err == sql.ErrNoRows {
			return resolution, nil
		}
		return resolution, err
	}
	resolution.Group = group
	if group.Status != ExternalWatchPending {
		resolution.Terminal = true
		resolution.Status = group.Status
		return resolution, tx.Commit()
	}
	var registered, active, succeeded, failed int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status IN ('pending','running') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status='succeeded' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN status IN ('failed','timed_out','blocked_environment','cancelled') THEN 1 ELSE 0 END),0)
		FROM external_watches WHERE tenant_id=? AND wait_group_id=?`, tenantID, groupID).Scan(&registered, &active, &succeeded, &failed); err != nil {
		return resolution, err
	}
	// New groups cannot settle while the creating run is still registering
	// members. If it leaves an incomplete contract, close the CHECK, not the
	// external operation. Older contracts never acquire new continuation rights.
	var modern, deadline int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(json_extract(preflight_receipt_json,'$.version')),0), COALESCE(MIN(timeout_at),0) FROM external_watches WHERE tenant_id=? AND wait_group_id=?`, tenantID, groupID).Scan(&modern, &deadline); err != nil {
		return resolution, err
	}
	status := ""
	if modern >= ExternalWatchContinuationReceiptVersion {
		var runStatus string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE tenant_id=? AND id=?`, tenantID, group.RunID).Scan(&runStatus); err != nil {
			return resolution, err
		}
		if runStatus == "running" && deadline > time.Now().Unix() {
			return resolution, tx.Commit()
		}
		if registered < group.ExpectedCount {
			status = ExternalWatchBlocked
		}
	}
	if status != "" {
		// An incomplete group is not a failed business operation.
	} else if group.Mode == ExternalWatchGroupAny {
		if succeeded > 0 {
			status = ExternalWatchSucceeded
		} else if registered >= group.ExpectedCount && active == 0 && failed == registered {
			status = ExternalWatchFailed
		}
	} else {
		if failed > 0 {
			status = ExternalWatchFailed
		} else if registered >= group.ExpectedCount && succeeded == group.ExpectedCount {
			status = ExternalWatchSucceeded
		}
	}
	if status == "" {
		return resolution, tx.Commit()
	}
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `UPDATE external_watch_groups SET status=?, winner_watch_id=?, updated_at=?, finished_at=?
		WHERE tenant_id=? AND id=? AND status='pending'`, status, triggerWatchID, now, now, tenantID, groupID)
	if err != nil {
		return resolution, err
	}
	won, _ := result.RowsAffected()
	if won == 1 {
		// The aggregate verdict owns one finalization. Other members are settled
		// silently so reconciliation cannot emit per-member child Runs later.
		if _, err := tx.ExecContext(ctx, `UPDATE external_watches SET
			status=CASE WHEN status IN ('pending','running') THEN 'cancelled' ELSE status END,
			last_error=CASE WHEN status IN ('pending','running') THEN 'wait group reached its aggregate verdict' ELSE last_error END,
			finalized=1, notified=1, finished_at=COALESCE(finished_at, ?), updated_at=?
			WHERE tenant_id=? AND wait_group_id=? AND id<>?`, now, now, tenantID, groupID, triggerWatchID); err != nil {
			return resolution, err
		}
	}
	if err := tx.Commit(); err != nil {
		return resolution, err
	}
	group.Status = status
	group.WinnerWatchID = triggerWatchID
	resolution.Group = group
	resolution.Terminal = true
	resolution.Won = won == 1
	resolution.Status = status
	return resolution, nil
}

// IncompleteRunWatchGroups is also the handoff guard: registration is not a
// successful lifecycle handoff while any promised target has no durable member.
func (s *Store) IncompleteRunWatchGroups(ctx context.Context, tenantID, runID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.group_key, g.expected_count, COUNT(w.id)
 FROM external_watch_groups g LEFT JOIN external_watches w ON w.tenant_id=g.tenant_id AND w.wait_group_id=g.id
 WHERE g.tenant_id=? AND g.run_id=? AND g.status='pending'
 GROUP BY g.id HAVING COUNT(w.id)<g.expected_count ORDER BY g.group_key`, normalizeTenant(tenantID), runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var missing []string
	for rows.Next() {
		var key string
		var expected, actual int
		if err := rows.Scan(&key, &expected, &actual); err != nil {
			return nil, err
		}
		missing = append(missing, fmt.Sprintf("group %s has %d/%d members", key, actual, expected))
	}
	return missing, rows.Err()
}

// ExternalWaitBacklog is a stock, independent of the report's event window.
type ExternalWaitBacklog struct {
	PendingGroups, IncompleteGroups, UnfinalizedMembers int
	OldestGroupAt                                       time.Time
}

func (s *Store) ExternalWaitBacklogForPerson(ctx context.Context, tenantID, personID string) (ExternalWaitBacklog, error) {
	var out ExternalWaitBacklog
	var oldest int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(members<expected_count),0),COALESCE(MIN(created_at),0) FROM (
 SELECT g.expected_count,g.created_at,COUNT(w.id) members FROM external_watch_groups g
 LEFT JOIN external_watches w ON w.tenant_id=g.tenant_id AND w.wait_group_id=g.id
 WHERE g.tenant_id=? AND g.person_id=? AND g.status='pending' GROUP BY g.id)`, normalizeTenant(tenantID), personID).Scan(&out.PendingGroups, &out.IncompleteGroups, &oldest)
	if err != nil {
		return out, err
	}
	if oldest > 0 {
		out.OldestGroupAt = time.Unix(oldest, 0)
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_watches WHERE tenant_id=? AND person_id=? AND finalized=0 AND status IN ('succeeded','failed','timed_out','blocked_environment')`, normalizeTenant(tenantID), personID).Scan(&out.UnfinalizedMembers)
	return out, err
}
