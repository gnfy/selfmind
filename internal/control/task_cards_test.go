package control

import (
	"context"
	"testing"
)

func TestListTaskCards(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Me")
	if err != nil {
		t.Fatal(err)
	}

	mk := func(title, status, summary string) *Task {
		task, err := store.CreateTask(ctx, TaskCreate{
			TenantID: identity.TenantID, PersonID: identity.PersonID, Title: title, Channel: "cli",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateTaskStatus(ctx, identity.TenantID, task.ID, status, summary, nil); err != nil {
			t.Fatal(err)
		}
		return task
	}
	kept := mk("tetris game", "in_progress", "rotation logic done")
	if _, err := store.SaveHandoff(ctx, Handoff{
		TaskID: kept.ID, Summary: "handoff: scoring next", ChangedFiles: []string{"tetris.html"},
	}); err != nil {
		t.Fatal(err)
	}
	// Two handoffs: the card must carry the LATEST one.
	if _, err := store.SaveHandoff(ctx, Handoff{
		TaskID: kept.ID, Summary: "handoff: scoring implemented", ChangedFiles: []string{"tetris.html", "score.js"},
	}); err != nil {
		t.Fatal(err)
	}
	abandoned := mk("abandoned", "cancelled", "gone")
	if err := NewWorkTimeline(store).Archive(ctx, identity.TenantID, identity.PersonID, abandoned.ID); err != nil {
		t.Fatal(err)
	}
	mk("filed away", "archived", "gone")
	other := mk("stock summary", "completed", "sent report")

	cards, err := store.ListTaskCards(ctx, identity.TenantID, identity.PersonID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards (archived history excluded), got %d: %+v", len(cards), cards)
	}
	byID := map[string]TaskCard{}
	for _, card := range cards {
		byID[card.TaskID] = card
	}
	got, ok := byID[kept.ID]
	if !ok {
		t.Fatalf("kept task missing from cards: %+v", cards)
	}
	if got.Summary != "rotation logic done" {
		t.Fatalf("card summary = %q", got.Summary)
	}
	if got.HandoffSummary != "handoff: scoring implemented" {
		t.Fatalf("card must carry the latest handoff, got %q", got.HandoffSummary)
	}
	if len(got.ChangedFiles) != 2 || got.ChangedFiles[1] != "score.js" {
		t.Fatalf("card changed files = %v", got.ChangedFiles)
	}
	if _, ok := byID[other.ID]; !ok {
		t.Fatalf("completed task must stay recallable: %+v", cards)
	}
}
