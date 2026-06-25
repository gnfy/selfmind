package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/textutil"
	"selfmind/internal/tools"
)

type Server struct {
	Control           *control.Store
	Gateway           *router.Gateway
	Delivery          *delivery.Service
	DefaultTenantID   string
	DrainTimeout      time.Duration
	ShutdownFunc      func()
	RuntimeStatusFunc func() api.GatewayRuntimeInfo

	mu           sync.Mutex
	active       map[string]*activeRun
	draining     bool
	drainReason  string
	shutdownOnce sync.Once
}

type activeRun struct {
	TenantID  string
	PersonID  string
	TaskID    string
	RunID     string
	Channel   string
	Summary   string
	StartedAt time.Time
	Cancel    context.CancelFunc
}

func (d *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", d.handleHealth)
	mux.HandleFunc("/v1/message", d.handleMessage)
	mux.HandleFunc("/v1/im/", d.handleIMWebhook)
	mux.HandleFunc("/v1/accounts/bind", d.handleAccountBind)
	mux.HandleFunc("/v1/approvals", d.handleApprovals)
	mux.HandleFunc("/v1/approvals/respond", d.handleApprovalRespond)
	mux.HandleFunc("/v1/tasks/events", d.handleTaskEvents)
	mux.HandleFunc("/v1/tasks/events/stream", d.handleTaskEventsStream)
	mux.HandleFunc("/v1/tasks/artifacts", d.handleTaskArtifacts)
	mux.HandleFunc("/v1/tasks", d.handleTasks)
	mux.HandleFunc("/v1/tasks/current", d.handleCurrentTask)
	mux.HandleFunc("/v1/workspaces/register", d.handleWorkspaceRegister)
	mux.HandleFunc("/v1/workspaces", d.handleWorkspaces)
	mux.HandleFunc("/v1/gateway/status", d.handleGatewayStatus)
	mux.HandleFunc("/v1/gateway/shutdown", d.handleGatewayShutdown)
	return mux
}

func (d *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (d *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req api.MessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	resp, status := d.ProcessMessage(r.Context(), req)
	writeJSON(w, status, resp)
}

func (d *Server) ProcessMessage(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
	req.TenantID = d.tenantID(req.TenantID)
	req.Platform = fallback(req.Platform, "cli")
	req.PlatformUserID = fallback(req.PlatformUserID, "local")
	req.Channel = fallback(req.Channel, req.Platform)
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" {
		return api.MessageResponse{Error: "content is required", Turn: messageTurn("failed", "", "", "", "", "content is required")}, http.StatusBadRequest
	}

	identity, err := d.Control.ResolveOrCreateAccount(ctx, req.TenantID, req.Platform, req.PlatformUserID, req.DisplayName)
	if err != nil {
		return api.MessageResponse{Error: err.Error(), Turn: messageTurn("failed", "", "", "", "", err.Error())}, http.StatusInternalServerError
	}

	if handled, content, err := d.tryHandleControlCommand(ctx, identity, req); handled {
		if err != nil {
			return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "", "", "", err.Error())}, http.StatusInternalServerError
		}
		return api.MessageResponse{Identity: identity, Content: content, Turn: messageTurn("completed", "", "idle", "", "", "")}, http.StatusOK
	}

	if d.IsDraining() {
		return api.MessageResponse{
			Identity: identity,
			Error:    "gateway is shutting down; try again after restart",
			Accepted: false,
			Turn:     messageTurn("failed", "", "draining", "", "", "gateway is shutting down"),
		}, http.StatusServiceUnavailable
	}

	intent := d.classifyIntent(ctx, req.Content, req.Channel)
	if intent.Intent != router.IntentContinue && looksLikeAffirmativeContinuation(req.Content) {
		if task, _ := d.resolveContinueTask(ctx, identity); task != nil {
			intent = router.IntentResult{
				Intent:           router.IntentContinue,
				Confidence:       0.84,
				Reason:           "affirmative reply with existing task context",
				Signals:          []string{"continue.affirmative_with_context"},
				ShouldCreateTask: true,
				ShouldUseTools:   true,
				Source:           "httpapi",
			}
		}
	}
	if handled, resp := d.tryHandleIntentClarification(identity, intent); handled {
		return resp, http.StatusOK
	}
	if handled, resp := d.tryHandleDirectIntent(ctx, identity, req, intent); handled {
		return resp, http.StatusOK
	}

	if running := d.currentActive(identity.PersonID); running != nil {
		return api.MessageResponse{
			Identity: identity,
			Content:  formatBusyRun(running),
			Accepted: false,
			Turn:     messageTurn("busy", "running", "running", running.TaskID, running.RunID, running.Summary),
		}, http.StatusOK
	}

	if req.Async {
		return d.startAsyncRun(identity, req, intent), http.StatusOK
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if ok := d.beginActive(identity.PersonID, &activeRun{
		TenantID:  identity.TenantID,
		PersonID:  identity.PersonID,
		Channel:   req.Channel,
		Summary:   truncate(req.Content, 240),
		StartedAt: time.Now(),
		Cancel:    cancel,
	}); !ok {
		return api.MessageResponse{Identity: identity, Content: "Another task is already running. Use /status or /stop.", Turn: messageTurn("busy", "running", "running", "", "", "")}, http.StatusOK
	}
	defer d.endActive(identity.PersonID)

	return d.runMessage(runCtx, identity, req, intent)
}

func (d *Server) prepareRequestWorkspace(ctx context.Context, identity *control.IdentityContext, req *api.MessageRequest) (*control.Workspace, error) {
	if d == nil || d.Control == nil || identity == nil || req == nil {
		return nil, nil
	}
	if strings.TrimSpace(req.WorkspaceID) != "" {
		return d.Control.GetWorkspace(ctx, identity.TenantID, req.WorkspaceID)
	}
	if !isLocalCLIRequest(*req) {
		return nil, nil
	}
	cwd := cleanClientCWD(req.ClientCWD)
	if cwd == "" {
		return nil, nil
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	ws, err := d.Control.RegisterWorkspace(ctx, control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          filepath.Base(cwd),
		LocalPath:     cwd,
		AllowedRoots:  []string{cwd},
	})
	if err != nil {
		return nil, err
	}
	if ws != nil {
		req.WorkspaceID = ws.ID
	}
	return ws, nil
}

func cleanClientCWD(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n") {
		return ""
	}
	clean := filepath.Clean(raw)
	if !filepath.IsAbs(clean) {
		abs, err := filepath.Abs(clean)
		if err != nil {
			return ""
		}
		clean = abs
	}
	return clean
}

func isLocalCLIRequest(req api.MessageRequest) bool {
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	channel := strings.ToLower(strings.TrimSpace(req.Channel))
	switch platform {
	case "cli", "terminal", "tui":
		return true
	}
	switch channel {
	case "cli", "terminal", "tui":
		return true
	}
	return false
}

func messageTurn(status, taskStatus, backgroundStatus, taskID, runID, message string) *api.TurnStatus {
	return &api.TurnStatus{
		Status:           strings.TrimSpace(status),
		TaskStatus:       strings.TrimSpace(taskStatus),
		BackgroundStatus: strings.TrimSpace(backgroundStatus),
		TaskID:           strings.TrimSpace(taskID),
		RunID:            strings.TrimSpace(runID),
		Message:          strings.TrimSpace(message),
	}
}

func messageContextBudget(usage llm.UsageStats) *api.ContextBudgetInfo {
	budget := kernel.DefaultRuntimeContextBudget()
	return &api.ContextBudgetInfo{
		TotalChars:            budget.TotalChars,
		WorkspaceChars:        budget.WorkspaceChars,
		TaskChars:             budget.TaskChars,
		MemoryChars:           budget.MemoryChars,
		EstimatedInputTokens:  usage.InputTokens,
		EstimatedOutputTokens: usage.OutputTokens,
	}
}

func llmUsageZero() llm.UsageStats {
	return llm.UsageStats{}
}

func (d *Server) runMessage(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) (api.MessageResponse, int) {
	if _, err := d.prepareRequestWorkspace(ctx, identity, &req); err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	task, err := d.resolveTask(ctx, identity, req, intent)
	if err != nil {
		if strings.Contains(err.Error(), "no task to continue") {
			return api.MessageResponse{Identity: identity, Content: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusOK
		}
		return api.MessageResponse{Identity: identity, Error: err.Error(), Turn: messageTurn("failed", "", "idle", "", "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	_ = d.Control.RecordChannelMessage(ctx, *identity, req.Channel, task.ID, "user", req.Content)

	run, err := d.Control.StartRun(ctx, task, req.Channel, truncate(req.Content, 240))
	if err != nil {
		return api.MessageResponse{Identity: identity, Task: task, Error: err.Error(), Turn: messageTurn("failed", task.Status, "idle", task.ID, "", err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}
	stopHeartbeat := d.startRunHeartbeat(ctx, run)
	defer stopHeartbeat()
	d.updateActive(identity.PersonID, task, run)
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      run.ID,
		Type:       "run.started",
		Visibility: "task",
		Channel:    req.Channel,
		Payload:    mustJSON(map[string]string{"input": truncate(req.Content, 500)}),
	})

	workspace, _ := d.workspaceForTask(ctx, identity, task, req)
	cleanupScope := d.installExecutionScope(identity, task, run, workspace)
	defer cleanupScope()
	if workspace != nil && workspace.LocalPath != "" {
		ctx = kernel.WithWorkspaceContext(ctx, kernel.WorkspaceContext{
			ID:   workspace.ID,
			Root: workspace.LocalPath,
		})
	}
	ctx = kernel.WithTaskRuntimeContext(ctx, d.selectedTaskRuntimeContext(ctx, task, run, workspace, req.Channel))
	agentInput := d.withGatewayContext(req.Content, identity, task, workspace, req.Attachments)
	agentInput = d.withResumeContext(ctx, identity, task, run, intent, agentInput)
	ctx = kernel.WithTaskStrategy(ctx, taskStrategyForRequest(req, intent))

	if d.Gateway == nil {
		err := fmt.Errorf("gateway is not configured")
		_ = d.Control.FinishRun(context.Background(), identity.TenantID, run.ID, "failed")
		_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, "blocked", err.Error(), nil)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error(), Turn: messageTurn("failed", "blocked", "idle", task.ID, run.ID, err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusInternalServerError
	}

	resp, err := d.Gateway.RunAgentWithEvents(ctx, identity.PersonID, req.Channel, agentInput)
	if err != nil {
		status := "failed"
		taskStatus := "blocked"
		if ctx.Err() != nil {
			status = "cancelled"
			taskStatus = "cancelled"
		}
		_ = d.Control.FinishRun(context.Background(), identity.TenantID, run.ID, status)
		_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, taskStatus, err.Error(), nil)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error(), Turn: messageTurn(status, taskStatus, "idle", task.ID, run.ID, err.Error()), Context: messageContextBudget(llmUsageZero())}, http.StatusOK
	}

	content, usage, err := d.aggregateGatewayResponse(ctx, req.Channel, task, run, resp)
	if err != nil {
		status := "failed"
		taskStatus := "blocked"
		if ctx.Err() != nil {
			status = "cancelled"
			taskStatus = "cancelled"
		}
		_ = d.Control.FinishRun(context.Background(), identity.TenantID, run.ID, status)
		_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, taskStatus, err.Error(), nil)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error(), Turn: messageTurn(status, taskStatus, "idle", task.ID, run.ID, err.Error()), Context: messageContextBudget(usage)}, http.StatusOK
	}

	outcome := buildRunOutcome(content)
	if structured, ok := d.latestStructuredRunOutcome(ctx, task.ID, run.ID); ok {
		outcome = structured
		if outcome.Summary == "" {
			outcome.Summary = buildRunOutcome(content).Summary
		}
	}
	_ = d.Control.RecordChannelMessage(ctx, *identity, req.Channel, task.ID, "assistant", content)
	_ = d.Control.FinishRun(ctx, identity.TenantID, run.ID, outcome.Status)
	_ = d.Control.UpdateTaskStatus(ctx, identity.TenantID, task.ID, outcome.Status, outcome.Summary, outcome.NextSteps)
	_, _ = d.Control.SaveHandoff(ctx, control.Handoff{
		TaskID:       task.ID,
		Summary:      outcome.Summary,
		DoneItems:    outcome.Done,
		NextSteps:    outcome.NextSteps,
		ChangedFiles: outcome.Files,
		TestStatus:   strings.Join(outcome.Tests, "\n"),
		Risks:        outcome.Risks,
	})
	d.recordOutcomeArtifacts(ctx, task, run, req.Channel, outcome.Files)
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      run.ID,
		Type:       "run.finished",
		Visibility: "task",
		Channel:    req.Channel,
		Payload:    mustJSON(map[string]interface{}{"outcome": outcome, "usage": usage}),
	})
	taskID := task.ID
	task, _ = d.Control.GetTask(ctx, identity.TenantID, task.ID)
	taskStatus := outcome.Status
	if task != nil && task.Status != "" {
		taskStatus = task.Status
	}
	return api.MessageResponse{Identity: identity, Task: task, Run: run, Outcome: &outcome, Content: content, Usage: usage, Turn: messageTurn("completed", taskStatus, "idle", taskID, run.ID, outcome.Summary), Context: messageContextBudget(usage)}, http.StatusOK
}

func (d *Server) recordOutcomeArtifacts(ctx context.Context, task *control.Task, run *control.Run, channel string, files []string) {
	if d == nil || d.Control == nil || task == nil {
		return
	}
	seen := map[string]struct{}{}
	for _, uri := range files {
		uri = strings.TrimSpace(uri)
		if uri == "" {
			continue
		}
		if _, ok := seen[uri]; ok {
			continue
		}
		seen[uri] = struct{}{}
		artifact := control.Artifact{
			TaskID: task.ID,
			Kind:   artifactKind(uri),
			Name:   artifactName(uri),
			URI:    uri,
			Metadata: mustJSON(map[string]interface{}{
				"source": "run_outcome",
			}),
		}
		if run != nil {
			artifact.RunID = run.ID
		}
		saved, err := d.Control.SaveArtifact(ctx, artifact)
		if err != nil {
			continue
		}
		runID := ""
		if run != nil {
			runID = run.ID
		}
		_, _ = d.Control.AppendEvent(ctx, control.Event{
			TaskID:     task.ID,
			RunID:      runID,
			Type:       "artifact.created",
			Visibility: "task",
			Channel:    channel,
			Payload: mustJSON(map[string]interface{}{
				"artifact": saved,
			}),
		})
	}
}

func artifactKind(uri string) string {
	lower := strings.ToLower(strings.TrimSpace(uri))
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return "link"
	}
	return "file"
}

func artifactName(uri string) string {
	clean := strings.TrimSpace(uri)
	if clean == "" {
		return ""
	}
	if strings.Contains(clean, "://") {
		parts := strings.Split(strings.TrimRight(clean, "/"), "/")
		if len(parts) > 0 && parts[len(parts)-1] != "" {
			return parts[len(parts)-1]
		}
		return clean
	}
	name := filepath.Base(filepath.FromSlash(clean))
	if name == "." || name == string(filepath.Separator) {
		return clean
	}
	return name
}

func (d *Server) startAsyncRun(identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) api.MessageResponse {
	active := &activeRun{
		TenantID:  identity.TenantID,
		PersonID:  identity.PersonID,
		Channel:   req.Channel,
		Summary:   truncate(req.Content, 240),
		StartedAt: time.Now(),
	}
	if ok := d.beginActive(identity.PersonID, active); !ok {
		return api.MessageResponse{Identity: identity, Content: "Another task is already running. Use /status or /stop.", Turn: messageTurn("busy", "running", "running", "", "", "")}
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	active.Cancel = runCancel
	stopProgressNotices := d.startAsyncProgressNotices(runCtx, identity, req)
	go func() {
		defer d.endActive(identity.PersonID)
		defer runCancel()
		defer stopProgressNotices()
		resp, _ := d.runMessage(runCtx, identity, req, intent)
		d.deliverAsyncResult(context.Background(), identity, req, resp)
	}()

	notice := router.WorkingNotice(req.Channel)
	if notice == "" {
		notice = "Started in the background. Use /status to check progress or /stop to cancel."
	}
	return api.MessageResponse{
		Identity: identity,
		Content:  notice,
		Accepted: true,
		Turn:     messageTurn("accepted", "running", "running", "", "", notice),
	}
}

func (d *Server) resolveTask(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, intent router.IntentResult) (*control.Task, error) {
	if req.TaskID != "" {
		task, err := d.Control.GetTask(ctx, identity.TenantID, req.TaskID)
		if err != nil || task != nil {
			return d.bindTaskWorkspaceIfMissing(ctx, identity, task, req, err)
		}
	}
	if intent.Intent == router.IntentContinue {
		task, err := d.resolveContinueTask(ctx, identity)
		if err != nil || task != nil {
			return d.bindTaskWorkspaceIfMissing(ctx, identity, task, req, err)
		}
		return nil, fmt.Errorf("no task to continue; start a new task or use /resume <task_id>")
	}
	task, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return nil, err
	}
	if task != nil {
		return d.bindTaskWorkspaceIfMissing(ctx, identity, task, req, nil)
	}
	workspaceID := req.WorkspaceID
	if workspaceID == "" {
		if ws, _ := d.Control.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID); ws != nil {
			workspaceID = ws.ID
		}
	}
	return d.Control.CreateTask(ctx, control.TaskCreate{
		TenantID:    identity.TenantID,
		PersonID:    identity.PersonID,
		WorkspaceID: workspaceID,
		Title:       titleFromInput(req.Content),
		Channel:     req.Channel,
	})
}

func (d *Server) bindTaskWorkspaceIfMissing(ctx context.Context, identity *control.IdentityContext, task *control.Task, req api.MessageRequest, priorErr error) (*control.Task, error) {
	if priorErr != nil || task == nil || identity == nil {
		return task, priorErr
	}
	if task.WorkspaceID != "" || strings.TrimSpace(req.WorkspaceID) == "" {
		return task, nil
	}
	if err := d.Control.SetTaskWorkspace(ctx, identity.TenantID, task.ID, req.WorkspaceID); err != nil {
		return nil, err
	}
	refreshed, err := d.Control.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || refreshed == nil {
		return task, err
	}
	return refreshed, nil
}

func (d *Server) workspaceForTask(ctx context.Context, identity *control.IdentityContext, task *control.Task, req api.MessageRequest) (*control.Workspace, error) {
	workspaceID := req.WorkspaceID
	if workspaceID == "" && task != nil {
		workspaceID = task.WorkspaceID
	}
	if workspaceID == "" {
		return d.Control.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID)
	}
	return d.Control.GetWorkspace(ctx, identity.TenantID, workspaceID)
}

func (d *Server) installExecutionScope(identity *control.IdentityContext, task *control.Task, run *control.Run, workspace *control.Workspace) func() {
	if identity == nil {
		return func() {}
	}
	scope := tools.ExecutionScope{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
	}
	if workspace != nil {
		scope.WorkspaceID = workspace.ID
		scope.WorkspaceRoot = workspace.LocalPath
		scope.AllowedRoots = workspace.AllowedRoots
	}
	if task != nil {
		scope.TaskID = task.ID
	}
	if run != nil {
		scope.RunID = run.ID
		scope.Channel = run.Channel
	}
	scope.Approval = d.toolApprovalHandler(identity, task, run, scope.Channel)
	return tools.SetExecutionScope(identity.PersonID, scope)
}

func (d *Server) toolApprovalHandler(identity *control.IdentityContext, task *control.Task, run *control.Run, channel string) tools.ToolApprovalHandler {
	return func(ctx context.Context, req tools.ToolApprovalRequest) (tools.ToolApprovalDecision, error) {
		if d == nil || d.Control == nil || identity == nil {
			return tools.ToolApprovalDecision{}, fmt.Errorf("approval store is not available")
		}
		if ctx == nil {
			ctx = context.Background()
		}
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()

		taskID := req.TaskID
		runID := req.RunID
		if taskID == "" && task != nil {
			taskID = task.ID
		}
		if runID == "" && run != nil {
			runID = run.ID
		}
		approval, err := d.Control.CreateApprovalRequest(waitCtx, control.ApprovalRequest{
			TenantID:         identity.TenantID,
			PersonID:         identity.PersonID,
			TaskID:           taskID,
			RunID:            runID,
			ActionType:       "tool_call",
			RequestedChannel: fallback(channel, identity.Platform),
			Payload: mustJSON(map[string]interface{}{
				"tool":   req.ToolName,
				"reason": req.Reason,
				"args":   redactApprovalArgs(req.Args),
			}),
		})
		if err != nil {
			return tools.ToolApprovalDecision{}, err
		}
		if taskID != "" {
			_, _ = d.Control.AppendEvent(waitCtx, control.Event{
				TaskID:     taskID,
				RunID:      runID,
				Type:       "approval.requested",
				Visibility: "task",
				Channel:    fallback(channel, identity.Platform),
				Payload: mustJSON(map[string]interface{}{
					"approval_id": approval.ID,
					"action_type": approval.ActionType,
					"tool":        req.ToolName,
					"reason":      req.Reason,
				}),
			})
		}
		d.notifyApprovalRequested(context.Background(), identity, taskID, runID, fallback(channel, identity.Platform), approval, req)

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				return tools.ToolApprovalDecision{ApprovalID: approval.ID, Reason: waitCtx.Err().Error()}, waitCtx.Err()
			case <-ticker.C:
				current, err := d.Control.GetApprovalRequest(waitCtx, identity.TenantID, approval.ID)
				if err != nil {
					return tools.ToolApprovalDecision{ApprovalID: approval.ID}, err
				}
				if current == nil {
					return tools.ToolApprovalDecision{ApprovalID: approval.ID}, fmt.Errorf("approval request disappeared: %s", approval.ID)
				}
				switch current.Status {
				case "approved":
					return tools.ToolApprovalDecision{Approved: true, ApprovalID: approval.ID}, nil
				case "rejected":
					return tools.ToolApprovalDecision{Approved: false, ApprovalID: approval.ID, Reason: "rejected"}, nil
				}
			}
		}
	}
}

func redactApprovalArgs(args map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{})
	for k, v := range args {
		out[k] = tools.RedactSensitive(fmt.Sprintf("%v", v))
	}
	return out
}

func (d *Server) notifyApprovalRequested(ctx context.Context, identity *control.IdentityContext, taskID, runID, channel string, approval *control.ApprovalRequest, req tools.ToolApprovalRequest) {
	if d == nil || d.Delivery == nil || identity == nil || approval == nil {
		return
	}
	if identity.Platform == "cli" && channel == "cli" {
		return
	}
	content := fmt.Sprintf("Approval required for %s: %s\nApproval: %s\nReply with /approve %s or /reject %s.", req.ToolName, req.Reason, approval.ID, approval.ID, approval.ID)
	_ = d.Delivery.EnqueueAndTry(ctx, delivery.Message{
		TenantID:       identity.TenantID,
		PersonID:       identity.PersonID,
		Platform:       identity.Platform,
		PlatformUserID: identity.PlatformUserID,
		Channel:        channel,
		TaskID:         taskID,
		RunID:          runID,
		Content:        content,
	})
}

func (d *Server) withGatewayContext(input string, identity *control.IdentityContext, task *control.Task, workspace *control.Workspace, attachments []api.MessageAttachment) string {
	if (workspace == nil || workspace.LocalPath == "" || task == nil) && len(attachments) == 0 {
		return input
	}
	var sb strings.Builder
	sb.WriteString("[SelfMind daemon context]\n")
	if identity != nil {
		fmt.Fprintf(&sb, "person_id: %s\n", identity.PersonID)
		fmt.Fprintf(&sb, "platform: %s\n", identity.Platform)
		fmt.Fprintf(&sb, "platform_user_id: %s\n", identity.PlatformUserID)
	}
	if task != nil {
		fmt.Fprintf(&sb, "task_id: %s\n", task.ID)
	}
	if workspace != nil && workspace.LocalPath != "" {
		fmt.Fprintf(&sb, "workspace_id: %s\n", workspace.ID)
		fmt.Fprintf(&sb, "workspace_root: %s\n", workspace.LocalPath)
		sb.WriteString("Use workspace_root as the default cwd for local file tools.\n")
		sb.WriteString("When the user says current project, this repo, this codebase, or names a project without an explicit path, inspect workspace_root first.\n")
		sb.WriteString("Resolve relative paths against workspace_root. Do not access files outside workspace allowed roots.\n")
	}
	if len(attachments) > 0 {
		sb.WriteString("attachments:\n")
		for i, att := range attachments {
			fmt.Fprintf(&sb, "- index: %d\n", i+1)
			if att.Kind != "" {
				fmt.Fprintf(&sb, "  kind: %s\n", att.Kind)
			}
			if att.Path != "" {
				fmt.Fprintf(&sb, "  path: %s\n", att.Path)
			}
			if att.MimeType != "" {
				fmt.Fprintf(&sb, "  mime_type: %s\n", att.MimeType)
			}
			if att.Name != "" {
				fmt.Fprintf(&sb, "  name: %s\n", att.Name)
			}
			if att.Size > 0 {
				fmt.Fprintf(&sb, "  size: %d\n", att.Size)
			}
		}
		sb.WriteString("When useful, inspect attachment paths with local tools before answering.\n")
	}
	sb.WriteString("[/SelfMind daemon context]\n\n")
	sb.WriteString(input)
	return sb.String()
}

func (d *Server) tryHandleControlCommand(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (bool, string, error) {
	trimmed := strings.TrimSpace(req.Content)
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "/") {
		return false, "", nil
	}
	switch {
	case lower == "/help":
		return true, strings.TrimSpace(`SelfMind commands:
/help                 Show this help.
/model                Show the configured model.
/id                   Show your resolved account identity.
/status               Show the current task status.
/tasks                List recent tasks.
/events               List recent events for the current task.
/approvals            List pending approvals.
/approve <id>         Approve a pending action.
/reject <id>          Reject a pending action.
/stop                 Cancel the active run.
/new [title]          Create a new task.
/resume <task_id>     Resume a task.
/workspace <id>       Select a workspace.
/workspaces           List workspaces.`), nil
	case lower == "/model":
		if d != nil && d.Gateway != nil {
			return true, d.Gateway.ModelStatusReply(), nil
		}
		return true, "SelfMind is running, but the model gateway is not configured.", nil
	case lower == "/id":
		return true, formatIdentity(identity), nil
	case lower == "/stop":
		active := d.stopActive(identity.PersonID)
		if active == nil {
			return true, "No active run to stop.", nil
		}
		if active.RunID != "" {
			_ = d.Control.RequestRunCancel(context.Background(), identity.TenantID, active.RunID)
			_ = d.Control.FinishRun(context.Background(), identity.TenantID, active.RunID, "cancelled")
		}
		if active.TaskID != "" {
			_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, active.TaskID, "cancelled", "Cancelled by user.", nil)
			_, _ = d.Control.AppendEvent(context.Background(), control.Event{
				TaskID:     active.TaskID,
				RunID:      active.RunID,
				Type:       "run.cancelled",
				Visibility: "task",
				Channel:    req.Channel,
				Payload:    mustJSON(map[string]string{"reason": "user requested stop"}),
			})
		}
		return true, fmt.Sprintf("Stopping run %s.", fallback(active.RunID, "(starting)")), nil
	case strings.HasPrefix(lower, "/new"):
		title := strings.TrimSpace(trimmed[len("/new"):])
		if title == "" {
			title = "New task"
		}
		workspaceID := req.WorkspaceID
		if workspaceID == "" {
			if ws, _ := d.Control.CurrentWorkspace(ctx, identity.TenantID, identity.PersonID); ws != nil {
				workspaceID = ws.ID
			}
		}
		task, err := d.Control.CreateTask(ctx, control.TaskCreate{
			TenantID:    identity.TenantID,
			PersonID:    identity.PersonID,
			WorkspaceID: workspaceID,
			Title:       title,
			Channel:     req.Channel,
		})
		if err != nil {
			return true, "", err
		}
		return true, fmt.Sprintf("Created task: %s (%s)", task.Title, task.ID), nil
	case lower == "/task status" || lower == "/status":
		task, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			return true, "", err
		}
		if task == nil {
			return true, "No active task.", nil
		}
		handoff, _ := d.Control.LatestHandoff(ctx, task.ID)
		plan := d.latestPlanForTask(ctx, task.ID)
		return true, formatTaskStatus(task, handoff, d.currentActive(identity.PersonID), plan), nil
	case strings.HasPrefix(lower, "/resume ") || strings.HasPrefix(lower, "/task "):
		parts := strings.Fields(trimmed)
		if len(parts) < 2 {
			return true, "Usage: /resume <task_id>", nil
		}
		task, err := d.Control.GetTask(ctx, identity.TenantID, parts[1])
		if err != nil {
			return true, "", err
		}
		if task == nil || task.PersonID != identity.PersonID {
			return true, "Task not found.", nil
		}
		if err := d.Control.SetCurrentTask(ctx, identity.TenantID, identity.PersonID, task.ID); err != nil {
			return true, "", err
		}
		return true, fmt.Sprintf("Resumed task: %s (%s)", task.Title, task.ID), nil
	case lower == "/tasks" || lower == "tasks" || strings.Contains(lower, "任务列表"):
		tasks, err := d.Control.ListTasks(ctx, identity.TenantID, identity.PersonID, 20)
		if err != nil {
			return true, "", err
		}
		return true, formatTasks(tasks), nil
	case strings.HasPrefix(lower, "/workspace "):
		parts := strings.Fields(req.Content)
		if len(parts) < 2 {
			return true, "Usage: /workspace <workspace_id>", nil
		}
		ws, err := d.Control.GetWorkspace(ctx, identity.TenantID, parts[1])
		if err != nil {
			return true, "", err
		}
		if ws == nil || ws.OwnerPersonID != identity.PersonID {
			return true, "Workspace not found.", nil
		}
		if err := d.Control.SetCurrentWorkspace(ctx, identity.TenantID, identity.PersonID, ws.ID); err != nil {
			return true, "", err
		}
		return true, fmt.Sprintf("Current workspace: %s (%s)\n%s", ws.Name, ws.ID, ws.LocalPath), nil
	case lower == "/workspaces":
		workspaces, err := d.Control.ListWorkspaces(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			return true, "", err
		}
		return true, formatWorkspaces(workspaces), nil
	case lower == "/approvals":
		approvals, err := d.Control.ListApprovalRequests(ctx, identity.TenantID, identity.PersonID, "pending", 20)
		if err != nil {
			return true, "", err
		}
		return true, formatApprovals(approvals), nil
	case strings.HasPrefix(lower, "/approve "):
		parts := strings.Fields(trimmed)
		if len(parts) < 2 {
			return true, "Usage: /approve <approval_id>", nil
		}
		approval, err := d.Control.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, parts[1], "approved", req.Channel)
		if err != nil {
			return true, "", err
		}
		appendApprovalEvent(ctx, d.Control, approval, req.Channel)
		return true, fmt.Sprintf("Approved %s.", approval.ID), nil
	case strings.HasPrefix(lower, "/reject "):
		parts := strings.Fields(trimmed)
		if len(parts) < 2 {
			return true, "Usage: /reject <approval_id>", nil
		}
		approval, err := d.Control.RespondApprovalRequest(ctx, identity.TenantID, identity.PersonID, parts[1], "rejected", req.Channel)
		if err != nil {
			return true, "", err
		}
		appendApprovalEvent(ctx, d.Control, approval, req.Channel)
		return true, fmt.Sprintf("Rejected %s.", approval.ID), nil
	case lower == "/events":
		task, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			return true, "", err
		}
		if task == nil {
			return true, "No active task.", nil
		}
		events, err := d.Control.ListTaskEvents(ctx, task.ID, 20)
		if err != nil {
			return true, "", err
		}
		return true, formatEvents(events), nil
	case lower == "/task status" || lower == "task status" || lower == "/status" || lower == "status" || strings.Contains(lower, "进度"):
		task, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			return true, "", err
		}
		if task == nil {
			return true, "No active task.", nil
		}
		handoff, _ := d.Control.LatestHandoff(ctx, task.ID)
		plan := d.latestPlanForTask(ctx, task.ID)
		return true, formatTaskStatus(task, handoff, d.currentActive(identity.PersonID), plan), nil
	default:
		return false, "", nil
	}
}

func (d *Server) identityFromQuery(r *http.Request) (*control.IdentityContext, error) {
	q := r.URL.Query()
	return d.Control.ResolveOrCreateAccount(
		r.Context(),
		d.tenantID(q.Get("tenant_id")),
		fallback(q.Get("platform"), "cli"),
		fallback(q.Get("platform_user_id"), "local"),
		q.Get("display_name"),
	)
}

func (d *Server) tenantID(tenantID string) string {
	return fallback(tenantID, fallback(d.DefaultTenantID, control.DefaultTenantID))
}

func (d *Server) authorized(r *http.Request) bool {
	token := gatewayToken()
	if token == "" {
		return true
	}
	if got := strings.TrimSpace(r.Header.Get("X-SelfMind-Token")); got == token {
		return true
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) >= len("Bearer ") && strings.EqualFold(auth[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(auth[len("Bearer "):]) == token
	}
	return false
}

func gatewayToken() string {
	if token := strings.TrimSpace(os.Getenv("SELF_GATEWAY_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("SELF_DAEMON_TOKEN"))
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func mustJSON(value interface{}) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func fallback(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return textutil.TruncateBytes(value, max) + "..."
}

func titleFromInput(input string) string {
	title := strings.TrimSpace(input)
	if title == "" {
		return "Untitled task"
	}
	return truncate(title, 80)
}

func inferTaskStatus(content string) string {
	if looksTaskBlocked(content) {
		return "blocked"
	}
	if looksTaskComplete(content) {
		return "done"
	}
	return "running"

}

func looksTaskBlocked(content string) bool {
	lower := strings.ToLower(strings.TrimSpace(content))
	return containsAny(lower, []string{
		"blocked",
		"cannot proceed",
		"can't proceed",
		"need your input",
		"needs your input",
		"waiting for you",
		"requires approval",
	}) || containsAny(content, []string{
		"\u963b\u585e",
		"\u65e0\u6cd5\u7ee7\u7eed",
		"\u9700\u8981\u4f60\u63d0\u4f9b",
		"\u9700\u8981\u4f60\u786e\u8ba4",
	})
}

func looksTaskComplete(content string) bool {
	trimmed := strings.TrimSpace(content)
	lower := strings.ToLower(trimmed)
	if lower == "" {
		return false
	}
	if containsAny(lower, []string{
		"not done",
		"not completed",
		"not finished",
		"remaining work",
		"still need",
		"need to continue",
		"next steps",
		"todo:",
	}) || containsAny(trimmed, []string{
		"\u672a\u5b8c\u6210",
		"\u8fd8\u6ca1\u5b8c\u6210",
		"\u6ca1\u6709\u5b8c\u6210",
		"\u5f85\u5b8c\u6210",
		"\u9700\u8981\u7ee7\u7eed",
	}) {
		return false
	}
	if lower == "done" || lower == "completed" || lower == "finished" || lower == "all done" {
		return true
	}
	return containsAny(lower, []string{
		"task complete",
		"task completed",
		"completed successfully",
		"finished successfully",
		"all done",
		"implementation complete",
		"tests pass",
	}) || containsAny(trimmed, []string{
		"\u5df2\u5b8c\u6210",
		"\u4efb\u52a1\u5b8c\u6210",
		"\u5904\u7406\u5b8c\u6210",
		"\u5df2\u5904\u7406\u5b8c",
		"\u5df2\u7ecf\u5b8c\u6210",
		"\u641e\u5b9a",
	})
}

func containsAny(value string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func formatTasks(tasks []control.Task) string {
	if len(tasks) == 0 {
		return "No tasks."
	}
	var sb strings.Builder
	for i, task := range tasks {
		fmt.Fprintf(&sb, "%d. [%s] %s (%s)\n", i+1, task.Status, task.Title, task.ID)
	}
	return strings.TrimSpace(sb.String())
}

func formatWorkspaces(workspaces []control.Workspace) string {
	if len(workspaces) == 0 {
		return "No workspaces."
	}
	var sb strings.Builder
	for i, ws := range workspaces {
		fmt.Fprintf(&sb, "%d. %s (%s)\n   %s\n", i+1, ws.Name, ws.ID, ws.LocalPath)
	}
	return strings.TrimSpace(sb.String())
}

func formatApprovals(approvals []control.ApprovalRequest) string {
	if len(approvals) == 0 {
		return "No pending approvals."
	}
	var sb strings.Builder
	sb.WriteString("Pending approvals:\n")
	for i, approval := range approvals {
		fmt.Fprintf(&sb, "%d. %s [%s]", i+1, approval.ID, approval.ActionType)
		if approval.TaskID != "" {
			fmt.Fprintf(&sb, " task=%s", approval.TaskID)
		}
		if len(approval.Payload) > 0 && string(approval.Payload) != "{}" {
			fmt.Fprintf(&sb, "\n   %s", truncate(toOneLine(string(approval.Payload)), 180))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\nUse /approve <id> or /reject <id>.")
	return strings.TrimSpace(sb.String())
}

func formatEvents(events []control.Event) string {
	if len(events) == 0 {
		return "No recent task events."
	}
	var sb strings.Builder
	sb.WriteString("Recent events:\n")
	for i, event := range events {
		fmt.Fprintf(&sb, "%d. %s", i+1, event.Type)
		if event.Channel != "" {
			fmt.Fprintf(&sb, " [%s]", event.Channel)
		}
		if len(event.Payload) > 0 && string(event.Payload) != "{}" {
			fmt.Fprintf(&sb, " %s", truncate(toOneLine(string(event.Payload)), 160))
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

func toOneLine(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	for strings.Contains(value, "  ") {
		value = strings.ReplaceAll(value, "  ", " ")
	}
	return strings.TrimSpace(value)
}

func formatIdentity(identity *control.IdentityContext) string {
	if identity == nil {
		return "No identity."
	}
	return fmt.Sprintf("tenant_id: %s\nperson_id: %s\naccount_id: %s\nplatform: %s\nplatform_user_id: %s",
		identity.TenantID, identity.PersonID, identity.AccountID, identity.Platform, identity.PlatformUserID)
}

func formatBusyRun(active *activeRun) string {
	if active == nil {
		return ""
	}
	elapsed := time.Since(active.StartedAt).Round(time.Second)
	runID := fallback(active.RunID, "(starting)")
	taskID := fallback(active.TaskID, "(starting)")
	return fmt.Sprintf("Task is running\n- task: %s\n- run: %s\n- elapsed: %s\n\nUse /status for details or /stop to cancel.", taskID, runID, elapsed)
}

func formatActiveRunStatus(active *activeRun) *api.ActiveRunStatus {
	if active == nil {
		return nil
	}
	return &api.ActiveRunStatus{
		TenantID:       active.TenantID,
		PersonID:       active.PersonID,
		TaskID:         active.TaskID,
		RunID:          active.RunID,
		Channel:        active.Channel,
		Summary:        active.Summary,
		StartedAt:      active.StartedAt.Format(time.RFC3339),
		ElapsedSeconds: int64(time.Since(active.StartedAt).Seconds()),
	}
}

func formatTaskStatus(task *control.Task, handoff *control.Handoff, active *activeRun, plan []taskPlanStep) string {
	if task == nil {
		return "No active task."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Task: %s\nStatus: %s\n", task.Title, task.Status)
	if task.WorkspaceID != "" {
		fmt.Fprintf(&sb, "Workspace: %s\n", task.WorkspaceID)
	}
	if active != nil {
		fmt.Fprintf(&sb, "\nRunning:\n- run: %s\n- elapsed: %s\n- channel: %s\n", fallback(active.RunID, "(starting)"), time.Since(active.StartedAt).Round(time.Second), active.Channel)
	}
	if task.CurrentSummary != "" {
		fmt.Fprintf(&sb, "\nSummary: %s\n", task.CurrentSummary)
	}
	if len(plan) > 0 {
		sb.WriteString("\nPlan:\n")
		for _, step := range plan {
			marker := "[ ]"
			switch step.Status {
			case "completed":
				marker = "[x]"
			case "in_progress":
				marker = "[>]"
			case "cancelled":
				marker = "[-]"
			}
			fmt.Fprintf(&sb, "- %s %s\n", marker, step.Step)
		}
	}
	if handoff != nil {
		if len(handoff.DoneItems) > 0 {
			sb.WriteString("\nDone:\n")
			for _, item := range handoff.DoneItems {
				fmt.Fprintf(&sb, "- %s\n", item)
			}
		}
		if handoff.TestStatus != "" {
			fmt.Fprintf(&sb, "\nTests:\n%s\n", handoff.TestStatus)
		}
		if len(handoff.ChangedFiles) > 0 {
			sb.WriteString("\nFiles:\n")
			for _, file := range handoff.ChangedFiles {
				fmt.Fprintf(&sb, "- %s\n", file)
			}
		}
	}
	nextSteps := task.NextSteps
	if len(nextSteps) == 0 && handoff != nil {
		nextSteps = handoff.NextSteps
	}
	if len(nextSteps) > 0 {
		sb.WriteString("\nNext:\n")
		for _, step := range nextSteps {
			fmt.Fprintf(&sb, "- %s\n", step)
		}
	}
	if handoff != nil && len(handoff.Risks) > 0 {
		sb.WriteString("\nRisks:\n")
		for _, risk := range handoff.Risks {
			fmt.Fprintf(&sb, "- %s\n", risk)
		}
	}
	return strings.TrimSpace(sb.String())
}
