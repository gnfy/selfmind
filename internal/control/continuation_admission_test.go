package control

import (
	"context"
	"errors"
	"testing"
	"time"
)

// parkableProviderRun starts a Run with the given snapshot and the durable
// model checkpoint a provider wait requires.
func parkableProviderRun(t *testing.T, store *Store, platformUserID string, admission AdmissionSnapshot) (*IdentityContext, *Task, *Run) {
	t.Helper()
	ctx := context.Background()
	owner, task := admissionOwnerAndThread(t, store, platformUserID)
	run, err := store.StartRunWithOptions(ctx, task, "cli", "check the nightly export", StartRunOptions{Admission: admission})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveLoopCheckpoint(ctx, LoopCheckpointRecord{TenantID: owner.TenantID, PersonID: owner.PersonID,
		TaskID: task.ID, RunID: run.ID, ContractVersion: RunRecoveryContractVersion,
		Outcome: "continue_model", Detail: "provider_request", Snapshot: []byte(`[{"role":"user","content":"x"}]`)}); err != nil {
		t.Fatal(err)
	}
	return owner, task, run
}

func providerWaitFinalization(owner *IdentityContext, task *Task, run *Run, admission AdmissionSnapshot) RunFinalization {
	return RunFinalization{Identity: *owner, RunID: run.ID, RunStatus: "waiting_external", TaskID: task.ID,
		TaskStatus: "in_progress", Channel: "cli", Event: Event{Type: "run.finished"},
		Continuation: &QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID, Platform: "cli",
			PlatformUserID: owner.PlatformUserID, Channel: "cli", Content: "Continue this exact work after the provider became available.",
			TaskID: task.ID, ReplyToRunID: run.ID, IdempotencyKey: "provider-wait:" + run.ID,
			NotBefore: time.Now().Add(time.Second), Admission: admission},
		ExpectedRunStatus: "running", RequireCheckpoint: true}
}

// The provider-wait row is inserted inside the parking transaction. It used to
// record no snapshot, so the drained continuation re-derived its authority from
// "Continue this exact work..." and lost the verification-only surface, the
// scheduled origin and its cutoff. It now carries the parked Run's own snapshot.
func TestProviderWaitContinuationCopiesTheParkedRunsAdmission(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	want := verifyOnlyCronAdmission()
	owner, task, run := parkableProviderRun(t, store, "owner", want)

	widened := providerWaitFinalization(owner, task, run, otherAdmission())
	if _, err := store.MaterializeRunFinalization(ctx, widened); !errors.Is(err, ErrResumeAdmissionMismatch) {
		t.Fatalf("a provider-wait row took a snapshot other than the parked Run's: %v", err)
	}
	if current, err := store.GetRun(ctx, owner.TenantID, run.ID); err != nil || current.Status != "running" {
		t.Fatalf("refused park changed the Run: %+v %v", current, err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, providerWaitFinalization(owner, task, run, AdmissionSnapshot{})); err != nil {
		t.Fatal(err)
	}
	row, err := store.GetQueuedByIdempotencyKey(ctx, owner.TenantID, "provider-wait:"+run.ID)
	if err != nil || row == nil || row.AdmissionError != nil || row.Admission != want {
		t.Fatalf("provider-wait row = %+v err=%v, want the parked Run's snapshot", row, err)
	}

	// A caller that restates the parked snapshot is accepted, by another Run
	// and with a different summary spelling that normalizes to the same value.
	restatingOwner, restatingTask, restatingRun := parkableProviderRun(t, store, "restating", want)
	restated := want
	restated.Input.Summary = "  " + want.Input.Summary + "  "
	if _, err := store.MaterializeRunFinalization(ctx, providerWaitFinalization(restatingOwner, restatingTask, restatingRun, restated)); err != nil {
		t.Fatalf("restated snapshot refused: %v", err)
	}
	if row := queuedAdmissionByKey(t, store, restatingOwner.TenantID, "provider-wait:"+restatingRun.ID); row != want {
		t.Fatalf("restated provider-wait row = %+v", row)
	}

	// A Run admitted before snapshots existed parks a row with none, so the
	// drain keeps today's derivation for it.
	legacyOwner, legacyTask, legacyRun := parkableProviderRun(t, store, "legacy", AdmissionSnapshot{})
	if _, err := store.MaterializeRunFinalization(ctx, providerWaitFinalization(legacyOwner, legacyTask, legacyRun, AdmissionSnapshot{})); err != nil {
		t.Fatal(err)
	}
	if row := queuedAdmissionByKey(t, store, legacyOwner.TenantID, "provider-wait:"+legacyRun.ID); !row.IsZero() {
		t.Fatalf("legacy provider-wait row gained a snapshot: %+v", row)
	}

	// An unreadable parked snapshot is copied as stored: the row is then
	// quarantined like any unreadable row, never read as "none".
	brokenOwner, brokenTask, brokenRun := parkableProviderRun(t, store, "broken", want)
	if _, err := store.db.ExecContext(ctx, `UPDATE runs SET admission_json = '{"version":99}' WHERE id = ?`, brokenRun.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, providerWaitFinalization(brokenOwner, brokenTask, brokenRun, AdmissionSnapshot{})); err != nil {
		t.Fatal(err)
	}
	broken, err := store.GetQueuedByIdempotencyKey(ctx, brokenOwner.TenantID, "provider-wait:"+brokenRun.ID)
	if err != nil || broken == nil || !errors.Is(broken.AdmissionError, ErrAdmissionSnapshotUnreadable) {
		t.Fatalf("unreadable parked snapshot row = %+v err=%v, want a quarantined row", broken, err)
	}
}

func queuedAdmissionByKey(t *testing.T, store *Store, tenantID, key string) AdmissionSnapshot {
	t.Helper()
	row, err := store.GetQueuedByIdempotencyKey(context.Background(), tenantID, key)
	if err != nil || row == nil || row.AdmissionError != nil {
		t.Fatalf("queue row %s = %+v err=%v", key, row, err)
	}
	return row.Admission
}

// A durable choice answer queues the person's own words. The row records the
// snapshot built from those words, so its drain neither re-derives authority
// from them later nor reads the answer ("1") as the request.
func TestTurnChoiceQueueRowRecordsThePersonsOwnAdmission(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, action := range []string{"new", "resume"} {
		var task *Task
		var run *Run
		if action == "resume" {
			var err error
			task, err = store.CreateTask(ctx, TaskCreate{TenantID: "default", PersonID: "person", Title: "historical", Channel: "im"})
			if err != nil {
				t.Fatal(err)
			}
			run, err = store.StartRun(ctx, task, "im", "historical")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.FinishRun(ctx, "default", run.ID, "interrupted"); err != nil {
				t.Fatal(err)
			}
		}
		_, route := newRoutedChoice(t, store, action, task, run)
		want := AdmissionSnapshot{Version: AdmissionSnapshotVersion, ApprovalMode: "smart",
			Strategy: AdmissionStrategy{Class: "general_task", ToolMode: "full", PlanPolicy: "optional", WebPolicy: "disabled", ChannelMode: "im"},
			Input:    AdmissionInput{Summary: route.Queued.Content}}
		route.Queued.Admission = want
		result, err := store.RoutePendingTurnChoice(ctx, route)
		if err != nil || result.Queued == nil {
			t.Fatalf("%s: route = %+v err=%v", action, result, err)
		}
		if result.Queued.Admission != want {
			t.Fatalf("%s: routed row returned admission %+v", action, result.Queued.Admission)
		}
		stored, err := store.GetQueued(ctx, "default", result.Queued.ID)
		if err != nil || stored == nil || stored.Admission != want || stored.Admission.Input.Summary != "original" {
			t.Fatalf("%s: stored choice row = %+v err=%v, want the person's own words", action, stored, err)
		}
	}
}

// A claimed queue row's Run carries that row's snapshot verbatim. The caller
// may restate it but cannot start the Run under another one, and a row
// admitted before snapshots existed takes the caller's derivation.
func TestClaimedRunCarriesItsQueueRowsAdmission(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, task := admissionOwnerAndThread(t, store, "owner")
	want := verifyOnlyCronAdmission()
	claim := func(admission AdmissionSnapshot) (*QueuedTask, string) {
		t.Helper()
		row, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID,
			Platform: "cli", PlatformUserID: "owner", Channel: "cli", Content: "check the nightly export", Admission: admission})
		if err != nil {
			t.Fatal(err)
		}
		token, ok, err := store.ClaimQueued(ctx, owner.TenantID, row.ID, time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim = %v %v", ok, err)
		}
		return row, token
	}

	row, token := claim(want)
	if _, err := store.StartRunWithOptions(ctx, task, "cli", "x", StartRunOptions{QueueID: row.ID, QueueClaimToken: token,
		Admission: otherAdmission()}); !errors.Is(err, ErrQueuedAdmissionMismatch) {
		t.Fatalf("a claimed Run started under another snapshot: %v", err)
	}
	if current, err := store.GetQueued(ctx, owner.TenantID, row.ID); err != nil || current.RunID != "" {
		t.Fatalf("refused start bound the row: %+v %v", current, err)
	}
	run, err := store.StartRunWithOptions(ctx, task, "cli", "x", StartRunOptions{QueueID: row.ID, QueueClaimToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.RunAdmissionSnapshot(ctx, owner.TenantID, run.ID); err != nil || got != want {
		t.Fatalf("claimed Run admission = %+v err=%v, want the queue row's", got, err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}

	legacy, legacyToken := claim(AdmissionSnapshot{})
	derived := otherAdmission()
	legacyRun, err := store.StartRunWithOptions(ctx, task, "cli", "x", StartRunOptions{QueueID: legacy.ID, QueueClaimToken: legacyToken, Admission: derived})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.RunAdmissionSnapshot(ctx, owner.TenantID, legacyRun.ID); err != nil || got != derived {
		t.Fatalf("legacy row's Run admission = %+v err=%v, want the derived one", got, err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, legacyRun.ID, "done"); err != nil {
		t.Fatal(err)
	}

	// A row whose snapshot became unreadable after its claim never starts.
	broken, brokenToken := claim(want)
	if _, err := store.db.ExecContext(ctx, `UPDATE task_queue SET admission_json = '{"version":99}' WHERE id = ?`, broken.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "cli", "x", StartRunOptions{QueueID: broken.ID, QueueClaimToken: brokenToken,
		Admission: otherAdmission()}); !errors.Is(err, ErrAdmissionSnapshotUnreadable) {
		t.Fatalf("an unreadable claimed row started a Run: %v", err)
	}
}
