package control

import (
	"context"
	"testing"
	"time"
)

// TestMaintenanceJobLifecycle pins the idempotency contract: the job is born
// in FinishRun's terminal transaction, exactly one claimer wins, completion is
// terminal, and a crashed 'running' job can be reset and reclaimed.
func TestMaintenanceJobLifecycle(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)

	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	job, err := store.GetMaintenanceJob(ctx, identity.TenantID, run.ID, 1)
	if err != nil || job == nil {
		t.Fatalf("FinishRun must create the maintenance job: job=%+v err=%v", job, err)
	}
	if job.Status != MaintenanceJobPending {
		t.Fatalf("new job status = %q, want pending", job.Status)
	}
	if err := store.SetMaintenanceJobPayload(ctx, identity.TenantID, run.ID, 1, `{"run":"payload"}`); err != nil {
		t.Fatal(err)
	}
	due, err := store.ListRunnableMaintenanceJobs(ctx, 1, 10)
	if err != nil || len(due) != 1 || due[0].PayloadJSON == "" {
		t.Fatalf("runnable payload jobs=%+v err=%v", due, err)
	}

	// Duplicate terminal notification: FinishRun again must not reset the job.
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatalf("second FinishRun: %v", err)
	}

	claimed, err := store.ClaimMaintenanceJob(ctx, identity.TenantID, run.ID, 1)
	if err != nil || !claimed {
		t.Fatalf("first claim must win: claimed=%v err=%v", claimed, err)
	}
	claimed, err = store.ClaimMaintenanceJob(ctx, identity.TenantID, run.ID, 1)
	if err != nil || claimed {
		t.Fatalf("second claim must lose while running: claimed=%v err=%v", claimed, err)
	}
	if err := store.SaveMaintenanceProposal(ctx, identity.TenantID, run.ID, 1, `{"task_decision":"KEEP"}`, "abcd1234"); err != nil {
		t.Fatal(err)
	}

	if err := store.CompleteMaintenanceJob(ctx, identity.TenantID, run.ID, 1, "abcd1234"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	claimed, err = store.ClaimMaintenanceJob(ctx, identity.TenantID, run.ID, 1)
	if err != nil || claimed {
		t.Fatalf("succeeded job must never be reclaimed: claimed=%v err=%v", claimed, err)
	}
	job, _ = store.GetMaintenanceJob(ctx, identity.TenantID, run.ID, 1)
	if job.Status != MaintenanceJobSucceeded || job.ResultHash != "abcd1234" || job.Attempts != 1 {
		t.Fatalf("terminal job = %+v", job)
	}

	// A bumped analyzer version is a NEW logical job for the same run.
	if job2, _ := store.GetMaintenanceJob(ctx, identity.TenantID, run.ID, 2); job2 != nil {
		t.Fatalf("version-2 job must not exist yet: %+v", job2)
	}
}

// TestMaintenanceJobFailureAndRecovery: a failed job becomes claimable only
// after its retry horizon, and a crashed 'running' job returns to pending via
// the recovery sweep helper.
func TestMaintenanceJobFailureAndRecovery(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}

	if claimed, _ := store.ClaimMaintenanceJob(ctx, identity.TenantID, run.ID, 1); !claimed {
		t.Fatal("claim failed")
	}
	if err := store.FailMaintenanceJob(ctx, identity.TenantID, run.ID, 1, "provider timeout", time.Hour); err != nil {
		t.Fatal(err)
	}
	if claimed, _ := store.ClaimMaintenanceJob(ctx, identity.TenantID, run.ID, 1); claimed {
		t.Fatal("failed job must not be claimable before its retry horizon")
	}
	if err := store.FailMaintenanceJob(ctx, identity.TenantID, run.ID, 1, "", 0); err != nil {
		t.Fatal(err) // status is failed, not running: no-op update
	}

	// Simulate the horizon passing.
	if _, err := store.db.ExecContext(ctx, `UPDATE maintenance_jobs SET next_retry_at = 0 WHERE run_id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if claimed, _ := store.ClaimMaintenanceJob(ctx, identity.TenantID, run.ID, 1); !claimed {
		t.Fatal("failed job must be claimable after its retry horizon")
	}

	// Crash while running: the sweep resets it to pending, then a claim wins.
	if _, err := store.db.ExecContext(ctx, `UPDATE maintenance_jobs SET updated_at = 1 WHERE run_id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	reset, err := store.ResetStaleMaintenanceJobs(ctx, time.Minute)
	if err != nil || reset != 1 {
		t.Fatalf("reset stale: n=%d err=%v", reset, err)
	}
	if claimed, _ := store.ClaimMaintenanceJob(ctx, identity.TenantID, run.ID, 1); !claimed {
		t.Fatal("reset job must be claimable again")
	}
	job, _ := store.GetMaintenanceJob(ctx, identity.TenantID, run.ID, 1)
	if job.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (initial + post-fail + post-reset)", job.Attempts)
	}
}
