package httpapi

import (
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
	if status == "pending" {
		// Same stable order as the /approvals text list so clients can render
		// ordinals that match what /approve <n> resolves.
		sortApprovalsForDisplay(approvals)
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
	// ApprovalID accepts the same references as /approve: the /approvals list
	// number, a unique apr_ prefix, a full id, or empty when exactly one
	// approval is pending. Resolution failures are user errors (400 + a
	// human-readable message), never a 500.
	approval, err := d.respondApprovalByToken(r.Context(), identity, req.ApprovalID, req.Decision, fallback(req.Channel, req.Platform),
		control.ApprovalDecisionInput{GrantScope: req.Scope, GrantKey: req.GrantKey, Note: req.Note})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, api.ApprovalRespondResponse{Identity: identity, Approval: approval})
}
