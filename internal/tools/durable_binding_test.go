package tools

import (
	"strings"
	"testing"

	"selfmind/internal/executionenv"
)

func TestDurableMaterialUsesFrozenBindingInsteadOfCurrentEnvironment(t *testing.T) {
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })

	first := registry.Install([]string{"HOME=/home/user", "DURABLE_MARKER=first"},
		"inherited", "principal-a", nil)
	binding := executionenv.BindingFromLease("binding-1", executionenv.Lease{
		ID: "lease-1", TenantID: "default", PersonID: "person-1",
	}, executionenv.TrustUntrusted, []string{executionenv.CapabilityNetworkShared}, first)
	registry.Install([]string{"HOME=/home/other", "DURABLE_MARKER=second"},
		"inherited", "principal-b", nil)

	material, err := durableExecMaterial(nil, "printf ok", t.TempDir(), DurableExecutionScope{Binding: binding})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(material.Env, "\n")
	if !strings.Contains(joined, "DURABLE_MARKER=first") || strings.Contains(joined, "DURABLE_MARKER=second") {
		t.Fatalf("durable material did not preserve its binding: %s", joined)
	}
}

func TestExternalWatchEffectiveCapabilitiesIgnoreStaleLeaseCapabilities(t *testing.T) {
	args := map[string]interface{}{
		"_network_shared":    false,
		credentialReadArgKey: true,
	}
	got := externalWatchEffectiveCapabilities(args)
	if len(got) != 1 || got[0] != executionenv.CapabilityCredentialRead {
		t.Fatalf("effective capabilities = %#v, want only current credential approval", got)
	}
}
