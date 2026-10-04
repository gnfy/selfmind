package control

import (
	"context"
	"errors"
	"testing"
)

func TestCoordinationRunHasIndependentCapacityAndNoWorkUnit(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	work, err := store.StartRunWithOptions(ctx, task, "cli-a", "work", StartRunOptions{MaxActiveRuns: 1})
	if err != nil {
		t.Fatal(err)
	}
	coordOwner := RunOwner{TenantID: owner.TenantID, PersonID: owner.PersonID}
	coord, err := store.StartRunForOwner(ctx, coordOwner, "im", "route input", StartRunOptions{ExecutionClass: "coordination"})
	if err != nil {
		t.Fatalf("coordination should not consume work capacity: %v", err)
	}
	if coord.TaskID != "" || coord.WorkUnitID != "" || coord.ExecutionClass != "coordination" {
		t.Fatalf("coordination acquired a work identity: %+v", coord)
	}
	if _, err := store.StartRunForOwner(ctx, coordOwner, "im", "other routing", StartRunOptions{ExecutionClass: "coordination"}); !errors.Is(err, ErrRunCapacity) {
		t.Fatalf("second coordination slot: %v", err)
	}
	if _, err := store.StartRunWithOptions(ctx, task, "cli-b", "more work", StartRunOptions{MaxActiveRuns: 1}); !errors.Is(err, ErrRunCapacity) {
		t.Fatalf("coordination bypassed work cap: %v", err)
	}
	current, err := store.CurrentTask(ctx, owner.TenantID, owner.PersonID)
	if err != nil || current == nil || current.ID != task.ID {
		t.Fatalf("coordination changed current work: %+v, %v", current, err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, coord.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, owner.TenantID, work.ID, "done"); err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetRun(ctx, owner.TenantID, coord.ID)
	if err != nil || stored == nil || stored.ExecutionClass != "coordination" || stored.Status != "done" {
		t.Fatalf("coordination audit run: %+v, %v", stored, err)
	}
}

func TestCrashedCoordinationRunCannotBecomeResumableWork(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	owner, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRunForOwner(ctx, RunOwner{TenantID: owner.TenantID, PersonID: owner.PersonID}, "im", "route input", StartRunOptions{ExecutionClass: "coordination"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := store.MarkInterruptedRuns(ctx, 0); err != nil || n != 1 {
		t.Fatalf("interrupt coordination: %d, %v", n, err)
	}
	if events, err := store.ListRunEvents(ctx, owner.TenantID, owner.PersonID, run.ID, run.ID, 10); err != nil || len(events) != 0 {
		t.Fatalf("crashed routing turn was presented as resumable work: %+v, %v", events, err)
	}
	if candidates, err := store.ListExplicitlyResumableRunsForPerson(ctx, owner.TenantID, owner.PersonID, 10); err != nil || len(candidates) != 0 {
		t.Fatalf("coordination was offered as a resume target: %+v, %v", candidates, err)
	}
	if attention, err := NewWorkTimeline(store).Attention(ctx, owner.TenantID, owner.PersonID, 10); err != nil || len(attention) != 0 {
		t.Fatalf("coordination entered work Attention: %+v, %v", attention, err)
	}
	if _, err := store.StartRunForOwner(ctx, RunOwner{TenantID: owner.TenantID, PersonID: owner.PersonID}, "im", "unsafe resume",
		StartRunOptions{ResumesRunID: run.ID}); !errors.Is(err, ErrResumeTargetNotResumable) {
		t.Fatalf("coordination became a resumable parent: %v", err)
	}
}
