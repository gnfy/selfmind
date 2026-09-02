package httpapi

import (
	"context"
	"encoding/json"
	"errors"
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
	Direct   bool
	Task     *control.Task
	Rejected bool
	Notice   string
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
		claimed, claimErr := c.srv.Control.ClaimInteractionContinuation(ctx, identity.TenantID, identity.PersonID, run.ID, target.ID)
		if claimErr == nil {
			claimedTask, err := c.srv.Control.GetTask(ctx, identity.TenantID, claimed.TaskID)
			if err != nil || claimedTask == nil {
				if err == nil {
					err = fmt.Errorf("claimed continuation task is unavailable")
				}
				return nil, err
			}
			*run = *claimed
			commit.Direct = true
			commit.Task = claimedTask
			commit.Notice = "The historical work was validated and continued directly in the current execution domain."
			c.appendWorkSelectionEvent(ctx, claimedTask, run, req.Channel, "work.selection_committed", map[string]interface{}{
				"action": proposal.Action, "run_id": target.ID, "commit_mode": "direct",
			})
			return commit, nil
		}
		if !errors.Is(claimErr, control.ErrContinuationDomainMismatch) && !errors.Is(claimErr, control.ErrParentCheckpointRequired) {
			return nil, claimErr
		}
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
