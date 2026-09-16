package delivery

import (
	"context"
	"errors"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
)

// This boundary uses the real store: repairing notification delivery must not
// reopen a completed tool, consume accepted input, or settle unfinished work.
func TestNotificationFailurePreservesCommittedRunAndToolOutcome(t *testing.T) {
	for _, status := range []string{"done", "waiting_user"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			store := controltest.NewStore(t)
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			// Only fixture construction uses the legacy grouping API. Execution,
			// result and delivery assertions below are all keyed to this exact Run.
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "delivery boundary", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "cli", "prepare artifact")
			if err != nil {
				t.Fatal(err)
			}
			entry := control.ToolLedgerEntry{RunID: run.ID, ToolCallID: "write", ToolName: "write_file", ArgsHash: "frozen-args", RetryClass: "idempotent"}
			claim, err := store.ClaimToolDispatch(ctx, identity.TenantID, entry)
			if err != nil || !claim.Execute {
				t.Fatalf("claim=%+v err=%v", claim, err)
			}
			if err := store.RecordToolOutcome(ctx, identity.TenantID, run.ID, "write", true); err != nil {
				t.Fatal(err)
			}
			input, err := store.AcceptSteering(ctx, control.SteeringMessage{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, Content: "additional accepted input"})
			if err != nil {
				t.Fatal(err)
			}
			outcome := control.RunFinalization{Identity: *identity, RunID: run.ID, TaskID: task.ID, RunStatus: status, TaskStatus: status, Summary: "artifact recorded", NextSteps: []string{"user decision remains"}}
			first, err := store.MaterializeRunFinalization(ctx, outcome)
			if err != nil {
				t.Fatal(err)
			}
			sends := 0
			service := NewService(store, SenderFunc(func(context.Context, Message) error {
				sends++
				if sends == 1 {
					return errors.New("notification transport unavailable")
				}
				return nil
			}), Options{RetryBaseDelay: time.Nanosecond})
			message := Message{TenantID: identity.TenantID, PersonID: identity.PersonID, RunID: run.ID, Platform: "weixin", PlatformUserID: "wx-local", Channel: "weixin", Kind: KindFinalResult, Content: "artifact recorded"}
			if err := service.EnqueueAndTry(ctx, message); err == nil {
				t.Fatal("notification failure hidden")
			}
			stored, err := store.GetRun(ctx, identity.TenantID, run.ID)
			if err != nil || stored.Status != status {
				t.Fatalf("notification changed Run: %+v %v", stored, err)
			}
			second, err := store.MaterializeRunFinalization(ctx, outcome)
			if err != nil || first.ID != second.ID {
				t.Fatalf("commit replay duplicated outcome: %v", err)
			}
			if err := service.EnqueueAndTry(ctx, message); err != nil {
				t.Fatal(err)
			}
			if sends != 2 {
				t.Fatalf("sends=%d", sends)
			}
			claim, err = store.ClaimToolDispatch(ctx, identity.TenantID, entry)
			if err != nil || claim.Execute || claim.Status != "completed" {
				t.Fatalf("notification repair reopened effect: %+v %v", claim, err)
			}
			pending, err := store.ListUnconsumedSteering(ctx, identity.TenantID, run.ID, 10)
			if err != nil || len(pending) != 1 || pending[0].ID != input.ID {
				t.Fatalf("accepted input lost: %+v %v", pending, err)
			}
			stored, err = store.GetRun(ctx, identity.TenantID, run.ID)
			if err != nil || stored.Status != status {
				t.Fatalf("notification repair changed Run: %+v %v", stored, err)
			}
		})
	}
}
