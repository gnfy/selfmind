package runpool

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPoolAdmissionWaitPausesIdleWatchdog(t *testing.T) {
	for _, resource := range []string{"key", "paths", "worker"} {
		t.Run(resource, func(t *testing.T) {
			pool := New(1)
			root := t.TempDir()
			firstStarted, release := make(chan struct{}), make(chan struct{})
			firstDone := make(chan error, 1)
			job := func() error { close(firstStarted); <-release; return nil }
			go func() {
				if resource == "paths" {
					firstDone <- pool.RunObservedPaths(context.Background(), []string{root}, nil, job)
				} else {
					firstDone <- pool.Run(context.Background(), "same", job)
				}
			}()
			<-firstStarted
			parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ctx, _, stop := WithWatchdog(parent, 40*time.Millisecond)
			defer stop()
			waiting, started, done := make(chan State, 1), make(chan struct{}), make(chan error, 1)
			observe := func(state State) {
				if state != StateRunning {
					waiting <- state
				}
			}
			run := func() error { close(started); <-ctx.Done(); return context.Cause(ctx) }
			go func() {
				if resource == "paths" {
					done <- pool.RunObservedPaths(ctx, []string{root}, observe, run)
				} else {
					key := "same"
					if resource == "worker" {
						key = "other"
					}
					done <- pool.RunObserved(ctx, key, observe, run)
				}
			}()
			select {
			case <-waiting:
			case <-time.After(time.Second):
				t.Fatal("job did not enter admission wait")
			}
			select {
			case <-ctx.Done():
				t.Errorf("admission wait hit idle watchdog: %v", context.Cause(ctx))
			case <-time.After(120 * time.Millisecond):
			}
			close(release)
			if err := <-firstDone; err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("job did not execute after admission")
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrStalled) {
					t.Fatalf("running job lost watchdog: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("execution watchdog did not resume")
			}
		})
	}
}
