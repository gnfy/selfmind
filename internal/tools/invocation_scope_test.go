package tools

import (
	"testing"

	"selfmind/internal/kernel"
)

func TestInvocationScopeRoutesAssetsAndProcessesIndependently(t *testing.T) {
	args := map[string]interface{}{
		"_tenant_id": "person",
		"_invocation_scope": kernel.ToolInvocationScope{
			ControlTenantID: "tenant", PersonID: "person", RunID: "run", LeaseID: "lease",
		},
	}
	if got := skillStorageTenantID(args); got != "tenant" {
		t.Fatalf("skill tenant = %q", got)
	}
	if got := processRegistryScopeID(args); got != "lease:lease" {
		t.Fatalf("process scope = %q", got)
	}
}
