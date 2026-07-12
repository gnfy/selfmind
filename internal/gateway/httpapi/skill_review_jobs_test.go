package httpapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/control"
)

type fakeReviewRunner struct {
	calls int
	fail  bool
}

func (f *fakeReviewRunner) RunReviewFromPayload(_ context.Context, _, _ string) (string, error) {
	f.calls++
	if f.fail {
		return "", fmt.Errorf("provider unavailable")
	}
	return "memory updated", nil
}

// TestSkillReviewJobsDurable: enqueue is idempotent by key, the worker pass
// claims and completes a job exactly once, and failures stay pending with a
// retry horizon instead of vanishing (W7).
func TestSkillReviewJobsDurable(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	inserted, err := store.EnqueueMaintenanceJob(ctx, "tenant", "skillreview_abc", SkillReviewJobVersion, `{"channel":"cli"}`)
	if err != nil || !inserted {
		t.Fatalf("first enqueue must insert: %v %v", inserted, err)
	}
	if dup, _ := store.EnqueueMaintenanceJob(ctx, "tenant", "skillreview_abc", SkillReviewJobVersion, `{"channel":"cli"}`); dup {
		t.Fatal("duplicate key must not insert a second job")
	}

	runner := &fakeReviewRunner{}
	d := &Server{Control: store, SkillReviewer: runner}
	d.runSkillReviewPass(ctx)
	if runner.calls != 1 {
		t.Fatalf("expected one review execution, got %d", runner.calls)
	}
	job, err := store.GetMaintenanceJob(ctx, "tenant", "skillreview_abc", SkillReviewJobVersion)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != control.MaintenanceJobSucceeded || job.ResultHash == "" {
		t.Fatalf("job must complete with a result hash: %+v", job)
	}
	// Completed jobs are not re-run.
	d.runSkillReviewPass(ctx)
	if runner.calls != 1 {
		t.Fatalf("completed job must not re-run: %d", runner.calls)
	}
}

func TestSkillReviewJobFailureRetries(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.EnqueueMaintenanceJob(ctx, "tenant", "skillreview_fail", SkillReviewJobVersion, `{"channel":"cli"}`); err != nil {
		t.Fatal(err)
	}
	runner := &fakeReviewRunner{fail: true}
	d := &Server{Control: store, SkillReviewer: runner}
	d.runSkillReviewPass(ctx)

	job, err := store.GetMaintenanceJob(ctx, "tenant", "skillreview_fail", SkillReviewJobVersion)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != control.MaintenanceJobFailed && job.Status != control.MaintenanceJobPending {
		t.Fatalf("failed review must stay retryable: %+v", job)
	}
	if job.Attempts != 1 || !strings.Contains(job.LastError, "provider unavailable") {
		t.Fatalf("failure must be recorded with attempts: %+v", job)
	}
}
