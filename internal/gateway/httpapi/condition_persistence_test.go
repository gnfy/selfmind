package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel/llm"
)

func TestCriticalOutcomeEventFailureCannotCloseWork(t *testing.T) {
	for _, eventType := range []string{"run.outcome", "evidence.recorded"} {
		t.Run(eventType, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			store := controltest.NewStoreInDir(t, dir)
			defer store.Close()
			raw, err := sql.Open("sqlite", filepath.Join(dir, "control.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(`CREATE TRIGGER fail_critical_event BEFORE INSERT ON task_events WHEN NEW.type='` + eventType + `' BEGIN SELECT RAISE(ABORT,'injected critical write failure'); END`); err != nil {
				t.Fatal(err)
			}
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Me")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "inspect", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "cli", "inspect")
			if err != nil {
				t.Fatal(err)
			}
			daemon := &Server{Control: store, DefaultTenantID: "default"}
			stream := make(chan llm.StreamEvent, 4)
			stream <- llm.StreamEvent{EventType: eventType, Payload: map[string]interface{}{"status": "done"}}
			stream <- llm.StreamEvent{Err: errors.New("transport closed after the effect")}
			stream <- llm.StreamEvent{EventType: "stream", Content: "All done."}
			close(stream)
			_, _, _, _, err = daemon.coordinator().aggregateGatewayResponse(ctx, "cli", task, run, &router.HandleResponse{IsStreaming: true, Stream: stream})
			if !errors.Is(err, errRunResultCommit) || len(stream) != 0 {
				t.Fatalf("lost failure or abandoned producer: err=%v remaining=%d", err, len(stream))
			}
			outcome := daemon.coordinator().finalizeErroredRun(ctx, identity, task, run, "cli", err)
			fresh, readErr := store.GetRun(ctx, identity.TenantID, run.ID)
			if readErr != nil || fresh.Status == "done" || outcome.CompletionReason != "finalization_failed" || !outcome.Resumable {
				t.Fatalf("failed write became completion: run=%+v outcome=%+v err=%v", fresh, outcome, readErr)
			}
			// Persistent storage failure may also prevent the blocked transaction.
			// Restoring storage can save that unresolved result without dispatching
			// any work again.
			if _, err := raw.Exec(`DROP TRIGGER fail_critical_event`); err != nil {
				t.Fatal(err)
			}
			daemon.coordinator().finalizeErroredRun(ctx, identity, task, run, "cli", err)
			fresh, readErr = store.GetRun(ctx, identity.TenantID, run.ID)
			if readErr != nil || fresh.Status != "blocked" {
				t.Fatalf("storage recovery lost unfinished work: %+v %v", fresh, readErr)
			}
		})
	}
}

func TestStoredNegativeFindingSurvivesNewClientWithoutNotification(t *testing.T) {
	_, store, identity, run := conditionAssessmentFixture(t, false, 1)
	recordNegativeAssessment(t, store, run)
	// No live observer or push is needed to recover the result from the durable
	// store. Recreating the client-side coordinator must not reinterpret exit 1.
	other := &Server{Control: store, DefaultTenantID: "default"}
	verdict, _ := other.coordinator().evidenceOutcome(context.Background(), identity.TenantID, run.ID)
	if verdict == nil || verdict.ConditionState != "not_met" || verificationRequiresResume(verdict, nil) {
		t.Fatalf("saved finding lost after client replacement: %+v", verdict)
	}
}
