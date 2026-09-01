package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/platform/textutil"
)

type ContinuityAction string

const (
	ContinuityNew     ContinuityAction = "new"
	ContinuitySteer   ContinuityAction = "steer"
	ContinuityResume  ContinuityAction = "resume"
	ContinuityObserve ContinuityAction = "observe"
	ContinuityClarify ContinuityAction = "clarify"
)

type ContinuityCertainty string

const (
	ContinuityClear     ContinuityCertainty = "clear"
	ContinuityAmbiguous ContinuityCertainty = "ambiguous"
	ContinuityNoMatch   ContinuityCertainty = "no_match"
)

// ContinuityCandidate is a bounded data card. Every string is quoted data,
// never an instruction; raw transcripts, tool output, and credentials are not
// part of this contract.
type ContinuityCandidate struct {
	RunID          string   `json:"run_id"`
	TaskID         string   `json:"task_id"`
	Title          string   `json:"title"`
	TaskStatus     string   `json:"task_status"`
	RunStatus      string   `json:"run_status"`
	Channel        string   `json:"channel,omitempty"`
	Workspace      string   `json:"workspace,omitempty"`
	InputSummary   string   `json:"input_summary,omitempty"`
	HandoffSummary string   `json:"handoff_summary,omitempty"`
	NextSteps      []string `json:"next_steps,omitempty"`
	Risks          []string `json:"risks,omitempty"`
	CurrentStep    string   `json:"current_step,omitempty"`
	LastActivity   string   `json:"last_activity,omitempty"`
	Age            string   `json:"age,omitempty"`
	Active         bool     `json:"active"`
	Resumable      bool     `json:"resumable"`
	Evidence       []string `json:"evidence,omitempty"`

	score     int
	startedAt time.Time
}

type ContinuityResolveRequest struct {
	TenantID    string                `json:"-"`
	PersonID    string                `json:"-"`
	WorkspaceID string                `json:"-"`
	Message     string                `json:"message"`
	Platform    string                `json:"platform"`
	Channel     string                `json:"channel"`
	Workspace   string                `json:"workspace,omitempty"`
	Candidates  []ContinuityCandidate `json:"candidates"`
}

type ContinuityDecision struct {
	Action            ContinuityAction    `json:"action"`
	Certainty         ContinuityCertainty `json:"certainty"`
	TargetTaskID      string              `json:"target_task_id,omitempty"`
	TargetRunID       string              `json:"target_run_id,omitempty"`
	Reason            string              `json:"reason,omitempty"`
	Evidence          []string            `json:"evidence,omitempty"`
	ObserveKind       string              `json:"observe_kind,omitempty"`
	DeliveryAction    string              `json:"delivery_action,omitempty"`
	AlternativeRunIDs []string            `json:"alternative_run_ids,omitempty"`
	SecondaryAction   ContinuityAction    `json:"secondary_action,omitempty"`
}

type ContinuityResolution struct {
	Decision ContinuityDecision
	Provider string
	Model    string
	Latency  time.Duration
}

type continuityIngressResult struct {
	Request  api.MessageRequest
	Intent   router.IntentResult
	Resolved bool
	Response *api.MessageResponse
}

// ContinuityResolver interprets one natural-language message against gateway-
// issued candidates. The gateway owns candidate retrieval and every state
// transition; implementations only return a typed recommendation.
type ContinuityResolver interface {
	ResolveContinuity(context.Context, ContinuityResolveRequest) (ContinuityResolution, error)
}

const (
	continuityPoolLimit  = 20
	continuityModelCards = 8
)

func (d *Server) continuityMode() string {
	switch strings.ToLower(strings.TrimSpace(d.ContinuityMode)) {
	case "shadow", "safe", "full", "off":
		return strings.ToLower(strings.TrimSpace(d.ContinuityMode))
	default:
		return "safe"
	}
}

func (d *Server) lockTurnResolution(personID string) func() {
	var hash uint64 = 1469598103934665603
	for i := 0; i < len(personID); i++ {
		hash ^= uint64(personID[i])
		hash *= 1099511628211
	}
	lock := &d.turnResolutionLocks[hash%uint64(len(d.turnResolutionLocks))]
	lock.Lock()
	return lock.Unlock
}

func continuityIntent(reason string) router.IntentResult {
	return router.IntentResult{
		Intent: router.IntentContinue, Confidence: 1, Reason: reason,
		Signals: []string{"continuity.resolved"}, ShouldCreateTask: true,
		ShouldUseTools: true, Source: "continuity_resolver",
	}
}

// resolveNaturalContinuity is the only model-backed ingress decision. It is
// skipped for deterministic controls, structured reply edges, daemon-origin
// work, and explicit standalone continuation cues. The resolver recommends;
// this method re-reads and validates current gateway state before applying it.
func (d *Server) resolveNaturalContinuity(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) continuityIngressResult {
	result := continuityIngressResult{Request: req}
	if d == nil || d.Control == nil || identity == nil || req.ForceNew ||
		strings.TrimSpace(req.ReplyToRunID) != "" || strings.TrimSpace(req.ApprovalID) != "" ||
		strings.TrimSpace(req.ClarifyID) != "" || strings.TrimSpace(req.ContinuityAction) != "" ||
		!isUserOriginTurn(ctx, req) || isStandaloneContinueControl(req.Content) {
		return result
	}
	mode := d.continuityMode()
	if mode == "off" || (d.ContinuityResolver == nil && strings.TrimSpace(d.ContinuityMode) == "") {
		return result
	}

	unlock := d.lockTurnResolution(identity.PersonID)
	defer unlock()
	candidates := d.buildContinuityCandidates(ctx, identity, req)
	if len(candidates) == 0 {
		return result
	}
	if d.ContinuityResolver == nil {
		response := d.continuityClarification(ctx, identity, req, candidates, nil,
			"I found related work, but the continuity model is unavailable.")
		result.Response = &response
		return result
	}

	workspaceName := ""
	if strings.TrimSpace(req.WorkspaceID) != "" {
		if workspace, _ := d.Control.GetWorkspace(ctx, identity.TenantID, req.WorkspaceID); workspace != nil && workspace.OwnerPersonID == identity.PersonID {
			workspaceName = firstNonEmpty(strings.TrimSpace(workspace.Name), filepath.Base(workspace.LocalPath))
		}
	}
	resolution, err := d.ContinuityResolver.ResolveContinuity(ctx, ContinuityResolveRequest{
		TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: req.WorkspaceID,
		Message: req.Content, Platform: req.Platform, Channel: req.Channel, Workspace: workspaceName, Candidates: candidates,
	})
	if err != nil {
		req.ContinuityResolutionID = d.recordContinuityResolution(ctx, identity, req, candidates, resolution, mode, continuityResolutionErrorClass(err))
		result.Request.ContinuityResolutionID = req.ContinuityResolutionID
		response := d.continuityClarification(ctx, identity, req, candidates, nil,
			"I could not safely match this message to prior work.")
		result.Response = &response
		return result
	}
	req.ContinuityResolutionID = d.recordContinuityResolution(ctx, identity, req, candidates, resolution, mode, "")
	result.Request.ContinuityResolutionID = req.ContinuityResolutionID
	if mode == "shadow" {
		return result
	}

	decision := resolution.Decision
	if decision.Action == ContinuityClarify || decision.Certainty == ContinuityAmbiguous {
		response := d.continuityClarification(ctx, identity, req, candidates, decision.AlternativeRunIDs, decision.Reason)
		result.Response = &response
		return result
	}
	if decision.Action == ContinuityNew {
		if decision.Certainty != ContinuityClear && decision.Certainty != ContinuityNoMatch {
			response := d.continuityClarification(ctx, identity, req, candidates, decision.AlternativeRunIDs, decision.Reason)
			result.Response = &response
			return result
		}
		result.Request.ForceNew = true
		result.Intent = forcedNewIntent()
		result.Resolved = true
		return result
	}

	candidate := continuityCandidateByRunID(candidates, decision.TargetRunID)
	if candidate == nil || (strings.TrimSpace(decision.TargetTaskID) != "" && decision.TargetTaskID != candidate.TaskID) {
		response := d.continuityClarification(ctx, identity, req, candidates, decision.AlternativeRunIDs,
			"The proposed work target was not one of the current candidates.")
		result.Response = &response
		return result
	}
	// Re-read after the model call: the candidate may have completed, resumed,
	// or been replaced while inference was in flight.
	current, ok := d.exactContinuityCandidate(ctx, identity, candidate.RunID)
	if !ok {
		response := d.continuityClarification(ctx, identity, req, candidates, nil,
			"That work item changed while I was matching your message.")
		result.Response = &response
		return result
	}

	switch decision.Action {
	case ContinuityObserve:
		if decision.SecondaryAction == ContinuityNew {
			result.Request.ContinuityContext = continuityProgressContent(current)
			result.Request.ForceNew = true
			result.Intent = forcedNewIntent()
			result.Resolved = true
			return result
		}
		moved := false
		if decision.DeliveryAction == "move_to_current" && current.Active {
			deliveryReq := req
			deliveryReq.Platform = identity.Platform
			deliveryReq.PlatformUserID = identity.PlatformUserID
			moved = d.coordinator().setActiveDeliveryOverride(identity.PersonID, current.RunID, deliveryReq)
		}
		response := d.continuityProgressResponse(ctx, identity, current)
		if moved {
			response.Content += "\nFinal result: this endpoint."
			if response.Turn != nil {
				response.Turn.Message = response.Content
			}
		}
		result.Response = &response
		return result
	case ContinuitySteer:
		active := d.coordinator().currentActive(identity.PersonID)
		if active == nil || active.RunID != current.RunID {
			response := d.continuityClarification(ctx, identity, req, candidates, []string{current.RunID},
				"That run is no longer active, so I did not send it guidance.")
			result.Response = &response
			return result
		}
		result.Request.TaskID = current.TaskID
		result.Request.ReplyToRunID = current.RunID
		if decision.DeliveryAction == "move_to_current" {
			deliveryReq := req
			deliveryReq.Platform = identity.Platform
			deliveryReq.PlatformUserID = identity.PlatformUserID
			d.coordinator().setActiveDeliveryOverride(identity.PersonID, current.RunID, deliveryReq)
		}
		result.Intent = continuityIntent("clear active-run guidance")
		result.Resolved = true
		return result
	case ContinuityResume:
		if !current.Resumable {
			response := d.continuityClarification(ctx, identity, req, candidates, []string{current.RunID},
				"That run is no longer resumable, so I did not start a continuation.")
			result.Response = &response
			return result
		}
		if mode == "safe" {
			response := d.continuityClarification(ctx, identity, req, candidates, []string{current.RunID},
				"I matched one historical run, but safe mode needs confirmation before resuming it.")
			result.Response = &response
			return result
		}
		result.Request.TaskID = current.TaskID
		result.Request.ReplyToRunID = current.RunID
		result.Intent = continuityIntent("clear historical-run continuation")
		result.Resolved = true
		return result
	default:
		response := d.continuityClarification(ctx, identity, req, candidates, nil,
			"I could not safely apply the continuity decision.")
		result.Response = &response
		return result
	}
}

func isStandaloneContinueControl(input string) bool {
	clean := strings.ToLower(strings.Trim(strings.TrimSpace(input), " \t\r\n.!?,;:。！？；：，"))
	switch clean {
	case "continue", "resume", "go on", "keep going", "继续", "接着", "继续吧", "接着来":
		return true
	default:
		return false
	}
}

func continuityRunResumable(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "interrupted", "waiting_user", "verification_partial", "blocked":
		return true
	default:
		return false
	}
}

// buildContinuityCandidates combines structural state, exact references,
// full-history local FTS, and recent-terminal fallback. All expensive
// enrichment happens only after dedupe/ranking, so provider cost and database
// reads remain bounded independently of history size.
func (d *Server) buildContinuityCandidates(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) []ContinuityCandidate {
	if d == nil || d.Control == nil || identity == nil {
		return nil
	}
	type rankedRun struct {
		run      control.Run
		score    int
		evidence map[string]struct{}
	}
	ranked := make(map[string]*rankedRun)
	add := func(run control.Run, score int, evidence string) {
		if run.ID == "" || run.PersonID != identity.PersonID {
			return
		}
		entry := ranked[run.ID]
		if entry == nil {
			entry = &rankedRun{run: run, score: score, evidence: map[string]struct{}{}}
			ranked[run.ID] = entry
		}
		if score > entry.score {
			entry.score = score
		}
		if evidence != "" {
			entry.evidence[evidence] = struct{}{}
		}
	}
	addTask := func(taskID string, score int, evidence string) {
		task, err := d.Control.GetTask(ctx, identity.TenantID, strings.TrimSpace(taskID))
		if err != nil || task == nil || task.PersonID != identity.PersonID || strings.EqualFold(task.Status, "archived") || strings.EqualFold(task.Status, "cancelled") {
			return
		}
		runs, err := d.Control.ListTaskRuns(ctx, identity.TenantID, task.ID, 3)
		if err != nil || len(runs) == 0 {
			return
		}
		add(runs[0], score, evidence)
	}

	active := d.coordinator().currentActive(identity.PersonID)
	if active != nil && strings.TrimSpace(active.RunID) != "" {
		if run, _ := d.Control.GetRun(ctx, identity.TenantID, active.RunID); run != nil {
			add(*run, 1000, "active_run")
		}
	}
	if unresolved, err := d.Control.ListUnresolvedRunsForPerson(ctx, identity.TenantID, identity.PersonID, "", continuityPoolLimit); err == nil {
		for i, run := range unresolved {
			add(run, 850-i, "unresolved_run")
			if run.Channel == req.Channel {
				add(run, 900-i, "same_channel")
			}
		}
	}
	if references, err := d.Control.ListTaskReferenceCards(ctx, identity.TenantID, identity.PersonID,
		[]string{control.TaskReferenceActive, control.TaskReferenceCandidate}, 200); err == nil {
		for _, reference := range references {
			if control.TaskReferenceAppearsInText(req.Content, reference.Reference.RawValue) {
				addTask(reference.Card.TaskID, 920, "exact_reference")
			}
		}
	}
	if d.Sessions != nil && strings.TrimSpace(req.Content) != "" {
		if sessions, err := d.Sessions.SearchSessions(identity.PersonID, req.Content, 12); err == nil {
			for i, session := range sessions {
				if strings.HasPrefix(session.SessionID, "task:") {
					addTask(strings.TrimPrefix(session.SessionID, "task:"), 700-i, "full_history_fts")
				}
			}
		}
	}
	if recent, err := d.Control.ListRecentRunsForPerson(ctx, identity.TenantID, identity.PersonID, 5); err == nil {
		for i, digest := range recent {
			if run, _ := d.Control.GetRun(ctx, identity.TenantID, digest.RunID); run != nil {
				add(*run, 500-i, "recent_fallback")
			}
		}
	}

	ordered := make([]*rankedRun, 0, len(ranked))
	for _, item := range ranked {
		ordered = append(ordered, item)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].score != ordered[j].score {
			return ordered[i].score > ordered[j].score
		}
		if !ordered[i].run.StartedAt.Equal(ordered[j].run.StartedAt) {
			return ordered[i].run.StartedAt.After(ordered[j].run.StartedAt)
		}
		return ordered[i].run.ID < ordered[j].run.ID
	})
	if len(ordered) > continuityPoolLimit {
		ordered = ordered[:continuityPoolLimit]
	}

	out := make([]ContinuityCandidate, 0, min(len(ordered), continuityModelCards))
	for _, item := range ordered {
		if len(out) >= continuityModelCards {
			break
		}
		evidence := make([]string, 0, len(item.evidence))
		for value := range item.evidence {
			evidence = append(evidence, value)
		}
		card, ok := d.continuityCandidateForRun(ctx, identity, item.run, active, item.score, evidence)
		if !ok {
			continue
		}
		out = append(out, card)
	}
	return out
}

func (d *Server) continuityCandidateForRun(ctx context.Context, identity *control.IdentityContext, run control.Run, active *activeRun, score int, evidence []string) (ContinuityCandidate, bool) {
	if d == nil || d.Control == nil || identity == nil || run.PersonID != identity.PersonID {
		return ContinuityCandidate{}, false
	}
	task, err := d.Control.GetTask(ctx, identity.TenantID, run.TaskID)
	if err != nil || task == nil || task.PersonID != identity.PersonID {
		return ContinuityCandidate{}, false
	}
	card := ContinuityCandidate{
		RunID: run.ID, TaskID: run.TaskID, Title: textutil.Truncate(toOneLine(task.Title), 96),
		TaskStatus: task.Status, RunStatus: run.Status, Channel: run.Channel,
		InputSummary: textutil.Truncate(toOneLine(run.InputSummary), 180),
		Age:          relativeAge(run.StartedAt), Active: active != nil && active.RunID == run.ID,
		Resumable: continuityRunResumable(run.Status), score: score, startedAt: run.StartedAt,
		Evidence: append([]string(nil), evidence...),
	}
	sort.Strings(card.Evidence)
	if workspaceID := firstNonEmpty(run.WorkspaceID, task.WorkspaceID); workspaceID != "" {
		if workspace, _ := d.Control.GetWorkspace(ctx, identity.TenantID, workspaceID); workspace != nil && workspace.OwnerPersonID == identity.PersonID {
			card.Workspace = strings.TrimSpace(workspace.Name)
			if card.Workspace == "" {
				card.Workspace = filepath.Base(workspace.LocalPath)
			}
		}
	}
	if handoff, _ := d.Control.RunHandoff(ctx, identity.TenantID, identity.PersonID, run.ID); handoff != nil {
		card.HandoffSummary = textutil.Truncate(toOneLine(handoff.Summary), 220)
		card.NextSteps = boundedOneLineStrings(handoff.NextSteps, 3, 120)
		card.Risks = boundedOneLineStrings(handoff.Risks, 2, 120)
	}
	if plan := d.latestPlanForRun(ctx, identity.TenantID, identity.PersonID, run.TaskID, run.ID); len(plan) > 0 {
		for _, step := range plan {
			if step.Status == "in_progress" {
				card.CurrentStep = textutil.Truncate(toOneLine(step.Step), 140)
				break
			}
		}
	}
	card.LastActivity = d.latestActivityForRun(ctx, identity, run)
	return card, true
}

func (d *Server) exactContinuityCandidate(ctx context.Context, identity *control.IdentityContext, runID string) (ContinuityCandidate, bool) {
	if d == nil || d.Control == nil || identity == nil || strings.TrimSpace(runID) == "" {
		return ContinuityCandidate{}, false
	}
	run, err := d.Control.GetRun(ctx, identity.TenantID, strings.TrimSpace(runID))
	if err != nil || run == nil || run.PersonID != identity.PersonID {
		return ContinuityCandidate{}, false
	}
	return d.continuityCandidateForRun(ctx, identity, *run, d.coordinator().currentActive(identity.PersonID), 0, []string{"explicit_choice"})
}

func boundedOneLineStrings(values []string, limit, maxChars int) []string {
	out := make([]string, 0, min(len(values), limit))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out = append(out, textutil.Truncate(toOneLine(value), maxChars))
		if len(out) == limit {
			break
		}
	}
	return out
}

func (d *Server) latestActivityForRun(ctx context.Context, identity *control.IdentityContext, run control.Run) string {
	if d == nil || d.Control == nil || identity == nil {
		return ""
	}
	events, err := d.Control.ListRunEvents(ctx, identity.TenantID, identity.PersonID, run.TaskID, run.ID, 30)
	if err != nil {
		return ""
	}
	for _, event := range events {
		var payload struct {
			Tool    string `json:"tool"`
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		switch event.Type {
		case "tool.started":
			if payload.Tool != "" {
				return "running " + payload.Tool
			}
		case "tool.completed":
			if payload.Tool != "" && payload.Error != "" {
				return textutil.Truncate(payload.Tool+" failed: "+toOneLine(payload.Error), 140)
			}
			if payload.Tool != "" {
				return payload.Tool + " finished"
			}
		case "agent.thinking", "agent.step":
			if strings.TrimSpace(payload.Message) != "" {
				return textutil.Truncate(toOneLine(payload.Message), 140)
			}
		}
	}
	return ""
}

func continuityCandidateByRunID(candidates []ContinuityCandidate, runID string) *ContinuityCandidate {
	runID = strings.TrimSpace(runID)
	for i := range candidates {
		if candidates[i].RunID == runID {
			copy := candidates[i]
			return &copy
		}
	}
	return nil
}

func continuityCandidateIDs(candidates []ContinuityCandidate) []string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.RunID)
	}
	return ids
}

func continuityResolutionErrorClass(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(strings.ToLower(err.Error()), "deadline") || strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return "timeout"
	}
	return "provider"
}

func (d *Server) recordContinuityResolution(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, candidates []ContinuityCandidate, resolution ContinuityResolution, mode, errorClass string) string {
	if d == nil || d.Control == nil || identity == nil {
		return ""
	}
	id, _ := d.Control.RecordTurnResolution(context.WithoutCancel(ctx), control.TurnResolutionRecord{
		TenantID: identity.TenantID, PersonID: identity.PersonID, AccountID: identity.AccountID,
		Channel: req.Channel, Input: req.Content, Mode: mode,
		Decision: string(resolution.Decision.Action), Certainty: string(resolution.Decision.Certainty),
		TargetTaskID: resolution.Decision.TargetTaskID, TargetRunID: resolution.Decision.TargetRunID,
		CandidateIDs: continuityCandidateIDs(candidates), Evidence: resolution.Decision.Evidence,
		Provider: resolution.Provider, Model: resolution.Model, Latency: resolution.Latency, ErrorClass: errorClass,
	})
	return id
}

func continuityChoiceLabel(candidate ContinuityCandidate) string {
	parts := []string{textutil.Truncate(candidate.Title, 48), candidate.RunStatus, candidate.Age}
	if candidate.InputSummary != "" && candidate.InputSummary != candidate.Title {
		parts = append(parts, textutil.Truncate(candidate.InputSummary, 64))
	}
	return strings.Join(parts, " · ")
}

func (d *Server) continuityClarification(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, candidates []ContinuityCandidate, preferred []string, reason string) api.MessageResponse {
	ordered := make([]ContinuityCandidate, 0, 3)
	seen := map[string]bool{}
	for _, id := range preferred {
		if candidate := continuityCandidateByRunID(candidates, id); candidate != nil && !seen[id] {
			ordered = append(ordered, *candidate)
			seen[id] = true
		}
	}
	for _, candidate := range candidates {
		if len(ordered) >= 3 {
			break
		}
		if !seen[candidate.RunID] {
			ordered = append(ordered, candidate)
			seen[candidate.RunID] = true
		}
	}
	options := make([]control.TurnChoiceOption, 0, len(ordered)+1)
	var sb strings.Builder
	if strings.TrimSpace(reason) == "" {
		reason = "I found more than one plausible work item."
	}
	sb.WriteString(textutil.Truncate(toOneLine(reason), 160))
	sb.WriteString(" Which one did you mean?\n")
	for i, candidate := range ordered {
		key := fmt.Sprintf("%d", i+1)
		action := "observe"
		if candidate.Active {
			action = "steer"
		} else if candidate.Resumable {
			action = "resume"
		}
		label := continuityChoiceLabel(candidate)
		options = append(options, control.TurnChoiceOption{Key: key, Label: label, Action: action, TaskID: candidate.TaskID, RunID: candidate.RunID})
		fmt.Fprintf(&sb, "%s. %s\n", key, label)
	}
	newKey := fmt.Sprintf("%d", len(options)+1)
	options = append(options, control.TurnChoiceOption{Key: newKey, Label: "This is new work", Action: "new"})
	fmt.Fprintf(&sb, "%s. This is new work\n", newKey)
	choice, err := d.createTurnChoice(ctx, identity, req, options)
	if err == nil {
		fmt.Fprintf(&sb, "Reply with a number, or use /choose %s <number> from another endpoint.", choice.ID)
	} else {
		sb.WriteString("Use /resume <run_id> or /new --run <request> to choose explicitly.")
	}
	content := sb.String()
	return api.MessageResponse{Identity: identity, Content: content, Choice: choice,
		Turn: messageTurn("waiting_user", "", "idle", "", "", content)}
}

func (d *Server) continuityProgressResponse(ctx context.Context, identity *control.IdentityContext, candidate ContinuityCandidate) api.MessageResponse {
	content := continuityProgressContent(candidate)
	task, _ := d.Control.GetTask(ctx, identity.TenantID, candidate.TaskID)
	run, _ := d.Control.GetRun(ctx, identity.TenantID, candidate.RunID)
	return api.MessageResponse{Identity: identity, Task: task, Run: run, Content: content,
		Turn: messageTurn("completed", candidate.TaskStatus, "idle", candidate.TaskID, candidate.RunID, content)}
}

func continuityProgressContent(candidate ContinuityCandidate) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s · %s\n", candidate.Title, candidate.RunStatus)
	if candidate.CurrentStep != "" {
		fmt.Fprintf(&sb, "Current step: %s\n", candidate.CurrentStep)
	}
	if candidate.LastActivity != "" {
		fmt.Fprintf(&sb, "Latest activity: %s\n", candidate.LastActivity)
	}
	if candidate.HandoffSummary != "" {
		fmt.Fprintf(&sb, "Latest result: %s\n", candidate.HandoffSummary)
	}
	if len(candidate.NextSteps) > 0 {
		sb.WriteString("Next: " + strings.Join(candidate.NextSteps, "; ") + "\n")
	}
	fmt.Fprintf(&sb, "Last recorded: %s · run %s", candidate.Age, shortRunID(candidate.RunID))
	content := strings.TrimSpace(sb.String())
	return content
}
