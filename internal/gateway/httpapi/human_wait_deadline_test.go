package httpapi

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

func TestHumanWaitTimeoutBeforeGraceRetainsNestedStack(t *testing.T) {
	daemon, store, owner, task, run := newClarifyTestServer(t)
	daemon.HumanWaitGrace = time.Second
	daemon.ApprovalWait = 20 * time.Millisecond
	daemon.ApprovalWaitUnattended = 20 * time.Millisecond
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := kernel.ForkDelegationContext(kernel.WithDelegationNamespace(parent, "partial-effect-tool"))
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = daemon.coordinator().toolApprovalHandler(owner, task, run, "cli")(ctx, tools.ToolApprovalRequest{TenantID: owner.TenantID, PersonID: owner.PersonID, TaskID: task.ID, RunID: run.ID, ToolName: "write_file", Args: map[string]interface{}{"path": "receipt.txt", "content": "not dispatched"}})
	}()
	select {
	case <-finished:
		pending, _ := store.ListApprovalRequests(context.Background(), owner.TenantID, owner.PersonID, "pending", 10)
		cp, _ := store.IncompleteLoopCheckpointForRun(context.Background(), owner.TenantID, run.ID)
		t.Fatalf("nested invocation unwound before safe checkpoint: pending=%+v checkpoint=%+v", pending, cp)
	case <-time.After(150 * time.Millisecond):
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release waiter")
	}
}

func TestHumanWaitShortApprovalBudgetRequiresCheckpoint(t *testing.T) {
	daemon, store, _, owner := newHumanWaitServer(t)
	daemon.ApprovalWait = 20 * time.Millisecond
	daemon.ApprovalWaitUnattended = 20 * time.Millisecond
	ws := humanWaitWorkspace(t, store, owner, "short-budget")
	startHumanWait(t, daemon, ws, "short-budget")
	approval := waitParkedApproval(t, daemon, store, owner)
	cp, err := store.IncompleteLoopCheckpointForRun(context.Background(), owner.TenantID, approval.RunID)
	run, _ := store.GetRun(context.Background(), owner.TenantID, approval.RunID)
	if err != nil || cp == nil || cp.Outcome != "await_human" || cp.WaitKind != control.LoopCheckpointWaitApproval || cp.WaitRef != approval.ID || cp.WaitToolCallID == "" {
		t.Fatalf("worker released with pending approval but no exact human checkpoint: run=%+v approval=%+v cp=%+v err=%v", run, approval, cp, err)
	}
}

func TestHumanWaitReadFailureAfterFailedCheckpointRetainsWorker(t *testing.T) {
	for _, kind := range []string{"approval", "clarification"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(`CREATE TRIGGER fail_human_checkpoint BEFORE INSERT ON loop_checkpoints WHEN NEW.outcome='await_human' BEGIN SELECT RAISE(ABORT,'injected checkpoint write failure'); END`); err != nil {
				t.Fatal(err)
			}
			daemon, _, provider, owner := newHumanWaitServerWithStore(t, store)
			daemon.HumanWaitGrace = 25 * time.Millisecond
			provider.clarification = kind == "clarification"
			ws := humanWaitWorkspace(t, store, owner, "failed-human-read")
			startHumanWait(t, daemon, ws, "failed-human-read")
			var runID, taskID, waitID string
			waitUntil(t, time.Second, func() bool {
				if kind == "approval" {
					pending, _ := store.ListApprovalRequests(context.Background(), owner.TenantID, owner.PersonID, "pending", 10)
					if len(pending) == 1 {
						runID, taskID, waitID = pending[0].RunID, pending[0].TaskID, pending[0].ID
					}
				} else {
					pending, _ := store.ListClarifyRequests(context.Background(), owner.TenantID, owner.PersonID, "pending", 10)
					if len(pending) == 1 {
						runID, taskID, waitID = pending[0].RunID, pending[0].TaskID, pending[0].ID
					}
				}
				if runID == "" {
					return false
				}
				events, _ := store.ListTaskEvents(context.Background(), taskID, 100)
				for _, event := range events {
					if event.Type == "checkpoint.failed" {
						return true
					}
				}
				return false
			}, "checkpoint failure not reached")
			query := `UPDATE approval_requests SET created_at='injected-invalid-timestamp' WHERE id=?`
			if kind == "clarification" {
				query = `UPDATE clarify_requests SET created_at='injected-invalid-timestamp' WHERE id=?`
			}
			if _, err := raw.Exec(query, waitID); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, 2*time.Second, func() bool {
				events, _ := store.ListTaskEvents(context.Background(), taskID, 100)
				for _, event := range events {
					if event.Type == "human_wait.control_failed" {
						return true
					}
				}
				return false
			}, "read failure did not retain the wait with a diagnostic")
			run, _ := store.GetRun(context.Background(), owner.TenantID, runID)
			if daemon.coordinator().activeCount(owner.PersonID) != 1 || run.Status != "running" {
				t.Fatalf("failed checkpoint wait released without answer/cancellation: run=%+v active=%d", run, daemon.coordinator().activeCount(owner.PersonID))
			}
			daemon.coordinator().stopAllActive("cancel unreadable human wait")
			waitUntil(t, time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "unreadable wait ignored cancellation")
		})
	}
}

func TestHumanWaitDecisionClaimFailureRetainsOriginalTurn(t *testing.T) {
	dir := t.TempDir()
	store := controltest.NewStoreInDir(t, dir)
	raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER fail_human_claim BEFORE UPDATE ON approval_requests WHEN NEW.decision_claimed_at>0 BEGIN SELECT RAISE(ABORT,'injected decision claim write failure'); END`); err != nil {
		t.Fatal(err)
	}
	daemon, _, _, owner := newHumanWaitServerWithStore(t, store)
	daemon.HumanWaitGrace = 3 * time.Second
	ws := humanWaitWorkspace(t, store, owner, "claim-failure")
	startHumanWait(t, daemon, ws, "claim-failure")
	var approval control.ApprovalRequest
	waitUntil(t, time.Second, func() bool {
		pending, _ := store.ListApprovalRequests(context.Background(), owner.TenantID, owner.PersonID, "pending", 10)
		if len(pending) == 1 {
			approval = pending[0]
			return true
		}
		return false
	}, "approval was not durable")
	if _, err := daemon.respondApprovalByToken(context.Background(), owner, approval.ID, "approved", "claim-failure", control.ApprovalDecisionInput{}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, func() bool {
		events, _ := store.ListTaskEvents(context.Background(), approval.TaskID, 100)
		for _, event := range events {
			if event.Type == "human_wait.control_failed" {
				return true
			}
		}
		return false
	}, "decision claim error unwound the original tool stack")
	run, _ := store.GetRun(context.Background(), owner.TenantID, approval.RunID)
	if run.Status != "running" || daemon.coordinator().activeCount(owner.PersonID) != 1 {
		t.Fatalf("claim failure released original turn: %+v", run)
	}
	if _, err := os.Stat(filepath.Join(ws.LocalPath, "receipt.txt")); !os.IsNotExist(err) {
		t.Fatalf("unclaimed decision produced an effect: %v", err)
	}
	if _, err := raw.Exec(`DROP TRIGGER fail_human_claim`); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(ws.LocalPath, "receipt.txt"))
		return err == nil && string(data) == "approved effect"
	}, "original waiter did not consume its recovered exact decision")
	waitUntil(t, time.Second, func() bool { return daemon.coordinator().activeCount(owner.PersonID) == 0 }, "original turn did not settle")
	if row, _ := store.GetQueuedByIdempotencyKey(context.Background(), owner.TenantID, "approval-resume:"+approval.ID); row != nil {
		t.Fatalf("claim error created a second continuation: %+v", row)
	}
}

func TestHumanWaitExpiredRecordIsNotAHumanAnswer(t *testing.T) {
	for _, kind := range []string{"approval", "clarification"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			// Fault injection competes with the live waiter, just like an
			// ordinary store write; do not use SQLite's zero-wait default.
			raw.SetMaxOpenConns(1)
			if _, err := raw.Exec(`PRAGMA busy_timeout=5000`); err != nil {
				t.Fatal(err)
			}
			daemon, _, provider, owner := newHumanWaitServerWithStore(t, store)
			daemon.HumanWaitGrace = 3 * time.Second
			provider.clarification = kind == "clarification"
			ws := humanWaitWorkspace(t, store, owner, "expired-human-record")
			startHumanWait(t, daemon, ws, "expired-human-record")
			var taskID, runID, waitID string
			waitUntil(t, time.Second, func() bool {
				if kind == "approval" {
					rows, _ := store.ListApprovalRequests(context.Background(), owner.TenantID, owner.PersonID, "pending", 10)
					if len(rows) == 1 {
						taskID, runID, waitID = rows[0].TaskID, rows[0].RunID, rows[0].ID
					}
				} else {
					rows, _ := store.ListClarifyRequests(context.Background(), owner.TenantID, owner.PersonID, "pending", 10)
					if len(rows) == 1 {
						taskID, runID, waitID = rows[0].TaskID, rows[0].RunID, rows[0].ID
					}
				}
				return waitID != ""
			}, "human request was not durable")
			query := `UPDATE approval_requests SET status='expired' WHERE id=?`
			if kind == "clarification" {
				query = `UPDATE clarify_requests SET status='expired' WHERE id=?`
			}
			if _, err := raw.Exec(query, waitID); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, 2*time.Second, func() bool {
				events, _ := store.ListTaskEvents(context.Background(), taskID, 100)
				for _, event := range events {
					if event.Type == "human_wait.control_failed" {
						return true
					}
				}
				return false
			}, "expired record was consumed as a human answer")
			run, _ := store.GetRun(context.Background(), owner.TenantID, runID)
			if run.Status != "running" || daemon.coordinator().activeCount(owner.PersonID) != 1 {
				t.Fatalf("expired record released tool stack: %+v", run)
			}
		})
	}
}
