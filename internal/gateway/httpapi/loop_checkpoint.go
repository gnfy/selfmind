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
	"selfmind/internal/runpool"
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
		Snapshot: snapshot, WaitKind: checkpoint.WaitKind, WaitRef: checkpoint.WaitRef, WaitToolCallID: checkpoint.WaitToolCallID,
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
	if parent.TaskID != task.ID || parent.PersonID != identity.PersonID {
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
	ctx = kernel.WithLoopResumeMessages(ctx, messages)
	if record.Outcome != "await_human" || record.WaitToolCallID == "" {
		return ctx
	}
	resume := kernel.HumanWaitResume{ToolCallID: record.WaitToolCallID}
	switch record.WaitKind {
	case control.LoopCheckpointWaitApproval:
		approval, lookupErr := c.srv.Control.GetApprovalRequest(ctx, identity.TenantID, record.WaitRef)
		if lookupErr != nil || approval == nil || approval.PersonID != identity.PersonID || approval.RunID != parent.ID || (approval.Status != "approved" && approval.Status != "rejected") {
			return ctx
		}
		resume.Denied = approval.Status == "rejected"
	case control.LoopCheckpointWaitClarification:
		clarify, lookupErr := c.srv.Control.GetClarifyRequest(ctx, identity.TenantID, record.WaitRef)
		if lookupErr != nil || clarify == nil || clarify.PersonID != identity.PersonID || clarify.RunID != parent.ID || clarify.Status != "answered" {
			return ctx
		}
		var choices []string
		_ = json.Unmarshal(clarify.Options, &choices)
		result, _ := json.Marshal(map[string]interface{}{"question": clarify.Question, "choices_offered": choices, "user_response": clarify.Answer})
		resume.Result = &llm.Message{Role: "tool", ToolCallID: record.WaitToolCallID, Content: string(result)}
	default:
		return ctx
	}
	if !resume.Denied {
		resume.RecheckReason = c.humanResumeRecheckReason(ctx, identity, parent, record)
	}
	return kernel.WithHumanWaitResume(ctx, resume)
}

func (c *RunCoordinator) humanResumeRecheckReason(ctx context.Context, identity *control.IdentityContext, parent *control.Run, record *control.LoopCheckpointRecord) string {
	scope, _ := kernel.ToolInvocationScopeFromContext(ctx)
	activity, err := c.srv.Control.ListRunRootActivitySince(ctx, identity.TenantID, identity.PersonID, parent.ID, scope.RunID, record.UpdatedAt)
	if err != nil {
		log.Warn("human resume cannot establish workspace stability", "run_id", parent.ID, "error", err)
		return "Workspace activity during the human wait could not be established; revalidate before acting."
	}
	paths := writableRootPaths(parent.ExecutionRoots)
	for _, other := range activity {
		otherPaths := writableRootPaths(other.ExecutionRoots)
		if len(other.ExecutionRoots) > 0 && len(otherPaths) == 0 {
			continue
		}
		if len(paths) == 0 || len(otherPaths) == 0 || runpool.PathsConflict(paths, otherPaths) {
			return "Another Run used overlapping writable execution roots during the human wait; revalidate before acting."
		}
	}
	return ""
}
