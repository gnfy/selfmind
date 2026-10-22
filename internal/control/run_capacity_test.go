package control

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestConcurrentRunCapacityAdmissionSingleWinner(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	owner, err := first.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	task, err := first.CreateTask(ctx, TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "A", Channel: "session-a"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := first.CreateTask(ctx, TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "B", Channel: "session-b"})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, attempt := range []struct {
		store *Store
		task  *Task
	}{{first, task}, {second, other}} {
		wg.Add(1)
		go func(i int, attempt struct {
			store *Store
			task  *Task
		}) {
			defer wg.Done()
			<-start
			_, err := attempt.store.StartRunWithOptions(ctx, attempt.task, "session", "attempt", StartRunOptions{MaxActiveRuns: 1})
			results <- err
		}(i, attempt)
	}
	close(start)
	wg.Wait()
	close(results)
	success, full := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrRunCapacity):
			full++
		default:
			t.Fatalf("unexpected concurrent admission error: %v", err)
		}
	}
	if success != 1 || full != 1 {
		t.Fatalf("admitted=%d capacity_rejected=%d, want 1/1", success, full)
	}
	if _, err := first.StartRunWithOptions(ctx, other, "session", "same terminal", StartRunOptions{
		MaxActiveRuns: 2, ExclusiveChannel: true,
	}); !errors.Is(err, ErrRunCapacity) {
		t.Fatalf("same CLI session bypassed durable lane check: %v", err)
	}
	if _, err := first.StartRunWithOptions(ctx, other, "session-b", "other terminal", StartRunOptions{
		MaxActiveRuns: 2, ExclusiveChannel: true,
	}); err != nil {
		t.Fatalf("other CLI session could not fill second person slot: %v", err)
	}
}

// Unfinished work remains actionable without holding execution capacity.
func TestFinishedTurnReleasesCapacityWithoutRewritingHistory(t *testing.T) {
	for _, state := range []string{"waiting_user", "waiting_external", "blocked", "verification_partial", "interrupted", "failed", "done", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			i, task, parent := humanWaitRun(t, s, "unfinished work")
			ask := pendingClarification(t, s, i, task, parent)
			if err := s.FinishRun(ctx, i.TenantID, parent.ID, state); err != nil {
				t.Fatal(err)
			}
			before, err := s.GetRun(ctx, i.TenantID, parent.ID)
			if err != nil {
				t.Fatal(err)
			}
			other, err := s.CreateTask(ctx, TaskCreate{TenantID: i.TenantID, PersonID: i.PersonID, Title: "unrelated work"})
			if err != nil {
				t.Fatal(err)
			}
			options := StartRunOptions{MaxActiveRuns: 1, ExclusiveChannel: true}
			next, err := s.StartRunWithOptions(ctx, other, "cli", "new request", options)
			if err != nil {
				t.Fatalf("ended %s turn blocked a new request: %v", state, err)
			}
			after, err := s.GetRun(ctx, i.TenantID, parent.ID)
			if err != nil || after.Status != before.Status || after.ResumesRunID != before.ResumesRunID {
				t.Fatalf("new request rewrote old work: %+v %v", after, err)
			}
			pending, err := s.GetClarifyRequest(ctx, i.TenantID, ask.ID)
			if err != nil || pending.Status != "pending" {
				t.Fatalf("unrelated request consumed human wait: %+v %v", pending, err)
			}
			options.ResumesRunID = parent.ID
			if _, err := s.StartRunWithOptions(ctx, task, "cli", "late exact continuation", options); !errors.Is(err, ErrRunCapacity) {
				t.Fatalf("continuation bypassed executing work: %v", err)
			}
			if err := s.FinishRun(ctx, i.TenantID, next.ID, "done"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
