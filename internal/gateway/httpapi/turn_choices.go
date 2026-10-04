package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
)

const (
	turnChoiceTTL        = 24 * time.Hour
	turnChoiceBareWindow = 30 * time.Minute
)

// pendingTurnRequest is the smallest restart-safe snapshot needed to replay an
// ambiguous pre-run request. Identity and endpoint fields deliberately stay
// out: the endpoint answering the choice is authenticated again and becomes
// the source of the resumed turn.
type pendingTurnRequest struct {
	Kind                  string                     `json:"kind,omitempty"`
	Content               string                     `json:"content"`
	WorkspaceID           string                     `json:"workspace_id,omitempty"`
	ExecutionRoots        []executionenv.RootBinding `json:"execution_roots,omitempty"`
	Platform              string                     `json:"platform,omitempty"`
	PlatformUserID        string                     `json:"platform_user_id,omitempty"`
	Channel               string                     `json:"channel,omitempty"`
	ClientCWD             string                     `json:"client_cwd,omitempty"`
	ClientAdditionalRoots []string                   `json:"client_additional_roots,omitempty"`
	Attachments           []api.MessageAttachment    `json:"attachments,omitempty"`
	AllowWeb              bool                       `json:"allow_web,omitempty"`
	ApprovalMode          string                     `json:"approval_mode,omitempty"`
	Async                 bool                       `json:"async,omitempty"`
}

func snapshotPendingTurnRequest(req api.MessageRequest, kind string) (string, error) {
	snapshot := pendingTurnRequest{
		Kind:                  kind,
		Content:               strings.TrimSpace(req.Content),
		WorkspaceID:           strings.TrimSpace(req.WorkspaceID),
		ExecutionRoots:        executionenv.CloneRootBindings(req.ExecutionRoots),
		Platform:              req.Platform,
		PlatformUserID:        req.PlatformUserID,
		Channel:               req.Channel,
		ClientCWD:             strings.TrimSpace(req.ClientCWD),
		ClientAdditionalRoots: append([]string(nil), req.ClientAdditionalRoots...),
		Attachments:           append([]api.MessageAttachment(nil), req.Attachments...),
		AllowWeb:              req.AllowWeb,
		ApprovalMode:          strings.TrimSpace(req.ApprovalMode),
		Async:                 req.Async,
	}
	data, err := json.Marshal(snapshot)
	return string(data), err
}

func restorePendingTurnRequest(current api.MessageRequest, raw string) (api.MessageRequest, error) {
	var snapshot pendingTurnRequest
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return current, err
	}
	if strings.TrimSpace(snapshot.Content) == "" {
		return current, fmt.Errorf("saved request is empty")
	}
	current.Content = snapshot.Content
	current.WorkspaceID = snapshot.WorkspaceID
	current.ExecutionRoots = executionenv.CloneRootBindings(snapshot.ExecutionRoots)
	current.ClientCWD = snapshot.ClientCWD
	current.ClientAdditionalRoots = append([]string(nil), snapshot.ClientAdditionalRoots...)
	current.Attachments = append([]api.MessageAttachment(nil), snapshot.Attachments...)
	current.AllowWeb = snapshot.AllowWeb
	current.ApprovalMode = snapshot.ApprovalMode
	current.Async = snapshot.Async
	return current, nil
}

func (d *Server) createTurnChoice(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, options []control.TurnChoiceOption, kind ...string) (*api.TurnChoice, error) {
	if d == nil || d.Control == nil || identity == nil {
		return nil, fmt.Errorf("turn choice storage is unavailable")
	}
	choiceKind := ""
	if len(kind) > 0 {
		choiceKind = kind[0]
	}
	requestJSON, err := snapshotPendingTurnRequest(req, choiceKind)
	if err != nil {
		return nil, err
	}
	choice, err := d.Control.CreatePendingTurnChoice(ctx, control.PendingTurnChoiceCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, AccountID: identity.AccountID,
		Channel: req.Channel, ResolutionID: req.ContinuityResolutionID, RequestJSON: requestJSON, Options: options,
		ExpiresAt: time.Now().Add(turnChoiceTTL),
	})
	if err != nil {
		return nil, err
	}
	wire := &api.TurnChoice{ID: choice.ID, Options: make([]api.TurnChoiceOption, 0, len(choice.Options))}
	for _, option := range choice.Options {
		wire.Options = append(wire.Options, api.TurnChoiceOption{Key: option.Key, Label: option.Label})
	}
	return wire, nil
}

// rewriteExplicitContinuityControl handles /new <request>, /choose, and a
// platform-supplied choice_id before the general control-command path. The
// returned request has the original message restored and a typed target; no
// rendered prose is parsed.
func (d *Server) rewriteExplicitContinuityControl(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (api.MessageRequest, bool, *api.MessageResponse) {
	trimmed := strings.TrimSpace(req.Content)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "/new --run ") {
		req.Content = strings.TrimSpace(trimmed[len("/new --run"):])
		if req.Content == "" {
			resp := api.MessageResponse{Identity: identity, Content: "Usage: /new --run <request>", Turn: messageTurn("completed", "", "idle", "", "", "")}
			return req, true, &resp
		}
		req.ForceNew = true
		return req, false, nil
	}

	choiceID := strings.TrimSpace(req.ChoiceID)
	optionKey := ""
	if strings.HasPrefix(lower, "/choose") {
		parts := strings.Fields(trimmed)
		if len(parts) != 3 {
			resp := api.MessageResponse{Identity: identity, Content: "Usage: /choose <choice_id> <number>", Turn: messageTurn("completed", "", "idle", "", "", "")}
			return req, true, &resp
		}
		choiceID, optionKey = parts[1], parts[2]
	} else if choiceID != "" {
		optionKey = strings.TrimSpace(trimmed)
	}
	if choiceID == "" {
		return req, false, nil
	}
	return d.claimTurnChoice(ctx, identity, req, choiceID, optionKey)
}

func (d *Server) rewriteBareTurnChoice(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (api.MessageRequest, bool, *api.MessageResponse) {
	key := strings.TrimSpace(req.Content)
	if _, err := strconv.Atoi(key); err != nil || strings.ContainsAny(key, "+-") {
		return req, false, nil
	}
	return d.claimTurnChoice(ctx, identity, req, "", key)
}

func (d *Server) claimTurnChoice(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, choiceID, optionKey string) (api.MessageRequest, bool, *api.MessageResponse) {
	peek, peekErr := d.Control.PeekPendingTurnChoice(ctx, identity.TenantID, identity.PersonID,
		choiceID, time.Now(), turnChoiceBareWindow)
	if errors.Is(peekErr, control.ErrTurnChoiceNotFound) && strings.TrimSpace(choiceID) != "" {
		if routed, err := d.Control.RoutedTurnChoice(ctx, identity.TenantID, identity.PersonID, choiceID, optionKey); err == nil {
			response := d.multiRunChoiceReceipt(identity, routed, false)
			return req, true, &response
		} else if !errors.Is(err, control.ErrTurnChoiceNotFound) {
			response := api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error())}
			return req, true, &response
		}
	}
	if peekErr == nil {
		var snapshot pendingTurnRequest
		if err := json.Unmarshal([]byte(peek.RequestJSON), &snapshot); err == nil && snapshot.Kind == "multi_active" {
			response := d.routeMultiRunChoice(ctx, identity, req, peek, optionKey)
			return req, true, &response
		}
	} else if !errors.Is(peekErr, control.ErrTurnChoiceNotFound) && !errors.Is(peekErr, control.ErrTurnChoiceAmbiguous) {
		response := api.MessageResponse{Identity: identity, Error: peekErr.Error(), Turn: messageTurn("failed", "", "idle", "", "", peekErr.Error())}
		return req, true, &response
	}
	choice, option, err := d.Control.ClaimPendingTurnChoice(ctx, identity.TenantID, identity.PersonID,
		choiceID, optionKey, time.Now(), turnChoiceBareWindow)
	if errors.Is(err, control.ErrTurnChoiceNotFound) {
		if strings.TrimSpace(choiceID) == "" {
			return req, false, nil
		}
		resp := api.MessageResponse{Identity: identity, Content: "That choice expired or was already used. Send the request again so I can re-check current work.", Turn: messageTurn("waiting_user", "", "idle", "", "", "")}
		return req, true, &resp
	}
	if errors.Is(err, control.ErrTurnChoiceAmbiguous) {
		resp := api.MessageResponse{Identity: identity, Content: "More than one question is waiting. Reply to the specific message or use /choose <choice_id> <number>.", Turn: messageTurn("waiting_user", "", "idle", "", "", "")}
		return req, true, &resp
	}
	if errors.Is(err, control.ErrTurnChoiceOption) {
		resp := api.MessageResponse{Identity: identity, Content: "That option is not available. Choose one of the numbers shown with the question.", Turn: messageTurn("waiting_user", "", "idle", "", "", "")}
		return req, true, &resp
	}
	if err != nil {
		resp := api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error())}
		return req, true, &resp
	}
	restored, err := restorePendingTurnRequest(req, choice.RequestJSON)
	if err != nil {
		resp := api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error())}
		return req, true, &resp
	}
	restored.ChoiceID = choice.ID
	restored.ContinuityAction = strings.TrimSpace(option.Action)
	switch option.Action {
	case "new":
		restored.ForceNew = true
		restored.TaskID = ""
		restored.ReplyToRunID = ""
	case "resume", "steer", "observe":
		restored.TaskID = strings.TrimSpace(option.TaskID)
		restored.ReplyToRunID = strings.TrimSpace(option.RunID)
	default:
		resp := api.MessageResponse{Identity: identity, Error: "saved choice has an unsupported action", Turn: messageTurn("failed", "", "idle", "", "", "saved choice has an unsupported action")}
		return req, true, &resp
	}
	if strings.TrimSpace(choice.ResolutionID) != "" {
		candidateIDs := make([]string, 0, len(choice.Options))
		for _, candidate := range choice.Options {
			if strings.TrimSpace(candidate.RunID) != "" {
				candidateIDs = append(candidateIDs, candidate.RunID)
			}
		}
		_, _ = d.Control.RecordTurnResolution(context.WithoutCancel(ctx), control.TurnResolutionRecord{
			TenantID: identity.TenantID, PersonID: identity.PersonID, AccountID: identity.AccountID,
			Channel: req.Channel, Input: restored.Content, Mode: "human_correction",
			Decision: option.Action, Certainty: "clear", TargetTaskID: option.TaskID, TargetRunID: option.RunID,
			CandidateIDs: candidateIDs, Evidence: []string{"user_choice"}, CorrectionOf: choice.ResolutionID,
		})
	}
	return restored, false, nil
}

func (d *Server) claimedTurnChoiceResponse(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) *api.MessageResponse {
	switch strings.TrimSpace(req.ContinuityAction) {
	case "observe":
		candidate, ok := d.exactContinuityCandidate(ctx, identity, req.ReplyToRunID)
		if !ok {
			content := "That work item changed after the question was shown. Send the status request again so I can re-check it."
			return &api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("waiting_user", "", "idle", "", "", content)}
		}
		response := d.continuityProgressResponse(ctx, identity, candidate)
		return &response
	case "steer":
		active := d.coordinator().activeForRun(identity.PersonID, strings.TrimSpace(req.ReplyToRunID))
		if active == nil {
			response := d.queueGuidanceAfterTargetEnded(ctx, identity, req)
			return &response
		}
	}
	return nil
}

// The target may finish between showing a durable choice and claiming it.
// Preserve the original request as a follow-up instead of dropping it or
// delivering it to a different live Run. A still-running database row without
// a live handle is uncertain; only recovery can decide its next execution.
func (d *Server) queueGuidanceAfterTargetEnded(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) api.MessageResponse {
	target, err := d.Control.GetRun(ctx, identity.TenantID, strings.TrimSpace(req.ReplyToRunID))
	if err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error())}
	}
	if target == nil || target.PersonID != identity.PersonID {
		content := "That run is unavailable for this account. The original request remains in the choice record; send it again after checking /status."
		return api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("waiting_user", "", "idle", "", "", content)}
	}
	if target.Status == "running" {
		content := "That run is transitioning and its effects are not yet known. Check /status before retrying this request."
		return api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("waiting_user", "", "idle", target.TaskID, target.ID, content)}
	}
	queued := control.QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: target.TaskID,
		Channel: req.Channel, Platform: req.Platform, PlatformUserID: req.PlatformUserID,
		Content: req.Content, ApprovalMode: req.ApprovalMode, WorkspaceID: target.WorkspaceID,
		ExecutionRoots: executionenv.CloneRootBindings(target.ExecutionRoots),
		Attachments:    d.admitQueuedAttachments(ctx, identity, req),
	}
	if continuityRunResumable(target.Status) {
		queued.ReplyToRunID = target.ID
	}
	saved, err := d.Control.EnqueueQueued(ctx, queued)
	if err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", target.TaskID, target.ID, err.Error())}
	}
	content := fmt.Sprintf("Run %s finished before your reply reached it. I saved your request as follow-up work (%s).", shortRunID(target.ID), saved.ID)
	turn := messageTurn("queued", "queued", "idle", target.TaskID, target.ID, content)
	turn.QueueID = saved.ID
	d.coordinator().drainQueue(identity)
	return api.MessageResponse{Identity: identity, Content: content, Accepted: true, Turn: turn}
}
