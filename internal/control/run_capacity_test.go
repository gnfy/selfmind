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
