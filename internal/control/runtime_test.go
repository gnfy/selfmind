package control

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// newRecoveryFixture builds a store with one identity and one task that has a
// 'running' run, the state a daemon crash leaves behind.
func newRecoveryFixture(t *testing.T) (*Store, *IdentityContext, *Task, *Run) {
	t.Helper()
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "long task",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	run, err := store.StartRun(ctx, task, "cli", "do the work")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return store, identity, task, run
}

// TestMarkInterruptedRunsBootSweep is the boot-sweep contract: a leftover
// 'running' run (stale by definition at boot, threshold 0) flips to
// 'interrupted' together with its still-running task.
func TestMarkInterruptedRunsBootSweep(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	approval, err := store.CreateApprovalRequest(ctx, ApprovalRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID, RunID: run.ID,
		ActionType: "tool_call", RequestedChannel: "cli", Payload: json.RawMessage(`{"tool":"terminal"}`),
	})
	if err != nil {
		t.Fatalf("CreateApprovalRequest: %v", err)
	}

	count, err := store.MarkInterruptedRuns(ctx, 0)
	if err != nil {
		t.Fatalf("MarkInterruptedRuns: %v", err)
	}
	if count != 1 {
		t.Fatalf("recovered = %d, want 1", count)
	}
	left, err := store.ListRunningRuns(ctx, identity.TenantID, nil)
	if err != nil {
		t.Fatalf("ListRunningRuns: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("running runs left after sweep: %+v", left)
	}
	got, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "interrupted" || got.ActiveRunID != "" {
		t.Fatalf("task after sweep = status %q active_run_id %q, want interrupted/empty", got.Status, got.ActiveRunID)
	}
	events, err := store.ListTaskEvents(ctx, task.ID, 10)
	if err != nil {
		t.Fatalf("ListTaskEvents: %v", err)
	}
	var recovered map[string]interface{}
	foundApprovalParked := false
	for _, event := range events {
		if event.Type == "run.interrupted" && event.RunID == run.ID {
			if err := json.Unmarshal(event.Payload, &recovered); err != nil {
				t.Fatalf("decode recovery event: %v", err)
			}
		}
		if event.Type == "approval.parked" {
			var payload map[string]interface{}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode approval parked event: %v", err)
			}
			foundApprovalParked = payload["approval_id"] == approval.ID
		}
	}
	outcome, _ := recovered["outcome"].(map[string]interface{})
	if outcome["completion_reason"] != "daemon_recovery" || outcome["resumable"] != true {
		t.Fatalf("recovery outcome = %#v, want daemon_recovery/resumable", outcome)
	}
	if !foundApprovalParked {
		t.Fatalf("recovery did not append approval.parked for %s: %+v", approval.ID, events)
	}
	storedApproval, err := store.GetApprovalRequest(ctx, identity.TenantID, approval.ID)
	if err != nil || storedApproval == nil || storedApproval.Status != "pending" || storedApproval.WaiterState != "parked" {
		t.Fatalf("approval after recovery = %+v, err=%v; want pending/parked", storedApproval, err)
	}
}

// TestMarkInterruptedRunsKeepsFreshHeartbeat proves the periodic sweep can
// never kill a run whose heartbeat is younger than the staleness threshold.
func TestMarkInterruptedRunsKeepsFreshHeartbeat(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)

	count, err := store.MarkInterruptedRuns(ctx, 30*time.Second)
	if err != nil {
		t.Fatalf("MarkInterruptedRuns: %v", err)
	}
	if count != 0 {
		t.Fatalf("recovered = %d, want 0 for a fresh run", count)
	}
	left, err := store.ListRunningRuns(ctx, identity.TenantID, nil)
	if err != nil {
		t.Fatalf("ListRunningRuns: %v", err)
	}
	if len(left) != 1 || left[0].ID != run.ID {
		t.Fatalf("fresh run should stay running, got %+v", left)
	}
	got, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "running" || got.ActiveRunID != run.ID {
		t.Fatalf("task should be untouched, got status %q active_run_id %q", got.Status, got.ActiveRunID)
	}
}

// TestMarkInterruptedRunsRespectsExcludeList proves a run registered in the
// gateway's active-run registry survives the sweep even when its heartbeat
// looks stale (the registry is the source of truth for "actually executing").
func TestMarkInterruptedRunsRespectsExcludeList(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)

	// olderThan < 0 pushes the cutoff into the future: every running run is
	// "stale" and only the exclude list protects it.
	count, err := store.MarkInterruptedRuns(ctx, -time.Second, run.ID)
	if err != nil {
		t.Fatalf("MarkInterruptedRuns: %v", err)
	}
	if count != 0 {
		t.Fatalf("recovered = %d, want 0 with the run excluded", count)
	}
	got, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "running" || got.ActiveRunID != run.ID {
		t.Fatalf("excluded run's task must be untouched, got status %q active_run_id %q", got.Status, got.ActiveRunID)
	}
}

// TestMarkInterruptedRunsRepairsOrphanedRunningTask reproduces the live-DB bug
// behind F5: a task left 'running' with active_run_id already cleared and no
// 'running' run at all (historic finalization wrote a non-terminal run status).
// The old sweep's task flip was guarded by active_run_id = <dead run> and
// skipped such tasks forever; the orphan repair must catch them.
func TestMarkInterruptedRunsRepairsOrphanedRunningTask(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)

	// Terminal run, cleared active_run_id...
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "interrupted"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	// ...but the task written back to 'running' (what old binaries did when the
	// outcome said "more work planned").
	if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, "running", "midway", nil); err != nil {
		t.Fatalf("UpdateTaskStatus: %v", err)
	}

	count, err := store.MarkInterruptedRuns(ctx, 0)
	if err != nil {
		t.Fatalf("MarkInterruptedRuns: %v", err)
	}
	if count != 1 {
		t.Fatalf("recovered = %d, want 1 orphaned task", count)
	}
	got, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "interrupted" || got.ActiveRunID != "" {
		t.Fatalf("orphaned task = status %q active_run_id %q, want interrupted/empty", got.Status, got.ActiveRunID)
	}
	if got.CurrentSummary != "midway" {
		t.Fatalf("orphan repair must not overwrite an existing summary, got %q", got.CurrentSummary)
	}
}

// TestFinishRunCoercesNonTerminalStatus pins FinishRun's terminal contract: a
// caller passing 'running' must not be able to leave a finished run row in a
// non-terminal state (that is what orphaned the F5 tasks in the first place).
func TestFinishRunCoercesNonTerminalStatus(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)

	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "running"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	left, err := store.ListRunningRuns(ctx, identity.TenantID, nil)
	if err != nil {
		t.Fatalf("ListRunningRuns: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("finished run must be terminal, still running: %+v", left)
	}
	got, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.ActiveRunID != "" {
		t.Fatalf("active_run_id should be cleared, got %q", got.ActiveRunID)
	}
}
