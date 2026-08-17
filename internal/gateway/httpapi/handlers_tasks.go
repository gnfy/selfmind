package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

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
	limit := queryInt(r, "limit", 50)
	offset := queryInt(r, "offset", 0)
	view := strings.TrimSpace(r.URL.Query().Get("view"))
	if view == "" {
		view = "all"
	}
	page, err := d.Control.QueryTasks(r.Context(), identity.TenantID, identity.PersonID, control.TaskQuery{
		View:        view,
		Status:      strings.TrimSpace(r.URL.Query().Get("status")),
		WorkspaceID: strings.TrimSpace(r.URL.Query().Get("workspace_id")),
		Keyword:     strings.TrimSpace(r.URL.Query().Get("q")),
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"identity": identity,
		"tasks":    page.Tasks,
		"total":    page.Total,
		"limit":    page.Limit,
		"offset":   page.Offset,
		"has_more": page.HasMore(),
	})
}

func queryInt(r *http.Request, key string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
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
	// An event poll is a liveness beat: the TUI polls this endpoint mid-turn,
	// which is exactly when approval routing needs to know it is attached.
	// Legacy active=0 is ignored: watching proves the endpoint process is live;
	// unanswered-request age, not keyboard activity, owns IM escalation.
	if presenceClaimed(r) {
		d.touchPresence(r.Context(), identity)
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

func (d *Server) handleTaskEventsStream(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	identity, task, err := d.taskFromEventsRequest(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if task == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	seen := map[string]struct{}{}
	sendEvents := func() {
		events, err := d.Control.ListTaskEvents(r.Context(), task.ID, 100)
		if err != nil {
			_, _ = fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
			flusher.Flush()
			return
		}
		for i := len(events) - 1; i >= 0; i-- {
			event := events[i]
			if _, ok := seen[event.ID]; ok {
				continue
			}
			seen[event.ID] = struct{}{}
			writeSSEEvent(w, event)
		}
		flusher.Flush()
	}

	_, _ = fmt.Fprintf(w, "event: ready\ndata: {\"task_id\":%q,\"person_id\":%q}\n\n", task.ID, identity.PersonID)
	sendEvents()
	if strings.EqualFold(r.URL.Query().Get("once"), "true") {
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			sendEvents()
		}
	}
}

func (d *Server) handleTaskArtifacts(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	identity, task, err := d.taskFromEventsRequest(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if task == nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	artifacts, err := d.Control.ListTaskArtifacts(r.Context(), task.ID, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"identity": identity, "task": task, "artifacts": artifacts})
}

func (d *Server) taskFromEventsRequest(r *http.Request) (*control.IdentityContext, *control.Task, error) {
	identity, err := d.identityFromQuery(r)
	if err != nil {
		return nil, nil, err
	}
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	if taskID == "" {
		task, err := d.Control.CurrentTask(r.Context(), identity.TenantID, identity.PersonID)
		if err != nil {
			return identity, nil, err
		}
		return identity, task, nil
	}
	task, err := d.Control.GetTask(r.Context(), identity.TenantID, taskID)
	if err != nil {
		return identity, nil, err
	}
	if task == nil || task.PersonID != identity.PersonID {
		return identity, nil, nil
	}
	return identity, task, nil
}

func writeSSEEvent(w http.ResponseWriter, event control.Event) {
	data, _ := json.Marshal(event)
	_, _ = fmt.Fprintf(w, "id: %s\n", event.ID)
	_, _ = fmt.Fprintf(w, "event: %s\n", event.Type)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
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
	var artifacts []control.Artifact
	if task != nil {
		handoff, _ = d.Control.LatestHandoff(r.Context(), task.ID)
		artifacts, _ = d.Control.ListTaskArtifacts(r.Context(), task.ID, 100)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"identity":   identity,
		"task":       task,
		"handoff":    handoff,
		"artifacts":  artifacts,
		"active_run": formatActiveRunStatus(d.coordinator().currentActive(identity.PersonID)),
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
	workspace := control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          req.Name,
		RepoURL:       req.RepoURL,
		LocalPath:     req.LocalPath,
		DefaultBranch: req.DefaultBranch,
		AllowedRoots:  req.AllowedRoots,
	}
	var ws *control.Workspace
	// Only an authenticated local CLI registration is an explicit trust act.
	// Remote callers may describe a workspace, but cannot elevate repository
	// content into trusted instructions.
	if d.localCLIRequest(r, req.Platform) {
		ws, err = d.Control.RegisterWorkspace(r.Context(), workspace)
	} else {
		ws, err = d.Control.EnsureWorkspace(r.Context(), workspace)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"identity": identity, "workspace": ws})
}

func (d *Server) handleWorkspaceTrust(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req api.WorkspaceTrustRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if !d.localCLIRequest(r, req.Platform) {
		http.Error(w, "workspace trust can only be changed by the authenticated local CLI", http.StatusForbidden)
		return
	}
	identity, err := d.Control.ResolveOrCreateAccount(
		r.Context(),
		d.tenantID(req.TenantID),
		"cli",
		fallback(req.PlatformUserID, "local"),
		"",
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	workspaceID := strings.TrimSpace(req.WorkspaceID)
	if workspaceID == "" {
		current, currentErr := d.Control.CurrentWorkspace(r.Context(), identity.TenantID, identity.PersonID)
		if currentErr != nil {
			writeError(w, http.StatusInternalServerError, currentErr)
			return
		}
		if current == nil {
			http.Error(w, "no current workspace", http.StatusBadRequest)
			return
		}
		workspaceID = current.ID
	}
	ws, err := d.Control.SetWorkspaceTrust(
		r.Context(),
		identity.TenantID,
		identity.PersonID,
		workspaceID,
		req.TrustLevel,
		"local_cli",
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"identity": identity, "workspace": ws})
}

func (d *Server) handleWorkspaceCapabilities(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req api.WorkspaceCapabilityRequest
	if r.Method == http.MethodGet {
		req = api.WorkspaceCapabilityRequest{
			TenantID:       r.URL.Query().Get("tenant_id"),
			Platform:       r.URL.Query().Get("platform"),
			PlatformUserID: r.URL.Query().Get("platform_user_id"),
			WorkspaceID:    r.URL.Query().Get("workspace_id"),
		}
	} else if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if !d.localCLIRequest(r, req.Platform) {
		http.Error(w, "workspace capabilities can only be managed by the authenticated local CLI", http.StatusForbidden)
		return
	}
	identity, err := d.Control.ResolveOrCreateAccount(
		r.Context(),
		d.tenantID(req.TenantID),
		"cli",
		fallback(req.PlatformUserID, "local"),
		"",
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	workspaceID := strings.TrimSpace(req.WorkspaceID)
	if workspaceID == "" {
		current, currentErr := d.Control.CurrentWorkspace(r.Context(), identity.TenantID, identity.PersonID)
		if currentErr != nil {
			writeError(w, http.StatusInternalServerError, currentErr)
			return
		}
		if current == nil {
			http.Error(w, "no current workspace", http.StatusBadRequest)
			return
		}
		workspaceID = current.ID
	}
	if r.Method == http.MethodDelete {
		capability := strings.TrimSpace(req.Capability)
		if capability == "" {
			http.Error(w, "capability is required", http.StatusBadRequest)
			return
		}
		if err := d.Control.RevokeExecutionCapability(r.Context(), identity.TenantID, identity.PersonID, workspaceID, capability); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"revoked": capability, "workspace_id": workspaceID})
		return
	}
	grants, err := d.Control.ListActiveExecutionCapabilities(r.Context(), identity.TenantID, identity.PersonID, workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]api.WorkspaceCapability, 0, len(grants))
	for _, grant := range grants {
		out = append(out, api.WorkspaceCapability{
			Capability: grant.Capability,
			GrantedBy:  grant.GrantedBy,
			ExpiresAt:  grant.ExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"workspace_id": workspaceID, "capabilities": out})
}

func (d *Server) handleWorkspaceObservationProfiles(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req api.WorkspaceObservationProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if !d.localCLIRequest(r, req.Platform) {
		http.Error(w, "observation profiles can only be granted by the authenticated local CLI", http.StatusForbidden)
		return
	}
	identity, err := d.Control.ResolveOrCreateAccount(
		r.Context(), d.tenantID(req.TenantID), "cli", fallback(req.PlatformUserID, "local"), "",
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	workspaceID := strings.TrimSpace(req.WorkspaceID)
	var workspace *control.Workspace
	if workspaceID == "" {
		workspace, err = d.Control.CurrentWorkspace(r.Context(), identity.TenantID, identity.PersonID)
	} else {
		workspace, err = d.Control.GetWorkspace(r.Context(), identity.TenantID, workspaceID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if workspace == nil || workspace.OwnerPersonID != identity.PersonID {
		http.Error(w, "workspace not found", http.StatusNotFound)
		return
	}
	if workspace.TrustLevel != executionenv.TrustTrusted {
		http.Error(w, "workspace must be trusted before declaring an observation script", http.StatusConflict)
		return
	}
	rule, err := tools.BuildObservationScriptRule(tools.ObservationScriptProfile{
		WorkspaceID: workspace.ID, ScriptPath: req.ScriptPath, ArgvPrefix: req.ArgvPrefix,
		AllowTrailing: req.AllowTrailing, AllowNetwork: req.AllowNetwork, AllowCredentials: req.AllowCredentials,
	}, workspace.LocalPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := d.Control.GrantApproval(r.Context(), "person", identity.TenantID, identity.PersonID, identity.PersonID, rule.Key, time.Time{}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"workspace_id": workspace.ID,
		"profile": map[string]interface{}{
			"kind": rule.Kind, "label": rule.Label, "argv_prefix": req.ArgvPrefix,
			"allow_trailing": req.AllowTrailing, "allow_network": req.AllowNetwork,
			"allow_credentials": req.AllowCredentials,
		},
	})
}

func (d *Server) localCLIRequest(r *http.Request, platform string) bool {
	if r == nil || !strings.EqualFold(strings.TrimSpace(platform), "cli") {
		return false
	}
	expected := strings.TrimSpace(d.LocalControlToken)
	provided := strings.TrimSpace(r.Header.Get(api.LocalControlTokenHeader))
	if expected == "" || len(expected) != len(provided) ||
		subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
		return false
	}
	host := strings.TrimSpace(r.RemoteAddr)
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
	// Same display-ordered fetch as /workspaces so API consumers see the list
	// in the order ordinal references resolve against.
	workspaces, err := d.listWorkspacesForDisplay(r.Context(), identity)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"identity": identity, "workspaces": workspaces})
}
