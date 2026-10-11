package control

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPendingTurnChoiceHandoffDoesNotConsumeChangedOwnership(t *testing.T) {
	for _, change := range []string{"token", "bound", "cancelled", "foreign_thread"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			store := newTestStore(t)
			owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "owner", "Owner")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "unfinished work"})
			if err != nil {
				t.Fatal(err)
			}
			row, err := store.EnqueueQueued(ctx, QueuedTask{TenantID: owner.TenantID, PersonID: owner.PersonID,
				Platform: "cli", PlatformUserID: "owner", Channel: "origin-session", Content: "new correction"})
			if err != nil {
				t.Fatal(err)
			}
			token, claimed, err := store.ClaimQueued(ctx, owner.TenantID, row.ID, time.Minute)
			if err != nil || !claimed {
				t.Fatalf("claim: %v, %v", claimed, err)
			}
			notice := &PendingTurnChoiceNotice{TaskID: task.ID, Question: "Which work?", QueueID: row.ID, QueueClaimToken: token}
			switch change {
			case "token":
				_, err = store.db.Exec(`UPDATE task_queue SET claim_token='new-owner' WHERE id=?`, row.ID)
			case "bound":
				_, err = store.db.Exec(`UPDATE task_queue SET run_id='bound-run' WHERE id=?`, row.ID)
			case "cancelled":
				_, err = store.db.Exec(`UPDATE task_queue SET status='cancelled' WHERE id=?`, row.ID)
			case "foreign_thread":
				stranger, resolveErr := store.ResolveOrCreateAccount(ctx, "other-tenant", "cli", "stranger", "Stranger")
				if resolveErr != nil {
					t.Fatal(resolveErr)
				}
				foreign, createErr := store.CreateTask(ctx, TaskCreate{TenantID: stranger.TenantID, PersonID: stranger.PersonID, Title: "private work"})
				if createErr != nil {
					t.Fatal(createErr)
				}
				notice.TaskID = foreign.ID
			}
			if err != nil {
				t.Fatal(err)
			}
			before, _ := store.GetQueued(ctx, owner.TenantID, row.ID)
			_, err = store.CreatePendingTurnChoice(ctx, PendingTurnChoiceCreate{TenantID: owner.TenantID, PersonID: owner.PersonID,
				AccountID: owner.AccountID, Channel: row.Channel, RequestJSON: `{"content":"new correction"}`, Notice: notice,
				Options: []TurnChoiceOption{{Key: "1", Label: "New work", Action: "new"}, {Key: "2", Label: "Another new work", Action: "new"}}})
			if err == nil || (change != "foreign_thread" && !errors.Is(err, ErrQueueClaimLost)) {
				t.Fatalf("changed ownership accepted a handoff: %v", err)
			}
			after, err := store.GetQueued(ctx, owner.TenantID, row.ID)
			if err != nil || after.Status != before.Status || after.ClaimToken != before.ClaimToken || after.RunID != before.RunID {
				t.Fatalf("handoff changed another owner's row: %+v, %v", after, err)
			}
			if _, err := store.PeekBareTurnChoice(ctx, owner.TenantID, owner.PersonID,
				TurnChoiceEndpoint{AccountID: owner.AccountID, Channel: row.Channel}, time.Now(), time.Hour); !errors.Is(err, ErrTurnChoiceNotFound) {
				t.Fatalf("refused handoff left an answerable choice: %v", err)
			}
			events, err := store.ListPersonEventsAfter(ctx, owner.TenantID, owner.PersonID, 0, 100)
			if err != nil || len(events) != 0 {
				t.Fatalf("refused handoff left a notification: %+v, %v", events, err)
			}
		})
	}
}
