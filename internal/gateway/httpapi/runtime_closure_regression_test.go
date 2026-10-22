package httpapi

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestRuntimeClosureProviderDeadlineMustRemainResumable(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, fmt.Errorf("provider read: %w", context.DeadlineExceeded)} {
		c := &RunCoordinator{}
		outcome := c.finalizeErroredRun(context.Background(), nil, nil, nil, "session-test", err)
		if outcome.Status != "interrupted" || !outcome.Resumable {
			t.Fatalf("provider timeout became terminal: %+v", outcome)
		}
	}
}

func TestRuntimeClosureCallerCancellationRemainsTerminal(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		} else {
			cancel()
		}
		outcome := (&RunCoordinator{}).finalizeErroredRun(ctx, nil, nil, nil, "session-test", ctx.Err())
		cancel()
		if outcome.Status != "cancelled" || outcome.Resumable {
			t.Fatalf("caller termination became resumable: %+v", outcome)
		}
	}
}

func TestRuntimeClosureOtherSessionCannotHoldTheAskingRun(t *testing.T) {
	daemon, _, _, owner := newHoldServer(t)
	_, detach := daemon.events().subscribe(owner.PersonID, "other-session", "")
	defer detach()
	daemon.presenceTracker().Touch(owner.PersonID, "cli")
	settings := newHumanWaitSettings([]humanWaitOption{withHumanWaitHold("asking-session")})
	if hold := daemon.coordinator().startHumanWaitHold(context.Background(), settings, owner, time.Minute); hold != nil {
		t.Fatal("another CLI's presence held a wait whose asking session is detached")
	}
}

func TestRuntimeClosureHoldEndsWhenAskingSessionLeavesEvenIfAnotherStays(t *testing.T) {
	daemon, _, _, owner := newHoldServer(t)
	_, leave := daemon.events().subscribe(owner.PersonID, "asking-session", "")
	_, other := daemon.events().subscribe(owner.PersonID, "other-session", "")
	defer other()
	settings := newHumanWaitSettings([]humanWaitOption{withHumanWaitHold("asking-session")})
	hold := daemon.coordinator().startHumanWaitHold(context.Background(), settings, owner, time.Minute)
	if hold == nil {
		t.Fatal("asking session should hold")
	}
	leave()
	daemon.presenceTracker().Touch(owner.PersonID, "cli")
	if got := hold.parkReason(context.Background()); got != "client_detached" {
		t.Fatalf("detached asking session: %q", got)
	}
}
