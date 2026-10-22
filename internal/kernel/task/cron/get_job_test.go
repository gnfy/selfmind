package cron

import (
	"context"
	"errors"
	"testing"
)

// GetJob answers whether one exact job still exists and is enabled, which is
// what a fire that waited in the queue needs to know: a job that is gone is not
// the same as one whose lookup failed, and a job in another tenant is gone.
func TestGetJobDistinguishesFoundGoneAndOtherTenants(t *testing.T) {
	s := newTestScheduler(t)
	ctx := context.Background()
	id, err := s.AddJob(ctx, &CronJob{Name: "nightly", CronExpr: "0 2 * * *", Prompt: "tidy the nightly notes",
		TenantID: "default", Channel: "cli", Web: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.GetJob(ctx, "default", id)
	if err != nil || job == nil || job.ID != id || job.Prompt != "tidy the nightly notes" || !job.Web || !job.Enabled || job.Once {
		t.Fatalf("GetJob = %+v, %v", job, err)
	}

	// Disabled and one-time state is reported as stored.
	if err := s.EnableJob(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if job, err := s.GetJob(ctx, "default", id); err != nil || job == nil || job.Enabled {
		t.Fatalf("a disabled job reads as disabled: %+v, %v", job, err)
	}
	onceID, err := s.AddJob(ctx, &CronJob{Name: "reminder", CronExpr: "0 9 * * *", Prompt: "remind me", TenantID: "default", Channel: "cli", Once: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if job, err := s.GetJob(ctx, "default", onceID); err != nil || job == nil || !job.Once {
		t.Fatalf("a one-time job reads as one-time: %+v, %v", job, err)
	}

	// Another tenant's job, a removed job and an unknown id are all gone, and
	// gone is its own error, never a failed lookup.
	if _, err := s.GetJob(ctx, "other-tenant", id); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("another tenant's job must read as gone, got %v", err)
	}
	if err := s.RemoveJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetJob(ctx, "default", id); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("a removed job must read as gone, got %v", err)
	}
	if _, err := s.GetJob(ctx, "default", 987654); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("an unknown job must read as gone, got %v", err)
	}

	// A lookup that fails is an error that is not "gone".
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetJob(ctx, "default", onceID); err == nil || errors.Is(err, ErrJobNotFound) {
		t.Fatalf("a failed lookup must not read as gone: %v", err)
	}
}
