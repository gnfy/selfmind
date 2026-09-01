package httpapi

import (
	"context"
	"encoding/json"
	"fmt"

	"selfmind/internal/control"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/log"
)

type controlLoopCheckpointSink struct {
	store    *control.Store
	tenantID string
	personID string
	taskID   string
	runID    string
}

func (c *RunCoordinator) newLoopCheckpointSink(identity *control.IdentityContext, task *control.Task, run *control.Run) kernel.LoopCheckpointSink {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil || run == nil {
		return nil
	}
	return &controlLoopCheckpointSink{
		store: c.srv.Control, tenantID: identity.TenantID, personID: identity.PersonID,
		taskID: task.ID, runID: run.ID,
	}
}

func (s *controlLoopCheckpointSink) SaveLoopCheckpoint(ctx context.Context, checkpoint kernel.LoopCheckpoint) error {
	if s == nil || s.store == nil {
		return fmt.Errorf("loop checkpoint store is unavailable")
	}
	snapshot, err := json.Marshal(checkpoint.Messages)
	if err != nil {
		return fmt.Errorf("encode loop checkpoint: %w", err)
	}
	recovery, err := s.store.RunRecoveryState(ctx, s.tenantID, s.runID)
	if err != nil {
		return fmt.Errorf("read run recovery state: %w", err)
	}
	recoveryJSON, err := json.Marshal(recovery)
	if err != nil {
		return fmt.Errorf("encode run recovery state: %w", err)
	}
	return s.store.SaveLoopCheckpoint(ctx, control.LoopCheckpointRecord{
		TenantID: s.tenantID, PersonID: s.personID, TaskID: s.taskID, RunID: s.runID,
		ContractVersion: recovery.ContractVersion, Recovery: recoveryJSON,
		Iteration: checkpoint.Iteration, Outcome: string(checkpoint.Outcome), Detail: checkpoint.Detail,
		Snapshot: snapshot,
	})
}

// withLoopCheckpointResume restores the message ledger of the run this turn
// continues. Run-scoped by P0: only the exact resolved parent's incomplete
// checkpoint may replace the message array — the old "most recently updated
// checkpoint anywhere under the task" pick let recency, not ownership, decide
// which work line was resumed.
func (c *RunCoordinator) withLoopCheckpointResume(ctx context.Context, identity *control.IdentityContext, task *control.Task, parent *control.Run, _ router.IntentResult) context.Context {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil || parent == nil {
		return ctx
	}
	if parent.TaskID != task.ID {
		return ctx
	}
	record, err := c.srv.Control.IncompleteLoopCheckpointForRun(ctx, identity.TenantID, parent.ID)
	if err != nil {
		log.Warn("loop checkpoint lookup failed", "task_id", task.ID, "run_id", parent.ID, "error", err)
		return ctx
	}
	if record == nil || len(record.Snapshot) == 0 {
		return ctx
	}
	var messages []llm.Message
	if err := json.Unmarshal(record.Snapshot, &messages); err != nil {
		log.Warn("loop checkpoint decode failed", "task_id", task.ID, "run_id", record.RunID, "error", err)
		return ctx
	}
	return kernel.WithLoopResumeMessages(ctx, messages)
}
