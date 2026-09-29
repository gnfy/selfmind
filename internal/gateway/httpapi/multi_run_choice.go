package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
)

// ambiguousActiveInput parks ordinary input when several live Runs could own
// it. The full Main coordination path remains a separate capacity gate; until
// it exists, a durable, person-scoped choice is safer than silently selecting
// one Run or starting an effectful third Run under the wrong objective.
func (d *Server) ambiguousActiveInput(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (api.MessageResponse, bool) {
	if d == nil || identity == nil || req.Platform == "cli" || req.Platform == "eval" ||
		!isUserOriginTurn(ctx, req) || req.ForceNew || req.ReplyToRunID != "" ||
		req.ApprovalID != "" || req.ClarifyID != "" || req.TaskID != "" || req.ContinuityAction != "" {
		return api.MessageResponse{}, false
	}
	active := d.coordinator().activeRunsForPerson(identity.PersonID)
	if len(active) < 2 {
		return api.MessageResponse{}, false
	}
	workspace, err := d.coordinator().prepareRequestWorkspace(ctx, identity, &req)
	if err == nil {
		err = d.coordinator().prepareRequestExecutionRoots(ctx, workspace, &req)
	}
	if err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error())}, true
	}
	req.Attachments = d.coordinator().importAttachments(ctx, identity, nil, req.Attachments)
	options := make([]control.TurnChoiceOption, 0, len(active)+1)
	var message strings.Builder
	message.WriteString("Several runs are active. Which work is this for?\n")
	for _, run := range active {
		if run.RunID == "" || run.TaskID == "" {
			continue
		}
		title := run.Summary
		if task, taskErr := d.Control.GetTask(ctx, identity.TenantID, run.TaskID); taskErr == nil && task != nil && task.PersonID == identity.PersonID && strings.TrimSpace(task.Title) != "" {
			title = task.Title
		}
		key := fmt.Sprintf("%d", len(options)+1)
		label := shortRunID(run.RunID) + " · " + truncate(toOneLine(title), 60)
		options = append(options, control.TurnChoiceOption{Key: key, Label: label, Action: "steer", TaskID: run.TaskID, RunID: run.RunID})
		fmt.Fprintf(&message, "%s. %s\n", key, label)
	}
	newKey := fmt.Sprintf("%d", len(options)+1)
	options = append(options, control.TurnChoiceOption{Key: newKey, Label: "This is new work", Action: "new"})
	fmt.Fprintf(&message, "%s. This is new work\n", newKey)
	choice, err := d.createTurnChoice(ctx, identity, req, options, "multi_active")
	if err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error())}, true
	}
	fmt.Fprintf(&message, "Reply with a number, or use /choose %s <number> from another endpoint.", choice.ID)
	content := message.String()
	return api.MessageResponse{Identity: identity, Content: content, Choice: choice,
		Turn: messageTurn("waiting_user", "", "idle", "", "", content)}, true
}

func (d *Server) routeMultiRunChoice(ctx context.Context, identity *control.IdentityContext, answer api.MessageRequest, choice *control.PendingTurnChoice, optionKey string) api.MessageResponse {
	original, err := restorePendingTurnRequest(answer, choice.RequestJSON)
	if err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error())}
	}
	var selected *control.TurnChoiceOption
	for i := range choice.Options {
		if choice.Options[i].Key == optionKey {
			selected = &choice.Options[i]
			break
		}
	}
	if selected == nil || (selected.Action != "new" && selected.Action != "steer") {
		content := "That option is not available. Choose one of the numbers shown with the question."
		return api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("waiting_user", "", "idle", "", "", content)}
	}
	// Managed attachment refs and the original execution roots were frozen
	// before the question was shown. The answer endpoint supplies delivery
	// identity, but cannot widen the original request's filesystem scope.
	refs := attachmentRefsFromAPI(original.Attachments)
	queued := control.QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Channel: answer.Channel, Platform: answer.Platform, PlatformUserID: answer.PlatformUserID,
		Content: original.Content, ApprovalMode: original.ApprovalMode,
		WorkspaceID: original.WorkspaceID, ExecutionRoots: original.ExecutionRoots, Attachments: refs,
	}
	steering := control.SteeringMessage{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: selected.RunID, TaskID: selected.TaskID,
		Channel: answer.Channel, Platform: answer.Platform, PlatformUserID: answer.PlatformUserID,
		Content: original.Content, ApprovalMode: original.ApprovalMode,
		WorkspaceID: original.WorkspaceID, ExecutionRoots: original.ExecutionRoots, Attachments: refs,
	}
	routed, err := d.Control.RoutePendingTurnChoice(ctx, control.TurnChoiceWorkRoute{
		TenantID: identity.TenantID, PersonID: identity.PersonID, ChoiceID: choice.ID,
		OptionKey: optionKey, RequestJSON: choice.RequestJSON, Queued: queued, Steering: steering,
	})
	if errors.Is(err, control.ErrTurnChoiceNotFound) {
		content := "That choice expired or was already used. Send the request again so I can re-check current work."
		return api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("waiting_user", "", "idle", "", "", content)}
	}
	if err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error())}
	}
	if routed.Steering != nil {
		active := d.coordinator().activeForRun(identity.PersonID, routed.Steering.RunID)
		if active != nil && active.Steer != nil {
			select {
			case active.Steer <- kernel.SteeringInput{
				ID: routed.Steering.ID, Content: steeringContentWithAttachments(original.Content, original.Attachments),
				ContentHash: routed.Steering.ContentHash,
			}:
				if err := d.Control.MarkSteeringClaimed(ctx, identity.TenantID, routed.Steering.ID); err != nil {
					// The durable accepted row remains recoverable even if the
					// advisory handoff mark could not be written.
				}
				appendRunSteeredEvent(ctx, d.Control, active, routed.Steering)
				return d.multiRunChoiceReceipt(identity, routed, true)
			default:
				// The accepted mailbox row remains durable. Finalization or
				// restart moves unconsumed guidance to exact follow-up work.
			}
		}
	}
	return d.multiRunChoiceReceipt(identity, routed, false)
}

func (d *Server) multiRunChoiceReceipt(identity *control.IdentityContext, routed *control.TurnChoiceWorkResult, sentToLiveRun bool) api.MessageResponse {
	if routed == nil || routed.Option == nil {
		return api.MessageResponse{Identity: identity, Error: "choice has no durable destination", Turn: messageTurn("failed", "", "idle", "", "", "choice has no durable destination")}
	}
	if routed.Queued != nil {
		content := "Saved your request as queued work (" + routed.Queued.ID + ")."
		turn := messageTurn("queued", "queued", "idle", routed.Queued.TaskID, routed.Option.RunID, content)
		turn.QueueID = routed.Queued.ID
		d.coordinator().drainQueue(identity)
		return api.MessageResponse{Identity: identity, Content: content, Accepted: true, Turn: turn}
	}
	if routed.Steering != nil {
		content := "Saved your update for run " + shortRunID(routed.Steering.RunID) + ". It has not reached the current execution yet."
		if sentToLiveRun || routed.Steering.Status == control.SteeringClaimed || routed.Steering.Status == control.SteeringConsumed {
			content = "Your update is recorded for run " + shortRunID(routed.Steering.RunID) + "."
		}
		return api.MessageResponse{Identity: identity, Content: content, Accepted: true,
			Turn: messageTurn("accepted", "running", "running", routed.Steering.TaskID, routed.Steering.RunID, content)}
	}
	return api.MessageResponse{Identity: identity, Error: "choice has no durable destination", Turn: messageTurn("failed", "", "idle", "", "", "choice has no durable destination")}
}
