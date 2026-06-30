package runpool

import (
	"context"
	"errors"
	"time"
)

// ErrStalled is the cancellation cause when a run makes no progress within the
// watchdog's idle window. Callers can check context.Cause(ctx) == ErrStalled to
// distinguish a stall from a user cancel and surface an actionable handoff.
var ErrStalled = errors.New("run stalled: no progress within the idle timeout")

// WithWatchdog returns a child context that is cancelled with ErrStalled if
// activity() is not called within idle. Every progress signal (tool event,
// stream chunk, thinking) should call activity() to reset the timer; this kills
// only genuinely stuck runs, not slow-but-progressing ones, so a hung provider
// frees its worker instead of blocking the pool (W1c).
//
// idle <= 0 disables the watchdog: the parent context is returned with no-op
// activity/stop, so the default path is unchanged. The caller must defer stop()
// to release the timer goroutine (and the child context) on normal completion.
func WithWatchdog(parent context.Context, idle time.Duration) (context.Context, func(), func()) {
	if idle <= 0 {
		return parent, func() {}, func() {}
	}
	ctx, cancel := context.WithCancelCause(parent)
	reset := make(chan struct{}, 1)
	stopCh := make(chan struct{})

	go func() {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
				cancel(ErrStalled)
				return
			case <-reset:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
			case <-stopCh:
				cancel(context.Canceled)
				return
			case <-parent.Done():
				cancel(context.Cause(parent))
				return
			}
		}
	}()

	activity := func() {
		select {
		case reset <- struct{}{}:
		default: // a reset is already pending; the timer will be refreshed
		}
	}
	var stopped bool
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		close(stopCh)
	}
	return ctx, activity, stop
}
