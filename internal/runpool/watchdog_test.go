package runpool

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWatchdogCancelsOnStall(t *testing.T) {
	ctx, _, stop := WithWatchdog(context.Background(), 40*time.Millisecond)
	defer stop()
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrStalled) {
			t.Fatalf("cause = %v, want ErrStalled", context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("watchdog did not cancel a stalled run")
	}
}

func TestWatchdogActivityKeepsAlive(t *testing.T) {
	ctx, activity, stop := WithWatchdog(context.Background(), 50*time.Millisecond)
	defer stop()
	// Signal activity every 20ms for ~200ms; the run must stay alive throughout.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		activity()
		if ctx.Err() != nil {
			t.Fatal("watchdog cancelled a run that was making progress")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Stop signaling → it should stall shortly after.
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrStalled) {
			t.Fatalf("cause = %v, want ErrStalled", context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("watchdog did not cancel after activity stopped")
	}
}

func TestWatchdogDisabled(t *testing.T) {
	ctx, activity, stop := WithWatchdog(context.Background(), 0)
	defer stop()
	activity() // no-op, must not panic
	time.Sleep(30 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("disabled watchdog must never cancel")
	}
}

func TestWatchdogStopDoesNotStall(t *testing.T) {
	ctx, _, stop := WithWatchdog(context.Background(), time.Hour)
	stop() // normal completion
	select {
	case <-ctx.Done():
		if errors.Is(context.Cause(ctx), ErrStalled) {
			t.Fatal("normal stop must not report a stall")
		}
	case <-time.After(time.Second):
		t.Fatal("stop should cancel the watchdog context")
	}
	stop() // idempotent
}

func TestWatchdogParentCancelPropagates(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, _, stop := WithWatchdog(parent, time.Hour)
	defer stop()
	cancelParent()
	select {
	case <-ctx.Done():
		if errors.Is(context.Cause(ctx), ErrStalled) {
			t.Fatal("parent cancel must not be reported as a stall")
		}
	case <-time.After(time.Second):
		t.Fatal("parent cancel should propagate to the child")
	}
}
