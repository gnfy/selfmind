package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel/llm"
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
	agentMu      sync.Mutex
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
	mux.HandleFunc("/v1/tasks/events", d.handleTaskEvents)
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
		return api.MessageResponse{Error: "content is required"}, http.StatusBadRequest
	}

	identity, err := d.Control.ResolveOrCreateAccount(ctx, req.TenantID, req.Platform, req.PlatformUserID, req.DisplayName)
	if err != nil {
		return api.MessageResponse{Error: err.Error()}, http.StatusInternalServerError
	}

	if handled, content, err := d.tryHandleControlCommand(ctx, identity, req); handled {
		if err != nil {
			return api.MessageResponse{Identity: identity, Error: err.Error()}, http.StatusInternalServerError
		}
		return api.MessageResponse{Identity: identity, Content: content}, http.StatusOK
	}

	if d.IsDraining() {
		return api.MessageResponse{
			Identity: identity,
			Error:    "gateway is shutting down; try again after restart",
			Accepted: false,
		}, http.StatusServiceUnavailable
	}

	if running := d.currentActive(identity.PersonID); running != nil {
		return api.MessageResponse{
			Identity: identity,
			Content:  formatBusyRun(running),
			Accepted: false,
		}, http.StatusOK
	}

	if req.Async {
		return d.startAsyncRun(identity, req), http.StatusOK
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
		return api.MessageResponse{Identity: identity, Content: "Another task is already running. Use /status or /stop."}, http.StatusOK
	}
	defer d.endActive(identity.PersonID)

	return d.runMessage(runCtx, identity, req)
}

func (d *Server) runMessage(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (api.MessageResponse, int) {
	task, err := d.resolveTask(ctx, identity, req)
	if err != nil {
		return api.MessageResponse{Identity: identity, Error: err.Error()}, http.StatusInternalServerError
	}
	_ = d.Control.RecordChannelMessage(ctx, *identity, req.Channel, task.ID, "user", req.Content)

	run, err := d.Control.StartRun(ctx, task, req.Channel, truncate(req.Content, 240))
	if err != nil {
		return api.MessageResponse{Identity: identity, Task: task, Error: err.Error()}, http.StatusInternalServerError
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
	agentInput := d.withGatewayContext(req.Content, identity, task, workspace)

	if d.Gateway == nil {
		err := fmt.Errorf("gateway is not configured")
		_ = d.Control.FinishRun(context.Background(), identity.TenantID, run.ID, "failed")
		_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, "blocked", err.Error(), nil)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error()}, http.StatusInternalServerError
	}

	d.agentMu.Lock()
	resp, err := d.Gateway.HandleWithEvents(ctx, identity.PersonID, req.Channel, agentInput)
	if err != nil {
		d.agentMu.Unlock()
		status := "failed"
		taskStatus := "blocked"
		if ctx.Err() != nil {
			status = "cancelled"
			taskStatus = "cancelled"
		}
		_ = d.Control.FinishRun(context.Background(), identity.TenantID, run.ID, status)
		_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, taskStatus, err.Error(), nil)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error()}, http.StatusOK
	}

	content, usage, err := d.aggregateGatewayResponse(ctx, req.Channel, task, run, resp)
	d.agentMu.Unlock()
	if err != nil {
		status := "failed"
		taskStatus := "blocked"
		if ctx.Err() != nil {
			status = "cancelled"
			taskStatus = "cancelled"
		}
		_ = d.Control.FinishRun(context.Background(), identity.TenantID, run.ID, status)
		_ = d.Control.UpdateTaskStatus(context.Background(), identity.TenantID, task.ID, taskStatus, err.Error(), nil)
		return api.MessageResponse{Identity: identity, Task: task, Run: run, Error: err.Error()}, http.StatusOK
	}

	_ = d.Control.RecordChannelMessage(ctx, *identity, req.Channel, task.ID, "assistant", content)
	_ = d.Control.FinishRun(ctx, identity.TenantID, run.ID, "done")
	_ = d.Control.UpdateTaskStatus(ctx, identity.TenantID, task.ID, inferTaskStatus(content), truncate(content, 1000), nil)
	_, _ = d.Control.SaveHandoff(ctx, control.Handoff{
		TaskID:  task.ID,
		Summary: truncate(content, 1000),
	})
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      run.ID,
		Type:       "run.finished",
		Visibility: "task",
		Channel:    req.Channel,
		Payload:    mustJSON(map[string]interface{}{"summary": truncate(content, 500), "usage": usage}),
	})
	task, _ = d.Control.GetTask(ctx, identity.TenantID, task.ID)
	return api.MessageResponse{Identity: identity, Task: task, Run: run, Content: content, Usage: usage}, http.StatusOK
}

func (d *Server) startAsyncRun(identity *control.IdentityContext, req api.MessageRequest) api.MessageResponse {
	active := &activeRun{
		TenantID:  identity.TenantID,
		PersonID:  identity.PersonID,
		Channel:   req.Channel,
		Summary:   truncate(req.Content, 240),
		StartedAt: time.Now(),
	}
	if ok := d.beginActive(identity.PersonID, active); !ok {
		return api.MessageResponse{Identity: identity, Content: "Another task is already running. Use /status or /stop."}
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	active.Cancel = runCancel
	go func() {
		defer d.endActive(identity.PersonID)
		defer runCancel()
		resp, _ := d.runMessage(runCtx, identity, req)
		d.deliverAsyncResult(context.Background(), identity, req, resp)
	}()

	return api.MessageResponse{
		Identity: identity,
		Content:  "Started in the background. Use /status to check progress or /stop to cancel.",
		Accepted: true,
	}
}

func (d *Server) beginActive(personID string, run *activeRun) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == nil {
		d.active = make(map[string]*activeRun)
	}
	if _, exists := d.active[personID]; exists {
		return false
	}
	d.active[personID] = run
	return true
}

func (d *Server) updateActive(personID string, task *control.Task, run *control.Run) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == nil {
		return
	}
	active := d.active[personID]
	if active == nil {
		return
	}
	if task != nil {
		active.TaskID = task.ID
	}
	if run != nil {
		active.RunID = run.ID
		active.StartedAt = run.StartedAt
	}
}

func (d *Server) currentActive(personID string) *activeRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == nil {
		return nil
	}
	active := d.active[personID]
	if active == nil {
		return nil
	}
	copy := *active
	return &copy
}

func (d *Server) stopActive(personID string) *activeRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == nil {
		return nil
	}
	active := d.active[personID]
	if active == nil {
		return nil
	}
	if active.Cancel != nil {
		active.Cancel()
	}
	copy := *active
	return &copy
}

func (d *Server) endActive(personID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active != nil {
		delete(d.active, personID)
	}
}

func (d *Server) handleIMWebhook(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	platform := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/im/"), "/")
	if platform == "" {
		platform = "webhook"
	}

	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if challenge := mapString(payload, "challenge"); challenge != "" {
		writeJSON(w, http.StatusOK, map[string]string{"challenge": challenge})
		return
	}

	req := messageRequestFromIM(platform, payload)
	if boolFromMap(payload, "async") || (os.Getenv("SELF_IM_ASYNC") == "1" && !isControlCommand(req.Content)) {
		req.Async = true
	}
	resp, status := d.ProcessMessage(r.Context(), req)
	writeJSON(w, status, resp)
}

func (d *Server) handleAccountBind(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req api.BindAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.PersonID) == "" {
		http.Error(w, "person_id is required", http.StatusBadRequest)
		return
	}
	identity, err := d.Control.BindAccount(
		r.Context(),
		d.tenantID(req.TenantID),
		req.PersonID,
		req.Platform,
		req.PlatformUserID,
		req.DisplayName,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"identity": identity})
}

func (d *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, err := d.identityFromQuery(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	tasks, err := d.Control.ListTasks(r.Context(), identity.TenantID, identity.PersonID, 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"identity": identity, "tasks": tasks})
}

func (d *Server) handleTaskEvents(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, err := d.identityFromQuery(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	if taskID == "" {
		task, err := d.Control.CurrentTask(r.Context(), identity.TenantID, identity.PersonID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if task != nil {
			taskID = task.ID
		}
	}
	if taskID == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"identity": identity, "events": []control.Event{}})
		return
	}
	task, err := d.Control.GetTask(r.Context(), identity.TenantID, taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if task == nil || task.PersonID != identity.PersonID {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	events, err := d.Control.ListTaskEvents(r.Context(), taskID, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"identity": identity, "task": task, "events": events})
}

func (d *Server) handleCurrentTask(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, err := d.identityFromQuery(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	task, err := d.Control.CurrentTask(r.Context(), identity.TenantID, identity.PersonID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var handoff *control.Handoff
	if task != nil {
		handoff, _ = d.Control.LatestHandoff(r.Context(), task.ID)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"identity":   identity,
		"task":       task,
		"handoff":    handoff,
		"active_run": formatActiveRunStatus(d.currentActive(identity.PersonID)),
	})
}

func (d *Server) handleWorkspaceRegister(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req api.WorkspaceRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	identity, err := d.Control.ResolveOrCreateAccount(r.Context(), d.tenantID(req.TenantID), fallback(req.Platform, "cli"), fallback(req.PlatformUserID, "local"), req.DisplayName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	ws, err := d.Control.RegisterWorkspace(r.Context(), control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          req.Name,
		RepoURL:       req.RepoURL,
		LocalPath:     req.LocalPath,
		DefaultBranch: req.DefaultBranch,
		AllowedRoots:  req.AllowedRoots,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"identity": identity, "workspace": ws})
}

func (d *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, err := d.identityFromQuery(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	workspaces, err := d.Control.ListWorkspaces(r.Context(), identity.TenantID, identity.PersonID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"identity": identity, "workspaces": workspaces})
}

func (d *Server) handleGatewayStatus(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, d.GatewayStatus())
}

func (d *Server) handleGatewayShutdown(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	started := d.RequestGatewayShutdown(d.drainTimeout(), "api shutdown")
	status := http.StatusAccepted
	if !started {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]interface{}{
		"accepted": started,
		"status":   d.GatewayStatus(),
	})
}

func (d *Server) GatewayStatus() api.GatewayStatusResponse {
	runtime := api.GatewayRuntimeInfo{State: d.GatewayState()}
	if d.RuntimeStatusFunc != nil {
		runtime = d.RuntimeStatusFunc()
	}
	active := d.activeRunStatuses()
	state, draining, reason := d.gatewayStateParts()
	if runtime.State == "" {
		runtime.State = state
	}
	return api.GatewayStatusResponse{
		Runtime:        runtime,
		State:          state,
		Draining:       draining,
		DrainReason:    reason,
		ActiveRuns:     active,
		ActiveRunCount: len(active),
	}
}

func (d *Server) GatewayState() string {
	state, _, _ := d.gatewayStateParts()
	return state
}

func (d *Server) gatewayStateParts() (string, bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.draining {
		return "draining", true, d.drainReason
	}
	return "running", false, ""
}

func (d *Server) IsDraining() bool {
	_, draining, _ := d.gatewayStateParts()
	return draining
}

func (d *Server) RequestGatewayShutdown(timeout time.Duration, reason string) bool {
	started := false
	d.shutdownOnce.Do(func() {
		started = true
		go d.shutdownAfterDrain(timeout, reason)
	})
	return started
}

func (d *Server) shutdownAfterDrain(timeout time.Duration, reason string) {
	if timeout <= 0 {
		timeout = d.drainTimeout()
	}
	d.beginDraining(reason)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if !d.waitForIdle(ctx) {
		d.stopAllActive("gateway shutdown")
	}
	if d.ShutdownFunc != nil {
		d.ShutdownFunc()
	}
}

func (d *Server) beginDraining(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.draining = true
	d.drainReason = reason
}

func (d *Server) waitForIdle(ctx context.Context) bool {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if len(d.activeRunStatuses()) == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return len(d.activeRunStatuses()) == 0
		case <-ticker.C:
		}
	}
}

func (d *Server) stopAllActive(reason string) {
	d.mu.Lock()
	var runs []*activeRun
	for _, active := range d.active {
		copy := *active
		runs = append(runs, &copy)
		if active.Cancel != nil {
			active.Cancel()
		}
	}
	d.mu.Unlock()

	for _, active := range runs {
		if active.RunID != "" && d.Control != nil {
			_ = d.Control.RequestRunCancel(context.Background(), active.TenantID, active.RunID)
			_ = d.Control.FinishRun(context.Background(), active.TenantID, active.RunID, "cancelled")
		}
		if active.TaskID != "" && d.Control != nil {
			_ = d.Control.UpdateTaskStatus(context.Background(), active.TenantID, active.TaskID, "cancelled", reason, nil)
			_, _ = d.Control.AppendEvent(context.Background(), control.Event{
				TaskID:     active.TaskID,
				RunID:      active.RunID,
				Type:       "run.cancelled",
				Visibility: "task",
				Channel:    active.Channel,
				Payload:    mustJSON(map[string]string{"reason": reason}),
			})
		}
	}
}

func (d *Server) activeRunStatuses() []api.ActiveRunStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.active) == 0 {
		return nil
	}
	statuses := make([]api.ActiveRunStatus, 0, len(d.active))
	for _, active := range d.active {
		if active == nil {
			continue
		}
		status := formatActiveRunStatus(activeRunCopy(active))
		if status != nil {
			statuses = append(statuses, *status)
		}
	}
	return statuses
}

func activeRunCopy(active *activeRun) *activeRun {
	if active == nil {
		return nil
	}
	copy := *active
	return &copy
}

func (d *Server) drainTimeout() time.Duration {
	if d.DrainTimeout > 0 {
		return d.DrainTimeout
	}
	return 30 * time.Second
}

func (d *Server) resolveTask(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (*control.Task, error) {
	if req.TaskID != "" {
		task, err := d.Control.GetTask(ctx, identity.TenantID, req.TaskID)
		if err != nil || task != nil {
			return task, err
		}
	}
	task, err := d.Control.CurrentTask(ctx, identity.TenantID, identity.PersonID)
	if err != nil {
		return nil, err
	}
	if task != nil {
		return task, nil
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
	if identity == nil || workspace == nil || workspace.LocalPath == "" {
		return func() {}
	}
	scope := tools.ExecutionScope{
		TenantID:      identity.TenantID,
		PersonID:      identity.PersonID,
		WorkspaceID:   workspace.ID,
		WorkspaceRoot: workspace.LocalPath,
		AllowedRoots:  workspace.AllowedRoots,
	}
	if task != nil {
		scope.TaskID = task.ID
	}
	if run != nil {
		scope.RunID = run.ID
	}
	return tools.SetExecutionScope(identity.PersonID, scope)
}

func (d *Server) withGatewayContext(input string, identity *control.IdentityContext, task *control.Task, workspace *control.Workspace) string {
	if workspace == nil || workspace.LocalPath == "" || task == nil {
		return input
	}
	var sb strings.Builder
	sb.WriteString("[SelfMind daemon context]\n")
	fmt.Fprintf(&sb, "person_id: %s\n", identity.PersonID)
	fmt.Fprintf(&sb, "channel: %s\n", identity.Platform)
	fmt.Fprintf(&sb, "task_id: %s\n", task.ID)
	fmt.Fprintf(&sb, "workspace_id: %s\n", workspace.ID)
	fmt.Fprintf(&sb, "workspace_root: %s\n", workspace.LocalPath)
	sb.WriteString("Use workspace_root as the default cwd. Do not access files outside workspace allowed roots.\n")
	sb.WriteString("[/SelfMind daemon context]\n\n")
	sb.WriteString(input)
	return sb.String()
}

func (d *Server) tryHandleControlCommand(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest) (bool, string, error) {
	lower := strings.ToLower(strings.TrimSpace(req.Content))
	switch {
	case lower == "/id" || lower == "id":
		return true, formatIdentity(identity), nil
	case lower == "/stop" || lower == "stop":
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
		title := strings.TrimSpace(req.Content[len("/new"):])
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
	case strings.HasPrefix(lower, "/resume ") || strings.HasPrefix(lower, "/task "):
		parts := strings.Fields(req.Content)
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
	case lower == "/workspaces" || lower == "workspaces":
		workspaces, err := d.Control.ListWorkspaces(ctx, identity.TenantID, identity.PersonID)
		if err != nil {
			return true, "", err
		}
		return true, formatWorkspaces(workspaces), nil
	case lower == "/events" || lower == "events":
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
		return true, formatTaskStatus(task, handoff, d.currentActive(identity.PersonID)), nil
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

func messageRequestFromIM(platform string, payload map[string]interface{}) api.MessageRequest {
	req := api.MessageRequest{
		TenantID:       mapString(payload, "tenant_id"),
		Platform:       fallback(mapString(payload, "platform"), platform),
		PlatformUserID: firstNonEmpty(mapString(payload, "platform_user_id"), mapString(payload, "user_id"), mapString(payload, "open_id"), mapString(payload, "sender_id"), "local"),
		DisplayName:    firstNonEmpty(mapString(payload, "display_name"), mapString(payload, "user_name"), mapString(payload, "name")),
		Channel:        firstNonEmpty(mapString(payload, "channel"), mapString(payload, "chat_id"), mapString(payload, "conversation_id"), platform),
		Content:        firstNonEmpty(mapString(payload, "content"), mapString(payload, "text"), mapString(payload, "message")),
		WorkspaceID:    mapString(payload, "workspace_id"),
		TaskID:         mapString(payload, "task_id"),
		Async:          boolFromMap(payload, "async"),
	}

	if event := nestedMap(payload, "event"); event != nil {
		if sender := nestedMap(event, "sender"); sender != nil {
			if senderID := nestedMap(sender, "sender_id"); senderID != nil {
				req.PlatformUserID = firstNonEmpty(
					mapString(senderID, "union_id"),
					mapString(senderID, "user_id"),
					mapString(senderID, "open_id"),
					req.PlatformUserID,
				)
			}
			req.DisplayName = firstNonEmpty(mapString(sender, "sender_type"), req.DisplayName)
		}
		if msg := nestedMap(event, "message"); msg != nil {
			req.Channel = firstNonEmpty(mapString(msg, "chat_id"), mapString(msg, "root_id"), req.Channel)
			req.Content = firstNonEmpty(contentText(msg["content"]), mapString(msg, "text"), req.Content)
		}
	}

	if msg := nestedMap(payload, "message"); msg != nil {
		req.Content = firstNonEmpty(contentText(msg["content"]), mapString(msg, "content"), mapString(msg, "text"), req.Content)
		req.Channel = firstNonEmpty(mapString(msg, "chat_id"), mapString(msg, "group_id"), mapString(msg, "channel_id"), req.Channel)
	}
	if author := nestedMap(payload, "author"); author != nil {
		req.PlatformUserID = firstNonEmpty(mapString(author, "id"), mapString(author, "user_id"), req.PlatformUserID)
		req.DisplayName = firstNonEmpty(mapString(author, "username"), mapString(author, "name"), req.DisplayName)
	}

	// Common WeChat/WeCom XML-to-JSON field names used by many webhook relays.
	req.PlatformUserID = firstNonEmpty(mapString(payload, "FromUserName"), req.PlatformUserID)
	req.Channel = firstNonEmpty(mapString(payload, "ToUserName"), req.Channel)
	req.Content = firstNonEmpty(mapString(payload, "Content"), req.Content)
	return req
}

func contentText(value interface{}) string {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return ""
		}
		var decoded map[string]interface{}
		if strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(trimmed), &decoded) == nil {
			return firstNonEmpty(mapString(decoded, "text"), mapString(decoded, "content"))
		}
		return trimmed
	case map[string]interface{}:
		return firstNonEmpty(mapString(v, "text"), mapString(v, "content"))
	default:
		return ""
	}
}

func nestedMap(payload map[string]interface{}, keys ...string) map[string]interface{} {
	current := payload
	for _, key := range keys {
		next, ok := current[key].(map[string]interface{})
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func mapString(payload map[string]interface{}, key string) string {
	if payload == nil {
		return ""
	}
	value, ok := payload[key]
	if !ok || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func boolFromMap(payload map[string]interface{}, key string) bool {
	if payload == nil {
		return false
	}
	value, ok := payload[key]
	if !ok || value == nil {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
		}
	default:
		return strings.EqualFold(strings.TrimSpace(fmt.Sprint(v)), "true")
	}
}

func isControlCommand(content string) bool {
	lower := strings.ToLower(strings.TrimSpace(content))
	if lower == "" {
		return false
	}
	switch {
	case lower == "/id" || lower == "id":
		return true
	case lower == "/stop" || lower == "stop":
		return true
	case lower == "/tasks" || lower == "tasks":
		return true
	case lower == "/workspaces" || lower == "workspaces":
		return true
	case lower == "/events" || lower == "events":
		return true
	case lower == "/status" || lower == "status" || lower == "/task status" || lower == "task status":
		return true
	case strings.HasPrefix(lower, "/new"):
		return true
	case strings.HasPrefix(lower, "/resume ") || strings.HasPrefix(lower, "/task ") || strings.HasPrefix(lower, "/workspace "):
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (d *Server) startRunHeartbeat(ctx context.Context, run *control.Run) func() {
	if d == nil || d.Control == nil || run == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = d.Control.UpdateRunHeartbeat(context.Background(), run.TenantID, run.ID)
			}
		}
	}()
	return func() {
		close(done)
		_ = d.Control.UpdateRunHeartbeat(context.Background(), run.TenantID, run.ID)
	}
}

func (d *Server) deliverAsyncResult(ctx context.Context, identity *control.IdentityContext, req api.MessageRequest, resp api.MessageResponse) {
	if d == nil || d.Delivery == nil || identity == nil {
		return
	}
	if req.Platform == "cli" && req.Channel == "cli" {
		return
	}
	content := strings.TrimSpace(resp.Content)
	if resp.Error != "" {
		content = "SelfMind task failed: " + tools.RedactSensitive(resp.Error)
	}
	if content == "" {
		content = "SelfMind task finished."
	}
	_ = d.Delivery.EnqueueAndTry(ctx, delivery.Message{
		TenantID:       identity.TenantID,
		PersonID:       identity.PersonID,
		Platform:       req.Platform,
		PlatformUserID: identity.PlatformUserID,
		Channel:        req.Channel,
		TaskID:         taskIDForResponse(resp),
		RunID:          runIDForResponse(resp),
		Content:        content,
	})
}

func taskIDForResponse(resp api.MessageResponse) string {
	if resp.Task == nil {
		return ""
	}
	return resp.Task.ID
}

func runIDForResponse(resp api.MessageResponse) string {
	if resp.Run == nil {
		return ""
	}
	return resp.Run.ID
}

func (d *Server) aggregateGatewayResponse(ctx context.Context, channel string, task *control.Task, run *control.Run, resp *router.HandleResponse) (string, llm.UsageStats, error) {
	if resp == nil {
		return "", llm.UsageStats{}, nil
	}
	if !resp.IsStreaming {
		return resp.Content, resp.Usage, nil
	}
	var content strings.Builder
	var usage llm.UsageStats
	sawStream := false
	for event := range resp.Stream {
		if event.EventType != "" {
			d.recordStreamEvent(ctx, channel, task, run, event)
			if event.EventType == "stream" {
				sawStream = true
				content.WriteString(event.Content)
			}
			if event.Usage != nil {
				usage = *event.Usage
			}
			continue
		}
		if event.Err != nil {
			return content.String(), usage, event.Err
		}
		if event.Content != "" && !sawStream {
			content.WriteString(event.Content)
		}
		if event.Usage != nil {
			usage = *event.Usage
		}
	}
	return content.String(), usage, nil
}

func (d *Server) recordStreamEvent(ctx context.Context, channel string, task *control.Task, run *control.Run, event llm.StreamEvent) {
	if d == nil || d.Control == nil || task == nil {
		return
	}
	eventType := event.EventType
	if eventType == "stream" || eventType == "" {
		return
	}
	payload := map[string]interface{}{}
	switch eventType {
	case "tool.started":
		payload["tool"] = event.ToolName
		payload["args"] = tools.RedactSensitive(event.ToolArgs)
	case "tool.completed":
		payload["tool"] = event.ToolName
		payload["result"] = tools.RedactSensitive(truncate(event.ToolResult, 1000))
		payload["duration_seconds"] = event.DurationSeconds
		if event.Err != nil {
			payload["error"] = tools.RedactSensitive(event.Err.Error())
		}
	case "learning.review":
		payload["message"] = tools.RedactSensitive(event.Content)
	default:
		payload["message"] = tools.RedactSensitive(event.Content)
	}
	runID := ""
	if run != nil {
		runID = run.ID
	}
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     task.ID,
		RunID:      runID,
		Type:       eventType,
		Visibility: "task",
		Channel:    channel,
		Payload:    mustJSON(payload),
	})
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
	return value[:max] + "..."
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
	return fmt.Sprintf("A task is already running.\nRun: %s\nTask: %s\nElapsed: %s\nUse /status to check progress or /stop to cancel.", runID, taskID, elapsed)
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

func formatTaskStatus(task *control.Task, handoff *control.Handoff, active *activeRun) string {
	if task == nil {
		return "No active task."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\nStatus: %s\n", task.Title, task.Status)
	if active != nil {
		fmt.Fprintf(&sb, "\nActive run: %s\nElapsed: %s\n", fallback(active.RunID, "(starting)"), time.Since(active.StartedAt).Round(time.Second))
	}
	if task.CurrentSummary != "" {
		fmt.Fprintf(&sb, "\nSummary: %s\n", task.CurrentSummary)
	}
	if len(task.NextSteps) > 0 {
		sb.WriteString("\nNext steps:\n")
		for _, step := range task.NextSteps {
			fmt.Fprintf(&sb, "- %s\n", step)
		}
	}
	if handoff != nil && handoff.Summary != "" {
		fmt.Fprintf(&sb, "\nLast handoff: %s\n", handoff.Summary)
	}
	return strings.TrimSpace(sb.String())
}
