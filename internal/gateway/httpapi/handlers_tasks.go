package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
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
