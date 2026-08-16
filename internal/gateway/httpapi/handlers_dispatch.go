package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
)

// dispatchSafelist is the set of management tools a thin client may run via
// /v1/dispatch. It is deliberately limited to read/curate/learning-management
// tools that the user can already invoke through slash commands. It must NOT
// include workspace-mutating or code-executing tools (write_file, edit_file,
// terminal, execute_code, web_*), which have to flow through a real agent turn
// so workspace scope, approval mode, and run events apply. This keeps
// /v1/dispatch from becoming a tool-execution backdoor around those guards.
var dispatchSafelist = map[string]bool{
	"memory":                 true,
	"skill_manage":           true,
	"skill_catalog":          true,
	"skill_bundle":           true,
	"skill_lifecycle_manage": true,
	"checkpoint":             true,
	"session_search":         true, // read-only; backs the TUI /search command
}

// directSkillManagementTools are the explicit, authenticated management
// surfaces whose mutation capability is granted by /v1/dispatch. Keep this
// narrower than dispatchSafelist: catalog, bundle, memory, and read-only tools
// do not inherit active-Skill write authority merely because they share the
// endpoint.
var directSkillManagementTools = map[string]bool{
	"skill_manage":           true,
	"skill_lifecycle_manage": true,
}

// personPartitionTools are dispatch tools whose backing store partitions by
// PERSON: daemon agent runs execute with the person id as the storage tenant
// (`RunAgentWithEvents(ctx, identity.PersonID, …)` — see AGENTS.md "Context &
// Memory": the agent's storage tenant is person_id), so memory facts, session
// FTS rows, and checkpoints all live under the person partition. Dispatching
// these tools with the CONTROL tenant used to read the wrong (empty or stale
// in-process-era) partition — client `/memory list` could not see what the
// daemon had learned. Skill tools stay on the control tenant: the daemon's
// skills dir is keyed by the tenant the agent was built with.
var personPartitionTools = map[string]bool{
	"memory":         true,
	"session_search": true,
	"checkpoint":     true,
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
	// Partition scope is authoritative from the resolved identity, never from
	// the client-supplied args. Person-partitioned stores get the person id
	// (matching what agent runs write); tenant-partitioned stores (skills) get
	// the control tenant.
	if personPartitionTools[tool] {
		args["_tenant_id"] = identity.PersonID
	} else {
		args["_tenant_id"] = identity.TenantID
	}
	args["_context"] = r.Context()
	if directSkillManagementTools[tool] {
		task, taskErr := d.Control.CurrentTask(r.Context(), identity.TenantID, identity.PersonID)
		if taskErr != nil {
			writeJSON(w, http.StatusOK, api.DispatchResponse{Error: taskErr.Error()})
			return
		}
		scope := kernel.ToolInvocationScope{
			ControlTenantID: identity.TenantID, PersonID: identity.PersonID,
			SkillMutationMode: kernel.SkillMutationDirect,
		}
		if task != nil {
			scope.TaskID = task.ID
			scope.WorkspaceID = task.WorkspaceID
		}
		managementRunID := "management-" + uuid.NewString()
		scope.RunID = managementRunID
		executionScope := tools.ExecutionScope{
			TenantID: identity.TenantID, PersonID: identity.PersonID,
			TaskID: scope.TaskID, RunID: managementRunID, WorkspaceID: scope.WorkspaceID,
		}
		if scope.WorkspaceID != "" {
			workspace, workspaceErr := d.Control.GetWorkspace(r.Context(), identity.TenantID, scope.WorkspaceID)
			if workspaceErr != nil {
				writeJSON(w, http.StatusOK, api.DispatchResponse{Error: workspaceErr.Error()})
				return
			}
			if workspace != nil && workspace.OwnerPersonID == identity.PersonID {
				executionScope.WorkspaceRoot = workspace.LocalPath
				executionScope.AllowedRoots = append([]string{}, workspace.AllowedRoots...)
				if len(executionScope.AllowedRoots) == 0 && workspace.LocalPath != "" {
					executionScope.AllowedRoots = []string{workspace.LocalPath}
				}
				executionScope.TrustLevel = workspace.TrustLevel
			}
		}
		cleanup := tools.SetExecutionScope("", executionScope)
		defer cleanup()
		managementContext := tools.WithExecutionScopeKey(r.Context(), tools.ExecutionScopeKeyForRun(managementRunID))
		args["_context"] = kernel.WithToolInvocationScope(managementContext, scope)
		args["_invocation_scope"] = scope
	}

	result, derr := d.Gateway.DispatchTool(tool, args)
	if derr != nil {
		writeJSON(w, http.StatusOK, api.DispatchResponse{Error: derr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, api.DispatchResponse{Result: result})
}
