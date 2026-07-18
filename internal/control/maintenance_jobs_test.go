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

func TestFinishRunPersistsMaintenancePayloadAtomically(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	payload := `{"run":"atomic-replay"}`

	if err := store.FinishRunWithMaintenancePayload(ctx, identity.TenantID, run.ID, "done", 1, payload); err != nil {
		t.Fatalf("FinishRunWithMaintenancePayload: %v", err)
	}
	job, err := store.GetMaintenanceJob(ctx, identity.TenantID, run.ID, 1)
	if err != nil || job == nil {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	if job.PayloadJSON != payload || job.Status != MaintenanceJobPending {
		t.Fatalf("job=%+v", job)
	}

	// Duplicate finalization must not replace the immutable first replay input.
	if err := store.FinishRunWithMaintenancePayload(ctx, identity.TenantID, run.ID, "done", 1, `{"run":"replacement"}`); err != nil {
		t.Fatalf("duplicate finalize: %v", err)
	}
	job, _ = store.GetMaintenanceJob(ctx, identity.TenantID, run.ID, 1)
	if job.PayloadJSON != payload {
		t.Fatalf("payload was replaced: %q", job.PayloadJSON)
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

// TestMaintenanceJobProviderBlockAndRestartProbe pins the fatal-provider
// contract: quota/auth/configuration failures do not enter the timed retry
// loop, diagnostics can see the pause, and a daemon restart grants exactly one
// fresh probe after the owner updates provider configuration.
func TestMaintenanceJobProviderBlockAndRestartProbe(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimMaintenanceJob(ctx, identity.TenantID, run.ID, 1); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}

	blocked, err := store.BlockMaintenanceJob(ctx, identity.TenantID, run.ID, 1, "provider 403: quota exhausted")
	if err != nil || !blocked {
		t.Fatalf("block: blocked=%v err=%v", blocked, err)
	}
	if claimed, err := store.ClaimMaintenanceJob(ctx, identity.TenantID, run.ID, 1); err != nil || claimed {
		t.Fatalf("provider-blocked job must not retry: claimed=%v err=%v", claimed, err)
	}
	job, err := store.GetMaintenanceJob(ctx, identity.TenantID, run.ID, 1)
	if err != nil || job == nil {
		t.Fatalf("job: %+v err=%v", job, err)
	}
	if job.Status != MaintenanceJobBlockedProvider || job.Attempts != 1 {
		t.Fatalf("blocked job = %+v", job)
	}
	health, err := store.MaintenanceHealthForPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil || health.Blocked != 1 || health.LastError != "provider 403: quota exhausted" {
		t.Fatalf("health = %+v err=%v", health, err)
	}

	reset, err := store.ResetBlockedMaintenanceJobs(ctx)
	if err != nil || reset != 1 {
		t.Fatalf("restart reset: count=%d err=%v", reset, err)
	}
	if claimed, err := store.ClaimMaintenanceJob(ctx, identity.TenantID, run.ID, 1); err != nil || !claimed {
		t.Fatalf("fresh restart probe: claimed=%v err=%v", claimed, err)
	}
}

func TestReplayRetryLimitedMaintenanceJobsIsSelective(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	payload := `{"run":"historic"}`
	if err := store.FinishRunWithMaintenancePayload(ctx, identity.TenantID, run.ID, "done", 1, payload); err != nil {
		t.Fatal(err)
	}
	if err := store.SkipMaintenanceJob(ctx, identity.TenantID, run.ID, 1, "maintenance retry limit reached"); err != nil {
		t.Fatal(err)
	}

	// A deterministic skip must not be replayed even when it has evidence.
	task2, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "not eligible"})
	if err != nil {
		t.Fatal(err)
	}
	run2, err := store.StartRun(ctx, task2, "cli", "status")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRunWithMaintenancePayload(ctx, identity.TenantID, run2.ID, "done", 1, `{"run":"skip"}`); err != nil {
		t.Fatal(err)
	}
	if err := store.SkipMaintenanceJob(ctx, identity.TenantID, run2.ID, 1, "run is not eligible"); err != nil {
		t.Fatal(err)
	}

	n, err := store.ReplayRetryLimitedMaintenanceJobs(ctx, identity.TenantID, 10)
	if err != nil || n != 1 {
		t.Fatalf("replayed=%d err=%v", n, err)
	}
	job, _ := store.GetMaintenanceJob(ctx, identity.TenantID, run.ID, 1)
	if job == nil || job.Status != MaintenanceJobPending || job.Attempts != 0 || job.LastError != "" {
		t.Fatalf("replayed job=%+v", job)
	}
	job2, _ := store.GetMaintenanceJob(ctx, identity.TenantID, run2.ID, 1)
	if job2 == nil || job2.Status != MaintenanceJobSkipped {
		t.Fatalf("deterministic skip changed: %+v", job2)
	}
}

// TestMaintenanceAttemptHistory pins the append-only failure timeline: the
// job row's last_error is overwritten on every transition, so fail/skip/block
// must each leave a durable history row with the REAL error, and retention
// pruning stays bounded.
func TestMaintenanceAttemptHistory(t *testing.T) {
	ctx := context.Background()
	store, identity, _, run := newRecoveryFixture(t)
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimMaintenanceJob(ctx, identity.TenantID, run.ID, 1); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if err := store.FailMaintenanceJob(ctx, identity.TenantID, run.ID, 1, "context deadline exceeded", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.SkipMaintenanceJob(ctx, identity.TenantID, run.ID, 1, "maintenance retry limit reached"); err != nil {
		t.Fatal(err)
	}
	attempts, err := store.RecentMaintenanceAttempts(ctx, identity.TenantID, time.Now().Add(-time.Hour), 10)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempts=%+v err=%v", attempts, err)
	}
	// Newest first: the skip, then the failure that carries the real error.
	if attempts[0].Outcome != "skipped" || attempts[1].Outcome != "failed" {
		t.Fatalf("outcomes = %q, %q", attempts[0].Outcome, attempts[1].Outcome)
	}
	if attempts[1].Error != "context deadline exceeded" {
		t.Fatalf("real error lost: %q", attempts[1].Error)
	}

	// Retention: age the rows and prune.
	if _, err := store.db.ExecContext(ctx, `UPDATE maintenance_attempts SET created_at = ?`,
		time.Now().Add(-40*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	pruned, err := store.PruneMaintenanceAttempts(ctx, 0)
	if err != nil || pruned != 2 {
		t.Fatalf("pruned=%d err=%v", pruned, err)
	}
}
