package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func (d *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
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
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = "pending"
	}
	limit := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	approvals, err := d.Control.ListApprovalRequests(r.Context(), identity.TenantID, identity.PersonID, status, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ApprovalListResponse{Identity: identity, Approvals: approvals})
}

func (d *Server) handleApprovalRespond(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req api.ApprovalRespondRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	identity, err := d.Control.ResolveOrCreateAccount(
		r.Context(),
		d.tenantID(req.TenantID),
		fallback(req.Platform, "cli"),
		fallback(req.PlatformUserID, "local"),
		req.DisplayName,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	approval, err := d.Control.RespondApprovalRequest(r.Context(), identity.TenantID, identity.PersonID, req.ApprovalID, req.Decision, fallback(req.Channel, req.Platform))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	appendApprovalEvent(r.Context(), d.Control, approval, fallback(req.Channel, req.Platform))
	writeJSON(w, http.StatusOK, api.ApprovalRespondResponse{Identity: identity, Approval: approval})
}

func appendApprovalEvent(ctx context.Context, store *control.Store, approval *control.ApprovalRequest, channel string) {
	if store == nil || approval == nil || approval.TaskID == "" {
		return
	}
	_, _ = store.AppendEvent(ctx, control.Event{
		TaskID:     approval.TaskID,
		RunID:      approval.RunID,
		Type:       "approval." + approval.Status,
		Visibility: "task",
		Channel:    channel,
		Payload:    mustJSON(map[string]string{"approval_id": approval.ID, "action_type": approval.ActionType}),
	})
}
