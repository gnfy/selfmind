package httpapi

import (
	"context"
	"fmt"
	"testing"
	"time"

	"selfmind/internal/control"
)

func TestQueuedSynchronousAdmissionRefusalRetainsRetryableInput(t *testing.T) {
	testQueuedAdmissionRefusal(t, false)
}

func TestQueuedClaimReturnWriteFailureRecoversOnPeriodicDrain(t *testing.T) {
	testQueuedAdmissionRefusal(t, true)
}

func testQueuedAdmissionRefusal(t *testing.T, returnWriteFails bool) {
	t.Helper()
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	task, err := s.store.CreateTask(ctx, control.TaskCreate{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID, Title: "reservation fixture", Channel: "other-session"})
	if err != nil {
		t.Fatal(err)
	}
	victim, err := s.store.StartRun(ctx, task, "other-session", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.FinishRun(ctx, s.identity.TenantID, victim.ID, "interrupted"); err != nil {
		t.Fatal(err)
	}
	row, err := s.store.EnqueueQueued(ctx, control.QueuedTask{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		Platform: "cli", PlatformUserID: "local", Channel: "request-session", Content: "inspect the independent report"})
	if err != nil {
		t.Fatal(err)
	}
	// The drain's first read succeeds; only the read after claim commit fails.
	s.execRaw(t, fmt.Sprintf(`CREATE TRIGGER refuse_queued_admission AFTER UPDATE OF status ON task_queue
		WHEN NEW.id='%s' AND NEW.status='started' BEGIN
		UPDATE runs SET status='running', execution_roots_json='invalid-root-json' WHERE id='%s'; END`, row.ID, victim.ID))
	if returnWriteFails {
		s.execRaw(t, fmt.Sprintf(`CREATE TRIGGER refuse_claim_return BEFORE UPDATE OF status ON task_queue
			WHEN NEW.id='%s' AND OLD.status='started' AND NEW.status='queued'
			BEGIN SELECT RAISE(ABORT,'injected claim-return write failure'); END`, row.ID))
	}
	s.daemon.coordinator().drainQueue(s.identity)
	saved, err := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
	wantStatus := control.QueueStatusQueued
	if returnWriteFails {
		wantStatus = control.QueueStatusStarted
	}
	if err != nil || saved.Status != wantStatus || saved.RunID != "" || (!returnWriteFails && saved.ClaimToken != "") ||
		saved.Content != row.Content || saved.AttemptGeneration != 1 || s.daemon.ActiveRunCount() != 0 {
		t.Fatalf("synchronous refusal stranded or changed input: %+v, %v", saved, err)
	}
	// No immediate retry spin; the healthy periodic drain starts the same row.
	s.execRaw(t, `DROP TRIGGER refuse_queued_admission`)
	s.execRaw(t, `UPDATE runs SET status='interrupted', execution_roots_json='[]' WHERE id=?`, victim.ID)
	if returnWriteFails {
		s.execRaw(t, `DROP TRIGGER refuse_claim_return`)
	}
	s.daemon.runExternalWatchPass(ctx)
	waitUntil(t, 3*time.Second, func() bool {
		saved, err = s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
		return err == nil && saved.Status == control.QueueStatusDone && saved.RunID != "" && s.daemon.ActiveRunCount() == 0
	}, "healthy periodic drain did not recover the original input")
	if saved.AttemptGeneration != 2 {
		t.Fatalf("unexpected replay count: %+v", saved)
	}
	pending, err := s.store.HasPendingQueuedSource(ctx, s.identity.TenantID, s.identity.PersonID, row.Platform, row.PlatformUserID, row.Channel)
	if err != nil || pending {
		t.Fatalf("recovered input still blocks its source: %v, %v", pending, err)
	}
}
