package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrTaskHasLiveWork prevents display-level closure from racing work that
// can still produce side effects. The person must stop the active run or
// cancel the external watcher first.
var ErrTaskHasLiveWork = errors.New("task has live work")

type TaskClosureResult struct {
	Changed               bool
	ExpiredApprovals      int
	ExpiredClarifications int
	CancelledQueueRows    int
}

// CompleteTaskByUser dismisses a work label's current Attention without
// rewriting historical run outcomes. Unlike the ordinary reducer, this is
// explicit person authority: an old unclaimed parked run must not keep a task
// open after the person says the work is complete. Running work and live
// control objects (pending approvals, pending clarifications, watchers) fail
// closed; the person answers, rejects, or cancels them first.
func (s *Store) CompleteTaskByUser(ctx context.Context, tenantID, personID, taskID string) (TaskClosureResult, error) {
	changed, err := NewWorkTimeline(s).DismissAttention(ctx, tenantID, personID, taskID)
	return TaskClosureResult{Changed: changed > 0}, err
}

// ArchiveTaskByUser is the reversible hygiene variant of explicit closure. It
// shares completion's cleanup and live-effect guard, then hides the label from
// open work until an explicit /resume reopens it.
func (s *Store) ArchiveTaskByUser(ctx context.Context, tenantID, personID, taskID string) (TaskClosureResult, error) {
	err := NewWorkTimeline(s).Archive(ctx, tenantID, personID, taskID)
	return TaskClosureResult{Changed: err == nil}, err
}

func (s *Store) closeTaskByUser(ctx context.Context, tenantID, personID, taskID, targetStatus string) (TaskClosureResult, error) {
	if s == nil || s.db == nil || strings.TrimSpace(personID) == "" || strings.TrimSpace(taskID) == "" {
		return TaskClosureResult{}, fmt.Errorf("task closure requires a control store, person, and task ids")
	}
	if targetStatus == "archived" {
		return s.ArchiveTaskByUser(ctx, tenantID, personID, taskID)
	}
	return s.CompleteTaskByUser(ctx, tenantID, personID, taskID)
}
