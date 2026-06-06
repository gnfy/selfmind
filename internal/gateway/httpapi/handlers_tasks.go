package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

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
