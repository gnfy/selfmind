package router

import (
	"context"
	"errors"
	"testing"

	"selfmind/internal/runpool"
)

func TestWatchdogErrorPreservesStalledCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(runpool.ErrStalled)

	err := watchdogError(ctx)
	if !errors.Is(err, runpool.ErrStalled) {
		t.Fatalf("watchdog error %v does not wrap ErrStalled", err)
	}
}
