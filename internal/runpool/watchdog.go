package runpool

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrStalled is the cancellation cause when a run makes no progress within the
// watchdog's idle window. Callers can check context.Cause(ctx) == ErrStalled to
// distinguish a stall from a user cancel and surface an actionable handoff.
var ErrStalled = errors.New("run stalled: no progress within the idle timeout")

// Phase describes why a run may be quiet. Provider/tool work is governed by
// the idle watchdog; waits on a person have their own durable deadline and must
// not be mistaken for a stuck execution.
type Phase string

const (
	PhaseRunning         Phase = "running"
	PhaseWaitingApproval Phase = "waiting_approval"
	PhaseWaitingClarify  Phase = "waiting_clarify"
)

type watchdogController struct {
	mu        sync.Mutex
	phase     Phase
	waitDepth int
	changed   chan struct{}
}

type watchdogContextKey struct{}

func (c *watchdogController) set(phase Phase) {
	if c == nil || phase == "" {
		return
	}
	c.mu.Lock()
	c.phase = phase
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *watchdogController) current() Phase {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.phase
}

func (c *watchdogController) beginWait(phase Phase) func() {
	if c == nil || !phaseWaitsForPerson(phase) {
		return func() {}
	}
	c.mu.Lock()
	c.waitDepth++
	c.phase = phase
	c.mu.Unlock()
	c.signalChanged()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			if c.waitDepth > 0 {
				c.waitDepth--
			}
			if c.waitDepth == 0 {
				c.phase = PhaseRunning
			}
			c.mu.Unlock()
			c.signalChanged()
		})
	}
}

func (c *watchdogController) signalChanged() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

// SetPhase updates the watchdog attached to ctx. It is deliberately a no-op
// outside a watched run, so approval/clarify handlers remain reusable in tests
// and management paths.
func SetPhase(ctx context.Context, phase Phase) {
	if ctx == nil {
		return
	}
	if control, _ := ctx.Value(watchdogContextKey{}).(*watchdogController); control != nil {
		control.set(phase)
	}
}

// BeginPersonWait pauses the idle watchdog until the returned function is
// called. Pauses are reference-counted so overlapping approval and clarify
// waits cannot re-arm the watchdog while either one is still pending.
func BeginPersonWait(ctx context.Context, phase Phase) func() {
	if ctx == nil {
		return func() {}
	}
	if control, _ := ctx.Value(watchdogContextKey{}).(*watchdogController); control != nil {
		return control.beginWait(phase)
	}
	return func() {}
}

func phaseWaitsForPerson(phase Phase) bool {
	return phase == PhaseWaitingApproval || phase == PhaseWaitingClarify
}

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
	baseCtx, cancel := context.WithCancelCause(parent)
	control := &watchdogController{phase: PhaseRunning, changed: make(chan struct{}, 1)}
	ctx := context.WithValue(baseCtx, watchdogContextKey{}, control)
	reset := make(chan struct{}, 1)
	stopCh := make(chan struct{})

	go func() {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		armed := true
		resetTimer := func() {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
			armed = true
		}
		pauseTimer := func() {
			if armed && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			armed = false
		}
		for {
			select {
			case <-timer.C:
				armed = false
				if !phaseWaitsForPerson(control.current()) {
					cancel(ErrStalled)
					return
				}
			case <-reset:
				if !phaseWaitsForPerson(control.current()) {
					resetTimer()
				}
			case <-control.changed:
				if phaseWaitsForPerson(control.current()) {
					pauseTimer()
				} else {
					resetTimer()
				}
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
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() { close(stopCh) })
	}
	return ctx, activity, stop
}
