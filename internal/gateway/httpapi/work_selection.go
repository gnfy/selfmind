package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

type workSelectionProposal struct {
	Action string `json:"action"`
	RunID  string `json:"run_id"`
	TaskID string `json:"task_id,omitempty"`
}

type workSelectionCommit struct {
	Action   string
	RunID    string
	QueueID  string
	Rejected bool
	// Direct reports that work_select already claimed the parent inside the
	// turn: the interaction run now lives on the parent's thread and did the
	// work itself, so finalization must neither queue a child nor demote it.
	Direct bool
	Notice string
}

func (c *RunCoordinator) latestWorkSelection(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run) (*workSelectionProposal, error) {
	if c == nil || c.srv == nil || c.srv.Control == nil || identity == nil || task == nil || run == nil {
		return nil, nil
	}
	events, err := c.srv.Control.ListRunEvents(ctx, identity.TenantID, identity.PersonID, task.ID, run.ID, 50)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.Type != "work.selection" {
			continue
		}
		var proposal workSelectionProposal
		if json.Unmarshal(event.Payload, &proposal) != nil {
			return nil, fmt.Errorf("invalid work selection payload")
		}
		proposal.Action = strings.ToLower(strings.TrimSpace(proposal.Action))
		proposal.RunID = strings.TrimSpace(proposal.RunID)
		if (proposal.Action != "observe" && proposal.Action != "resume") || proposal.RunID == "" {
			return nil, fmt.Errorf("invalid work selection proposal")
		}
		return &proposal, nil
	}
	return nil, nil
}

// commitWorkSelection is the authority boundary after Main has interpreted the
// request. The typed proposal is re-read from durable state, the target and
// effect window are revalidated, and only then may the gateway claim the
// current same-domain interaction or create an exact queue edge. The active
// Run's frozen execution scope is never changed in place.
func (c *RunCoordinator) commitWorkSelection(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, task *control.Task, run *control.Run) (*workSelectionCommit, error) {
	proposal, err := c.latestWorkSelection(ctx, identity, task, run)
	if err != nil || proposal == nil {
		return nil, err
	}
	target, err := c.srv.Control.GetRun(ctx, identity.TenantID, proposal.RunID)
	if err != nil {
		return nil, err
	}
	if target == nil || target.PersonID != identity.PersonID || (proposal.TaskID != "" && proposal.TaskID != target.TaskID) {
		return nil, fmt.Errorf("selected work is unavailable for the current person")
	}
	commit := &workSelectionCommit{Action: proposal.Action, RunID: target.ID}
	if proposal.Action == "resume" {
		// A same-domain resume is claimed by work_select while the turn runs
		// (ClaimInteractionContinuation); the run already carries the parent
		// edge and the commit event. Nothing is left to queue.
		if strings.TrimSpace(run.ResumesRunID) == target.ID {
			commit.Direct = true
			return commit, nil
		}
		blocked, reason, err := c.srv.Control.RunSelectionEffectBoundary(ctx, identity.TenantID, identity.PersonID, run.ID)
		if err != nil {
			return nil, err
		}
		if blocked {
			commit.Rejected = true
			commit.Notice = "I found the historical work, but this interaction already produced effects (" + reason + "). I stopped before attaching or expanding that work. Please confirm whether to keep the observed effects and continue the historical run separately."
			c.appendWorkSelectionEvent(ctx, task, run, req.Channel, "work.selection_rejected", map[string]interface{}{
				"action": proposal.Action, "run_id": target.ID, "reason": reason,
			})
			return commit, nil
		}
		// Main interpreted this interaction before it had the selected Run's
		// checkpoint and execution scope. Even when both Runs share a workspace,
		// mutating the already-finished interaction Run would leave nobody to do
		// the selected work. Always materialize a fresh exact-parent child: its
		// scope is frozen correctly at creation and its plan/checkpoint can be
		// restored before Main starts.
		queued, err := c.srv.Control.EnqueueSelectedContinuation(ctx, identity.TenantID, identity.PersonID, run.ID, target.ID, control.QueuedTask{
			Channel: req.Channel, Platform: req.Platform, PlatformUserID: req.PlatformUserID,
			Content: req.Content, ApprovalMode: req.ApprovalMode,
		})
		if err != nil {
			return nil, err
		}
		commit.QueueID = queued.ID
		commit.Notice = "The historical work was validated and queued as the next exact continuation."
	}
	c.appendWorkSelectionEvent(ctx, task, run, req.Channel, "work.selection_committed", map[string]interface{}{
		"action": proposal.Action, "run_id": target.ID, "queue_id": commit.QueueID, "commit_mode": "transfer",
	})
	return commit, nil
}

func (c *RunCoordinator) appendWorkSelectionEvent(ctx context.Context, task *control.Task, run *control.Run, channel, eventType string, payload map[string]interface{}) {
	if c == nil || c.srv == nil || c.srv.Control == nil || task == nil || run == nil {
		return
	}
	_, _ = c.srv.Control.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: run.ID, Type: eventType, Visibility: "task", Channel: channel,
		Payload: mustJSON(payload), IdempotencyKey: eventType + ":" + run.ID,
	})
}
