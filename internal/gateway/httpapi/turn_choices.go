package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/platform/log"
)

const (
	turnChoiceTTL        = 24 * time.Hour
	turnChoiceBareWindow = 30 * time.Minute
	// multiRunChoiceKind marks a choice asked because several Runs were live.
	multiRunChoiceKind = "multi_active"
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

// MultiRunChoiceRequest returns the request snapshot that a multi-Run choice
// stores for req, exactly as the gateway writes it when it asks which Run a
// message belongs to. The eval harness seeds pending choices with it.
func MultiRunChoiceRequest(req api.MessageRequest) (string, error) {
	return snapshotPendingTurnRequest(req, multiRunChoiceKind)
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
	return d.createTurnChoiceWithNotice(ctx, identity, req, options, nil, kind...)
}

func (d *Server) createTurnChoiceWithNotice(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, options []control.TurnChoiceOption, notice *control.PendingTurnChoiceNotice, kind ...string) (*api.TurnChoice, error) {
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
	choiceID := ""
	if notice != nil {
		choiceID = "choice_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		notice.QueueID, notice.QueueClaimToken = req.QueueID, req.QueueClaimToken
		if d.Delivery != nil {
			msg := delivery.Message{TenantID: identity.TenantID, PersonID: identity.PersonID,
				Platform: req.Platform, PlatformUserID: identity.PlatformUserID, Channel: req.Channel,
				Kind: delivery.KindTurnChoice, ChoiceID: choiceID, LogicalKey: "turn-choice:" + choiceID,
				Content: strings.TrimSpace(notice.Question) + fmt.Sprintf("\nReply with a number, or use /choose %s <number> from another endpoint.", choiceID)}
			if req.Platform == "cli" {
				if account := d.coordinator().preferredIMAccount(ctx, identity); account != nil {
					msg.Platform, msg.PlatformUserID, msg.Channel = account.Platform, account.PlatformUserID, account.PlatformUserID
					// Bare replies remain endpoint-local. A cross-endpoint recovery
					// notice names the exact choice without changing that authority.
					msg.Kind = delivery.KindRecovery
					msg.Content = strings.TrimSpace(notice.Question) + fmt.Sprintf("\nUse /choose %s <number> to select this saved request.", choiceID)
				} else {
					msg.Platform = ""
				}
			}
			if msg.Platform != "" && d.Delivery.SupportsPlatform(msg.Platform) {
				notice.Deliveries = d.Delivery.PrepareChoiceNotice(msg)
			}
		}
	}
	// A failed write may be refused as "nothing saved", so it must be exact.
	choice, err := d.Control.CreatePendingTurnChoice(exactWrite(ctx), control.PendingTurnChoiceCreate{
		ID:       choiceID,
		TenantID: identity.TenantID, PersonID: identity.PersonID, AccountID: identity.AccountID,
		Channel: req.Channel, ResolutionID: req.ContinuityResolutionID, RequestJSON: requestJSON, Options: options,
		ExpiresAt: time.Now().Add(turnChoiceTTL), Notice: notice,
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
// rendered prose is parsed. A refusal means nothing was claimed and the answer
// may be delivered again.
func (d *Server) rewriteExplicitContinuityControl(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (api.MessageRequest, bool, *api.MessageResponse, *notDispatched) {
	trimmed := strings.TrimSpace(req.Content)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "/new --run ") {
		req.Content = strings.TrimSpace(trimmed[len("/new --run"):])
		if req.Content == "" {
			resp := api.MessageResponse{Identity: identity, Content: "Usage: /new --run <request>", Turn: messageTurn("completed", "", "idle", "", "", "")}
			return req, true, &resp, nil
		}
		req.ForceNew = true
		return req, false, nil, nil
	}

	choiceID := strings.TrimSpace(req.ChoiceID)
	optionKey := ""
	if strings.HasPrefix(lower, "/choose") {
		parts := strings.Fields(trimmed)
		if len(parts) != 3 {
			resp := api.MessageResponse{Identity: identity, Content: "Usage: /choose <choice_id> <number>", Turn: messageTurn("completed", "", "idle", "", "", "")}
			return req, true, &resp, nil
		}
		choiceID, optionKey = parts[1], parts[2]
	} else if choiceID != "" {
		optionKey = strings.TrimSpace(trimmed)
	}
	if choiceID == "" {
		return req, false, nil, nil
	}
	return d.claimTurnChoice(ctx, identity, req, choiceID, optionKey)
}

// hasExactReplyEdge reports a platform-proven reply, approval, or
// clarification edge. The edge already names the work a short answer belongs
// to, so no implicit prompt may claim the message instead.
func hasExactReplyEdge(req api.MessageRequest) bool {
	return strings.TrimSpace(req.NativeReplyMessageID) != "" || strings.TrimSpace(req.ReplyToRunID) != "" ||
		strings.TrimSpace(req.ApprovalID) != "" || strings.TrimSpace(req.ClarifyID) != ""
}

// rewriteBareTurnChoice treats a bare number as an answer only to the one
// recent choice shown on this account and channel. Anywhere else the number
// stays that endpoint's own input; /choose names a choice from any endpoint.
func (d *Server) rewriteBareTurnChoice(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (api.MessageRequest, bool, *api.MessageResponse, *notDispatched) {
	if hasExactReplyEdge(req) {
		return req, false, nil, nil
	}
	key := strings.TrimSpace(req.Content)
	if _, err := strconv.Atoi(key); err != nil || strings.ContainsAny(key, "+-") {
		return req, false, nil, nil
	}
	choice, err := d.Control.PeekBareTurnChoice(ctx, identity.TenantID, identity.PersonID,
		control.TurnChoiceEndpoint{AccountID: identity.AccountID, Channel: req.Channel}, time.Now(), turnChoiceBareWindow)
	if errors.Is(err, control.ErrTurnChoiceNotFound) {
		return req, false, nil, nil
	}
	if errors.Is(err, control.ErrTurnChoiceAmbiguous) {
		resp := api.MessageResponse{Identity: identity, Content: "More than one question is waiting here. Reply to the specific message or use /choose <choice_id> <number>.", Turn: messageTurn("waiting_user", "", "idle", "", "", "")}
		return req, true, &resp, nil
	}
	if err != nil {
		resp, refusal := readRefusal(identity, err)
		return req, true, &resp, refusal
	}
	return d.claimTurnChoice(ctx, identity, req, choice.ID, key)
}

// claimTurnChoice answers one named choice. Every refusal it returns happens
// before the claim, so the choice and its saved request stay pending.
func (d *Server) claimTurnChoice(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, choiceID, optionKey string) (api.MessageRequest, bool, *api.MessageResponse, *notDispatched) {
	peek, peekErr := d.Control.PeekPendingTurnChoice(ctx, identity.TenantID, identity.PersonID, choiceID, time.Now())
	if errors.Is(peekErr, control.ErrTurnChoiceNotFound) {
		if routed, err := d.Control.RoutedTurnChoice(ctx, identity.TenantID, identity.PersonID, choiceID, optionKey); err == nil {
			response := d.multiRunChoiceReceipt(identity, routed, false)
			return req, true, &response, nil
		} else if !errors.Is(err, control.ErrTurnChoiceNotFound) {
			response, refusal := readRefusal(identity, err)
			return req, true, &response, refusal
		}
	}
	if peekErr == nil {
		var snapshot pendingTurnRequest
		if err := json.Unmarshal([]byte(peek.RequestJSON), &snapshot); err == nil && snapshot.Kind == multiRunChoiceKind {
			response, refusal := d.routeMultiRunChoice(ctx, identity, req, peek, optionKey)
			return req, true, &response, refusal
		}
	} else if !errors.Is(peekErr, control.ErrTurnChoiceNotFound) {
		response, refusal := readRefusal(identity, peekErr)
		return req, true, &response, refusal
	}
	// The restored request would only meet the shutdown refusal after the
	// claim had consumed its choice. Refuse first so the choice keeps its saved
	// request for the answer redelivered after the restart.
	if peekErr == nil && d.shuttingDown() {
		response, _, refusal := shutdownRefusal(identity)
		return req, true, &response, refusal
	}
	choice, option, err := d.Control.ClaimPendingTurnChoice(ctx, identity.TenantID, identity.PersonID, choiceID, optionKey, time.Now())
	if errors.Is(err, control.ErrTurnChoiceNotFound) {
		resp := api.MessageResponse{Identity: identity, Content: "That choice expired or was already used. Send the request again so I can re-check current work.", Turn: messageTurn("waiting_user", "", "idle", "", "", "")}
		return req, true, &resp, nil
	}
	if errors.Is(err, control.ErrTurnChoiceOption) {
		resp := api.MessageResponse{Identity: identity, Content: "That option is not available. Choose one of the numbers shown with the question.", Turn: messageTurn("waiting_user", "", "idle", "", "", "")}
		return req, true, &resp, nil
	}
	if err != nil {
		// The claim commits in one transaction; a failed one consumed nothing.
		resp := api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error())}
		return req, true, &resp, &notDispatched{reason: refusalSave, cause: err}
	}
	restored, err := restorePendingTurnRequest(req, choice.RequestJSON)
	if err != nil {
		resp := api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error())}
		return req, true, &resp, nil
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
		return req, true, &resp, nil
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
	return restored, false, nil, nil
}

// claimedTurnChoiceResponse answers a claimed choice whose action needs no
// new turn. Its refusal comes after the claim used the choice, so the caller
// holds it rather than inviting a redelivery.
func (d *Server) claimedTurnChoiceResponse(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (*api.MessageResponse, *notDispatched) {
	switch strings.TrimSpace(req.ContinuityAction) {
	case "observe":
		candidate, ok := d.exactContinuityCandidate(ctx, identity, req.ReplyToRunID)
		if !ok {
			content := "That work item changed after the question was shown. Send the status request again so I can re-check it."
			return &api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("waiting_user", "", "idle", "", "", content)}, nil
		}
		response := d.continuityProgressResponse(ctx, identity, candidate)
		return &response, nil
	case "steer":
		active := d.coordinator().activeForRun(identity.PersonID, strings.TrimSpace(req.ReplyToRunID))
		if active == nil {
			response, refusal := d.queueGuidanceAfterTargetEnded(ctx, identity, req)
			return &response, refusal
		}
	}
	return nil, nil
}

// The target may finish between showing a durable choice and claiming it.
// Preserve the original request as a follow-up instead of dropping it or
// delivering it to a different live Run. A still-running database row without
// a live handle is uncertain; only recovery can decide its next execution.
func (d *Server) queueGuidanceAfterTargetEnded(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (api.MessageResponse, *notDispatched) {
	target, err := d.Control.GetRun(ctx, identity.TenantID, strings.TrimSpace(req.ReplyToRunID))
	if err != nil {
		return followUpNotSaved(identity, refusalRecords, err)
	}
	if target == nil || target.PersonID != identity.PersonID {
		content := "That run is unavailable for this account. The original request remains in the choice record; send it again after checking /status."
		return api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("waiting_user", "", "idle", "", "", content)}, nil
	}
	if target.Status == "running" {
		content := "That run is transitioning and its effects are not yet known. Check /status before retrying this request."
		return api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("waiting_user", "", "idle", target.TaskID, target.ID, content)}, nil
	}
	// The restored request is the person's original words.
	admission, admissionErr := d.queuedAdmission(ctx, identity, req)
	if admissionErr != nil {
		return followUpNotSaved(identity, refusalRecords, admissionErr)
	}
	queued := control.QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: target.TaskID,
		Channel: req.Channel, Platform: req.Platform, PlatformUserID: req.PlatformUserID,
		Content: req.Content, ApprovalMode: req.ApprovalMode, WorkspaceID: target.WorkspaceID,
		ExecutionRoots: executionenv.CloneRootBindings(target.ExecutionRoots),
		Attachments:    d.admitQueuedAttachments(ctx, identity, req),
		Admission:      admission,
	}
	if control.RunAcceptsQueuedContinuation(target.Status) {
		queued.ReplyToRunID = target.ID
	}
	saved, err := d.Control.EnqueueQueued(exactWrite(ctx), queued)
	if err != nil {
		return followUpNotSaved(identity, refusalSave, err)
	}
	content := fmt.Sprintf("Run %s finished before your reply reached it. I saved your request as follow-up work (%s).", shortRunID(target.ID), saved.ID)
	turn := messageTurn("queued", "queued", "idle", target.TaskID, target.ID, content)
	turn.QueueID = saved.ID
	d.coordinator().drainQueue(identity)
	return api.MessageResponse{Identity: identity, Content: content, Accepted: true, Turn: turn}, nil
}

// followUpNotSaved reports a steer answer whose follow-up work could not be
// saved after its Run ended. The sender sees the short reason; the cause goes
// to the log.
func followUpNotSaved(identity *control.IdentityContext, reason string, cause error) (api.MessageResponse, *notDispatched) {
	log.Warn("claimed guidance could not be saved as follow-up work", "reason", reason, "error", cause)
	text := "Your answer was recorded, but its request was not saved because " + reason +
		". Check /status, then send the request again if it is still needed."
	return api.MessageResponse{Identity: identity, Error: text, Turn: messageTurn("failed", "", "idle", "", "", text)},
		&notDispatched{reason: reason, cause: cause}
}
