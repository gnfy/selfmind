package eval

import (
	"context"
	"fmt"
	"os"
	"time"

	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

// heldTurnGrace bounds how long the end of a case waits for its held Runs to
// finish before it releases the holds that no stop ended. Tests shorten it.
var heldTurnGrace = 10 * time.Second

// heldTurn is a hold_worker turn whose request is still in flight.
type heldTurn struct {
	index   int
	started time.Time
	done    chan struct{}
	resp    api.MessageResponse
	status  int
}

// turnRequest builds the gateway request for one turn: its endpoint, account,
// channel and working directory, and any structured edges.
func (h *runtimeHarness) turnRequest(c *Case, turn Turn, input, channel, replyToRunID, workspace, workspaceID string) (api.MessageRequest, error) {
	req := api.MessageRequest{
		TenantID:              h.tenantID,
		Platform:              firstNonEmpty(turn.Platform, "eval"),
		PlatformUserID:        firstNonEmpty(turn.PlatformUserID, c.caseUser()),
		DisplayName:           "SelfMind Eval",
		Channel:               channel,
		Content:               input,
		ReplyToRunID:          replyToRunID,
		NativeReplyMessageID:  turn.NativeReplyMessageID,
		ApprovalID:            turn.ApprovalID,
		ClarifyID:             turn.ClarifyID,
		ClientCWD:             workspace,
		ClientAdditionalRoots: append([]string{}, turn.AdditionalRoots...),
		WorkspaceID:           workspaceID,
		// Cases default to full-auto; smart cases exercise the real judge.
		// Workspace scope and the hard deny floor remain active in both.
		ApprovalMode: firstNonEmpty(c.ApprovalMode, string(tools.ApprovalFullAuto)),
	}
	if turn.CWD != "" {
		dir, err := workspaceSubdir(workspace, turn.CWD)
		if err != nil {
			return req, err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return req, err
		}
		// A terminal sends only its directory; the gateway binds the workspace.
		req.ClientCWD, req.WorkspaceID = dir, ""
	}
	return req, nil
}

// startHeldTurn submits a hold_worker turn and returns once its Run holds a
// worker, the turn has ended without one, or the budget has run out.
func (h *runtimeHarness) startHeldTurn(ctx context.Context, budget time.Duration, index int, req api.MessageRequest) *heldTurn {
	leased := h.leases.expectHold(index + 1)
	turnCtx, cancel := context.WithTimeout(ctx, budget)
	held := &heldTurn{index: index, started: time.Now(), done: make(chan struct{})}
	go func() {
		defer close(held.done)
		defer cancel()
		held.resp, held.status = h.server.ProcessMessage(turnCtx, req)
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-leased:
	case <-held.done:
	case <-timer.C:
	}
	return held
}

// joinHeldTurns waits for the held turns. Runs the case stopped finish
// promptly; any other hold is released after the grace period, so the case
// fails instead of keeping a worker.
func (h *runtimeHarness) joinHeldTurns(held []*heldTurn) {
	if len(held) == 0 {
		return
	}
	all := make(chan struct{})
	go func() {
		for _, turn := range held {
			<-turn.done
		}
		close(all)
	}()
	select {
	case <-all:
	case <-time.After(heldTurnGrace):
		h.leases.releaseHolds()
		<-all
	}
}

// heldTurnChecks fail a held turn whose Run never held a worker, or that did
// not end because the case stopped it: the hold must have ended on a stop and
// the Run must be stored as cancelled.
func (h *runtimeHarness) heldTurnChecks(ctx context.Context, turn *heldTurn) []CheckResult {
	number := turn.index + 1
	lease, leased := h.leases.turnLease(number)
	leasedCheck := CheckResult{Name: fmt.Sprintf("turn-%d:hold_leased_worker", number), OK: leased, Score: 1}
	if !leased {
		leasedCheck.Score = 0
		leasedCheck.Message = fmt.Sprintf("the Run never leased a worker; turn status %q: %s",
			turnStatus(turn.resp), preview(firstNonEmpty(turn.resp.Content, turn.resp.Error), 160))
		return []CheckResult{leasedCheck}
	}
	stoppedCheck := CheckResult{Name: fmt.Sprintf("turn-%d:hold_stopped", number), OK: true, Score: 1}
	if problem := h.heldRunNotStopped(ctx, lease); problem != "" {
		stoppedCheck.OK, stoppedCheck.Score, stoppedCheck.Message = false, 0, "the held Run was not stopped: "+problem
	}
	return []CheckResult{leasedCheck, stoppedCheck}
}

// heldRunNotStopped explains why a held lease does not count as stopped by the
// case, or returns "" when it does.
func (h *runtimeHarness) heldRunNotStopped(ctx context.Context, lease workerLease) string {
	switch lease.endedBy {
	case holdStopped:
	case "":
		return "it still held its worker"
	case holdDeadline:
		return "the turn deadline expired"
	case holdStalled:
		return "the run watchdog cancelled it as stalled"
	case holdReleased:
		return "it was released when the case ended"
	default:
		return "it ended by " + lease.endedBy
	}
	run, err := h.controlStore.GetRun(ctx, h.tenantID, lease.runID)
	switch {
	case err != nil:
		return "its stored status could not be read: " + err.Error()
	case run == nil:
		return "no Run " + lease.runID + " is stored"
	case run.Status != "cancelled":
		return fmt.Sprintf("its stored status is %q, not cancelled", run.Status)
	}
	return ""
}

func turnStatus(resp api.MessageResponse) string {
	if resp.Turn != nil {
		return resp.Turn.Status
	}
	return ""
}
