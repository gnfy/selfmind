package httpapi

import (
	"context"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/platform/log"
)

// defaultHumanWaitHold is how long an unanswered approval or question may keep
// its Run in place while the person sits at the terminal that asked. An answer
// inside it continues the same Run: no checkpoint hop, no continuation Run, no
// state handed across a Run boundary. 93% of the approvals answered in the
// first week the daemon parked everything were answered within three minutes.
const defaultHumanWaitHold = 180 * time.Second

func (c *RunCoordinator) humanWaitHoldCap() time.Duration {
	switch {
	case c.srv.HumanWaitHold < 0:
		return 0
	case c.srv.HumanWaitHold > 0:
		return c.srv.HumanWaitHold
	}
	return defaultHumanWaitHold
}

// humanWaitOption configures the human-wait handlers of one Run.
type humanWaitOption func(*humanWaitSettings)

type humanWaitSettings struct{ session string }

// withHumanWaitHold lets this Run's unanswered waits stay in place; see
// humanWaitSourceCanHold for which Runs qualify.
func withHumanWaitHold(session string) humanWaitOption {
	return func(s *humanWaitSettings) { s.session = strings.TrimSpace(session) }
}

func newHumanWaitSettings(opts []humanWaitOption) humanWaitSettings {
	var settings humanWaitSettings
	for _, opt := range opts {
		opt(&settings)
	}
	return settings
}

// humanWaitSourceCanHold reports whether a person may be sitting at the
// endpoint this Run came from. Only an interactive terminal session qualifies:
// it shows the question inline and the person answers it there. IM keeps the
// short grace and parks at once, because nobody has measured how long an IM
// answer takes. `selfmind send` and API callers (the literal `cli` channel) are
// one-shot and have no panel, so holding would only block them. Work the daemon
// started that nobody is waiting on (a schedule, a watcher, a recovery) is
// never held, whatever terminal happens to be open.
func humanWaitSourceCanHold(ctx context.Context, req api.MessageRequest, identity *control.IdentityContext, channel string) bool {
	if identity == nil || !strings.EqualFold(strings.TrimSpace(identity.Platform), "cli") {
		return false
	}
	switch runOrigin(ctx, req) {
	case runOriginCron, runOriginWatch, runOriginRecovery:
		return false
	}
	switch strings.ToLower(strings.TrimPrefix(strings.TrimSpace(channel), "#")) {
	case "", "cli", "terminal", "tui":
		return false
	}
	return true
}

// humanWaitHold is the standing decision that one unanswered wait may stay in
// its Run. The waiter re-asks it on every poll and releases its worker through
// the ordinary checkpoint park the moment any condition stops holding, so a
// hold never outlives what justified it.
type humanWaitHold struct {
	c        *RunCoordinator
	identity *control.IdentityContext
	session  string
	deadline time.Time
}

// startHumanWaitHold returns the hold for a wait that may be bound by wait, or
// nil when the wait must take the short grace and park as before.
func (c *RunCoordinator) startHumanWaitHold(ctx context.Context, settings humanWaitSettings, identity *control.IdentityContext, wait time.Duration) *humanWaitHold {
	if c == nil || c.srv == nil || identity == nil || settings.session == "" || wait <= 0 {
		return nil
	}
	limit := c.humanWaitHoldCap()
	if limit <= 0 {
		return nil
	}
	if wait < limit {
		limit = wait
	}
	hold := &humanWaitHold{c: c, identity: identity, session: settings.session, deadline: time.Now().Add(limit)}
	if hold.parkReason(ctx) != "" {
		return nil
	}
	return hold
}

// parkReason is empty while the wait may stay in place; otherwise it is why the
// wait must release its worker now, as a control.HumanWaitPark* value. A daemon
// that is draining parks first: a restart waits for every Run to leave its
// worker, and an answer never needs this Run to still be there.
func (h *humanWaitHold) parkReason(ctx context.Context) string {
	switch {
	case h.c.srv.IsDraining():
		return control.HumanWaitParkDraining
	case !h.c.srv.events().sessionAttached(h.identity.PersonID, h.session):
		return control.HumanWaitParkClientDetached
	case h.c.otherWorkWaitsForASlot(ctx, h.identity):
		return control.HumanWaitParkCapacity
	case !time.Now().Before(h.deadline):
		return control.HumanWaitParkHoldCap
	}
	return ""
}

// otherWorkWaitsForASlot is the pressure signal: input of the same person that is
// queued or starting needs the slot, the writable roots or the terminal lane this
// Run is keeping. It deliberately says nothing about which of them the queued
// work needs, since a held Run is `running` for all three. A queue that cannot
// be read proves nothing, so it counts as pressure.
func (c *RunCoordinator) otherWorkWaitsForASlot(ctx context.Context, identity *control.IdentityContext) bool {
	if c.srv.Control == nil {
		return true
	}
	pollCtx, cancel := context.WithTimeout(ctx, approvalPollTimeout)
	defer cancel()
	waiting, err := c.srv.Control.HasDueQueuedWork(pollCtx, identity.TenantID, identity.PersonID)
	if err != nil {
		log.Warn("human wait cannot read the queue; its Run releases the worker", "person_id", identity.PersonID, "error", err)
		return true
	}
	return waiting
}

// armedHumanWait is a wait that wrote its exact pending-call checkpoint when it
// started. A crash while it stays in place then recovers like a park, and a park
// needs no second write. prior is the checkpoint that write replaced: when the
// answer is consumed in place the checkpoint no longer names a pending call, and
// putting prior back leaves the Run exactly as a Run that never waited, so a
// crash while the approved call runs cannot make a later resume dispatch it a
// second time. Every method is safe on a nil wait, which is one that was not armed.
type armedHumanWait struct {
	c        *RunCoordinator
	identity *control.IdentityContext
	runID    string
	kind     string
	ref      string
	prior    *control.LoopCheckpointRecord
	live     bool
}

// armHumanWait binds the Run's checkpoint to this wait. It returns nil when that
// cannot be done (no replayable top-level call, an entered tool, a store
// failure): the wait then takes today's grace and parks, or stays cancellable,
// exactly as it did before the hold existed.
func (c *RunCoordinator) armHumanWait(ctx context.Context, identity *control.IdentityContext, runID, kind, ref, toolName string) *armedHumanWait {
	prior, err := c.srv.Control.IncompleteLoopCheckpointForRun(context.WithoutCancel(ctx), identity.TenantID, runID)
	if err != nil {
		log.Warn("human wait is not held in place: its loop checkpoint cannot be read", "run_id", runID, "kind", kind, "error", err)
		return nil
	}
	if err := kernel.SaveHumanWaitCheckpoint(ctx, kind, ref, toolName); err != nil {
		log.Debug("human wait is not held in place: its pending call cannot be checkpointed", "run_id", runID, "kind", kind, "error", err)
		return nil
	}
	return &armedHumanWait{c: c, identity: identity, runID: runID, kind: kind, ref: ref, prior: prior, live: true}
}

// parked records that the wait's checkpoint is now the one a parked Run resumes.
func (a *armedHumanWait) parked() {
	if a != nil {
		a.live = false
	}
}

// consumed records that the answer was taken in place and the checkpoint put back.
func (a *armedHumanWait) consumed() {
	if a != nil {
		a.live = false
	}
}

// release is what a claim hands the store so that claiming the decision and
// putting the checkpoint back are one transaction. Nil when nothing is armed.
func (a *armedHumanWait) release() *control.WaitRelease {
	if a == nil || !a.live {
		return nil
	}
	return &control.WaitRelease{Prior: a.prior}
}

// restore puts the prior checkpoint back outside a claim: for a clarification,
// whose answer needs no claim, and for a wait that ends without parking.
func (a *armedHumanWait) restore(ctx context.Context) error {
	if a == nil || !a.live {
		return nil
	}
	err := a.c.srv.Control.ReleaseHumanWait(context.WithoutCancel(ctx), a.identity.TenantID, a.identity.PersonID, a.runID, a.kind, a.ref, control.WaitRelease{Prior: a.prior})
	if err == nil {
		a.live = false
	}
	return err
}

// abandon is the deferred end of a wait that returned without parking and
// without consuming an answer (a stop, an error): nothing pending remains, so the
// wait's checkpoint must not either. A failure is logged; it cannot be retried
// from here.
func (a *armedHumanWait) abandon(ctx context.Context) {
	if err := a.restore(ctx); err != nil {
		log.Warn("human wait could not put its loop checkpoint back", "run_id", a.runID, "kind", a.kind, "error", err)
	}
}
