package control

import (
	"context"
	"testing"
)

func TestListRecentRunsForPerson(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Ship it", Channel: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "do the thing")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}

	runs, err := store.ListRecentRunsForPerson(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("ListRecentRunsForPerson = %d; want 1", len(runs))
	}
	if runs[0].TaskTitle != "Ship it" {
		t.Fatalf("run task title = %q; want Ship it", runs[0].TaskTitle)
	}
	if runs[0].FinishedAt == nil {
		t.Fatalf("finished run should carry FinishedAt")
	}
	if runs[0].Elapsed() < 0 {
		t.Fatalf("elapsed should be non-negative")
	}
}

func TestCountChannelMessagesByChannel(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := store.RecordChannelMessage(ctx, *identity, "cli", "task_x", "user", "hi"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordChannelMessage(ctx, *identity, "telegram", "task_x", "user", "hi"); err != nil {
		t.Fatal(err)
	}
	counts, err := store.CountChannelMessagesByChannel(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 2 || counts[0].Channel != "cli" || counts[0].Count != 3 {
		t.Fatalf("counts = %+v; want cli=3 busiest", counts)
	}
}
