package tools

import (
	"strings"

	"selfmind/internal/kernel"
)

// InvocationScopeFromArgs returns the authenticated, gateway-created scope.
// Model-supplied JSON cannot populate hidden underscore-prefixed arguments.
func InvocationScopeFromArgs(args map[string]interface{}) (kernel.ToolInvocationScope, bool) {
	if args == nil {
		return kernel.ToolInvocationScope{}, false
	}
	scope, ok := args["_invocation_scope"].(kernel.ToolInvocationScope)
	return scope, ok
}

func skillStorageTenantID(args map[string]interface{}) string {
	if scope, ok := InvocationScopeFromArgs(args); ok {
		if tenantID := strings.TrimSpace(scope.ControlTenantID); tenantID != "" {
			return tenantID
		}
	}
	if tenantID, _ := args["_tenant_id"].(string); strings.TrimSpace(tenantID) != "" {
		return strings.TrimSpace(tenantID)
	}
	return "default"
}

func processRegistryScopeID(args map[string]interface{}) string {
	if scope, ok := InvocationScopeFromArgs(args); ok {
		if leaseID := strings.TrimSpace(scope.LeaseID); leaseID != "" {
			return "lease:" + leaseID
		}
		if runID := strings.TrimSpace(scope.RunID); runID != "" {
			return "run:" + runID
		}
		if personID := strings.TrimSpace(scope.PersonID); personID != "" {
			return "person:" + personID
		}
	}
	if tenantID, _ := args["_tenant_id"].(string); strings.TrimSpace(tenantID) != "" {
		return strings.TrimSpace(tenantID)
	}
	return "default"
}
