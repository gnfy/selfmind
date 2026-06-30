package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"selfmind/internal/gateway/api"
)

// dispatchSafelist is the set of management tools a thin client may run via
// /v1/dispatch. It is deliberately limited to read/curate/learning-management
// tools that the user can already invoke through slash commands. It must NOT
// include workspace-mutating or code-executing tools (write_file, edit_file,
// terminal, execute_code, web_*), which have to flow through a real agent turn
// so workspace scope, approval mode, and run events apply. This keeps
// /v1/dispatch from becoming a tool-execution backdoor around those guards.
var dispatchSafelist = map[string]bool{
	"memory":         true,
	"skill_manage":   true,
	"skill_catalog":  true,
	"skill_bundle":   true,
	"checkpoint":     true,
	"session_search": true, // read-only; reserved for a future client session browser
}

// handleDispatch runs a single safelisted management tool on the daemon's agent
// and returns its text result, so a daemon-client TUI can execute agent-backed
// slash commands (/skills, /memory subcommands, /bundles, /curator,
// /checkpoint). The tenant scope is taken from the resolved identity, never from
// the client-supplied args, so a client cannot reach another tenant's data.
func (d *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req api.DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	tool := strings.TrimSpace(req.Tool)
	if tool == "" {
		http.Error(w, "tool is required", http.StatusBadRequest)
		return
	}
	if !dispatchSafelist[tool] {
		writeJSON(w, http.StatusForbidden, api.DispatchResponse{Error: "tool not allowed via dispatch: " + tool})
		return
	}
	if d.Gateway == nil {
		writeJSON(w, http.StatusServiceUnavailable, api.DispatchResponse{Error: "gateway agent is not configured"})
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

	args := map[string]interface{}{}
	for k, v := range req.Args {
		args[k] = v
	}
	// Tenant scope is authoritative from the resolved identity, not the client.
	args["_tenant_id"] = identity.TenantID

	result, derr := d.Gateway.DispatchTool(tool, args)
	if derr != nil {
		writeJSON(w, http.StatusOK, api.DispatchResponse{Error: derr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, api.DispatchResponse{Result: result})
}
