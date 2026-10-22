package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
)

func TestQueuedRunCreationFailureRetainsInputAndNotifiesOnlyItsSession(t *testing.T) {
	testQueuedRunCreationFailure(t, false)
}

func TestQueuedRunCreationAndClaimReturnFailuresRecoverTogether(t *testing.T) {
	testQueuedRunCreationFailure(t, true)
}

func testQueuedRunCreationFailure(t *testing.T, returnWriteFails bool) {
	t.Helper()
	s := newAdmissionProbeServer(t)
	ctx := context.Background()
	row, err := s.store.EnqueueQueued(ctx, control.QueuedTask{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
		Platform: "cli", PlatformUserID: "local", Channel: "request-session", Content: "inspect the independent report"})
	if err != nil {
		t.Fatal(err)
	}
	owner, closeOwner := s.daemon.events().subscribe(s.identity.PersonID, row.Channel, "")
	defer closeOwner()
	other, closeOther := s.daemon.events().subscribe(s.identity.PersonID, "other-session", "")
	defer closeOther()
	s.daemon.presenceTracker().Touch(s.identity.PersonID, "cli")
	s.execRaw(t, `CREATE TRIGGER refuse_run_creation BEFORE INSERT ON runs
		BEGIN SELECT RAISE(ABORT,'injected Run creation failure'); END`)
	if returnWriteFails {
		s.execRaw(t, `CREATE TRIGGER refuse_startup_return BEFORE UPDATE OF status ON task_queue
			WHEN OLD.status='started' AND NEW.status='queued'
			BEGIN SELECT RAISE(ABORT,'injected claim-return failure'); END`)
	}
	s.daemon.coordinator().drainQueue(s.identity)
	waitUntil(t, time.Second, func() bool { return s.daemon.ActiveRunCount() == 0 }, "failed startup retained an active slot")
	saved, err := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
	wantStatus := control.QueueStatusQueued
	if returnWriteFails {
		wantStatus = control.QueueStatusStarted
	}
	if err != nil || saved.Status != wantStatus || saved.RunID != "" || saved.Content != row.Content || saved.AttemptGeneration != 1 {
		t.Fatalf("unstarted request was silently settled or retried immediately: %+v, %v", saved, err)
	}
	select {
	case event := <-owner.ch:
		if event.Type != "queue.start_failed" || event.Channel != row.Channel ||
			!strings.Contains(string(event.Payload), "retained") || !strings.Contains(string(event.Payload), "/queue") {
			t.Fatalf("startup failure was not actionable: %+v", event)
		}
	default:
		t.Fatal("CLI-only owner did not receive the startup failure")
	}
	select {
	case event := <-other.ch:
		t.Fatalf("startup details leaked to another session: %+v", event)
	default:
	}
	s.provider.mu.Lock()
	calls := len(s.provider.probes)
	s.provider.mu.Unlock()
	if calls != 0 {
		t.Fatal("pre-Run failure dispatched the model")
	}
	s.execRaw(t, `DROP TRIGGER refuse_run_creation`)
	if returnWriteFails {
		s.execRaw(t, `DROP TRIGGER refuse_startup_return`)
	}
	s.daemon.runExternalWatchPass(ctx)
	waitUntil(t, 3*time.Second, func() bool {
		saved, err = s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
		return err == nil && saved.Status == control.QueueStatusDone && saved.RunID != "" && s.daemon.ActiveRunCount() == 0
	}, "healthy periodic drain did not execute the retained request")
	if saved.AttemptGeneration != 2 {
		t.Fatalf("startup recovery replayed the request: %+v", saved)
	}
}

func TestPeriodicClaimCleanupPreservesLiveBoundAndReassignedRows(t *testing.T) {
	for _, variant := range []string{"live", "bound", "reassigned", "cancelled"} {
		t.Run(variant, func(t *testing.T) {
			s := newAdmissionProbeServer(t)
			ctx := context.Background()
			row, err := s.store.EnqueueQueued(ctx, control.QueuedTask{TenantID: s.identity.TenantID, PersonID: s.identity.PersonID,
				Platform: "cli", PlatformUserID: "local", Channel: "request-session", Content: "inspect the report"})
			if err != nil {
				t.Fatal(err)
			}
			token, claimed, err := s.store.ClaimQueued(ctx, s.identity.TenantID, row.ID, time.Second)
			if err != nil || !claimed {
				t.Fatalf("claim: %v, %v", claimed, err)
			}
			if variant != "live" {
				s.execRaw(t, `CREATE TRIGGER refuse_cleanup BEFORE UPDATE OF status ON task_queue
					WHEN OLD.status='started' AND NEW.status='queued' BEGIN SELECT RAISE(ABORT,'injected return failure'); END`)
				if _, err := s.daemon.coordinator().returnUnboundQueueClaim(ctx, s.identity.TenantID, row.ID, token); err == nil {
					t.Fatal("the injected claim-return fault did not fire")
				}
				s.execRaw(t, `DROP TRIGGER refuse_cleanup`)
			}
			switch variant {
			case "bound":
				s.execRaw(t, `UPDATE task_queue SET run_id='bound-run' WHERE id=?`, row.ID)
			case "reassigned":
				s.execRaw(t, `UPDATE task_queue SET claim_token='new-owner', attempt_generation=attempt_generation+1 WHERE id=?`, row.ID)
			case "cancelled":
				s.execRaw(t, `UPDATE task_queue SET status='cancelled' WHERE id=?`, row.ID)
			}
			// Even an expired lease is not proof that an unbound worker stopped.
			s.execRaw(t, `UPDATE task_queue SET lease_until=? WHERE id=?`, time.Now().Add(-time.Minute).Unix(), row.ID)
			before, _ := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
			s.daemon.runExternalWatchPass(ctx)
			s.daemon.runExternalWatchPass(ctx)
			after, err := s.store.GetQueued(ctx, s.identity.TenantID, row.ID)
			if err != nil || after.Status != before.Status || after.ClaimToken != before.ClaimToken || after.RunID != before.RunID || after.AttemptGeneration != before.AttemptGeneration {
				t.Fatalf("periodic cleanup changed another owner: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}
