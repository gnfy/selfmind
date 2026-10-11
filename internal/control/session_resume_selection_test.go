package control

import (
	"context"
	"testing"
)

func TestRunCancellationWithdrawsOnlyItsResumeSelections(t *testing.T) {
	store := newTestStore(t)
	owner, task, run := humanWaitRun(t, store, "selection-owner")
	ctx := context.Background()
	other, err := store.StartRun(ctx, task, "other", "another operation")
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"a", "b"} {
		if err := store.PinSessionResumeSelection(ctx, owner.TenantID, owner.PersonID, session, task.ID, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PinSessionResumeSelection(ctx, owner.TenantID, owner.PersonID, "c", task.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestRunCancel(ctx, owner.TenantID, run.ID); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"a", "b", "c"} {
		got, err := store.SessionResumeSelection(ctx, owner.TenantID, owner.PersonID, session)
		if err != nil {
			t.Fatal(err)
		}
		if session == "c" {
			if got == nil || got.RunID != other.ID {
				t.Fatal("unrelated selection was withdrawn")
			}
		} else if got != nil {
			t.Fatal("cancelled selection survived")
		}
	}
}
