package tools

import (
	"strings"
	"testing"
	"time"

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
	got := externalWatchEffectiveCapabilities(args, ExecutionScope{TrustLevel: executionenv.TrustUntrusted}, nil)
	if len(got) != 1 || got[0] != executionenv.CapabilityCredentialRead {
		t.Fatalf("effective capabilities = %#v, want only current credential approval", got)
	}
}

// A trusted workspace whose operator policy allows egress gives a FOREGROUND
// command shared networking without asking. The durable watcher must freeze the
// same capability, and must record it as trust-derived: the arg the capability
// middleware normally sets never arrives when the effective sandbox mode is
// `host` (the normal mode on macOS), which is how every such watch froze an
// empty set and then refused its own polls.
func TestExternalWatchFreezesTrustDerivedNetworkCapability(t *testing.T) {
	enabled, required, network := execSandboxPolicy()
	SetExecSandbox(enabled, required, true)
	t.Cleanup(func() { SetExecSandbox(enabled, required, network) })

	scope := ExecutionScope{WorkspaceID: "workspace-1", TrustLevel: executionenv.TrustTrusted}
	capabilities := externalWatchEffectiveCapabilities(map[string]interface{}{
		"_tool_name": "watch_external",
		"command":    "gcloud builds describe build-1 --format=value(status)",
	}, scope, nil)
	if len(capabilities) != 1 || capabilities[0] != executionenv.CapabilityNetworkShared {
		t.Fatalf("frozen capabilities = %#v, want network:shared", capabilities)
	}
	bindings := externalWatchCapabilityBindings(capabilities, scope, nil, time.Now().Add(time.Hour))
	if len(bindings) != 1 || bindings[0].Source != executionenv.CapabilitySourceTrust {
		t.Fatalf("capability provenance = %#v, want workspace trust", bindings)
	}
	if !bindings[0].ExpiresAt.IsZero() {
		t.Fatalf("a trust-derived capability must not carry its own expiry: %v", bindings[0].ExpiresAt)
	}
}

// The trust path is not a way around the operator's sandbox policy: with egress
// disabled, a trusted workspace freezes no network capability at all, so the
// durable poll refuses instead of running an unauthorized command.
func TestExternalWatchFreezesNoNetworkCapabilityWithoutSandboxEgress(t *testing.T) {
	enabled, required, network := execSandboxPolicy()
	SetExecSandbox(enabled, required, false)
	t.Cleanup(func() { SetExecSandbox(enabled, required, network) })

	capabilities := externalWatchEffectiveCapabilities(map[string]interface{}{
		"_tool_name": "watch_external",
		"command":    "gcloud builds describe build-1 --format=value(status)",
	}, ExecutionScope{WorkspaceID: "workspace-1", TrustLevel: executionenv.TrustTrusted}, nil)
	if len(capabilities) != 0 {
		t.Fatalf("frozen capabilities = %#v, want none while the sandbox policy denies egress", capabilities)
	}
}

// An untrusted workspace with no grant and no approval still freezes nothing.
// Resolving capabilities at registration must not become a second authority.
func TestExternalWatchFreezesNoNetworkCapabilityForUntrustedWorkspace(t *testing.T) {
	enabled, required, network := execSandboxPolicy()
	SetExecSandbox(enabled, required, true)
	t.Cleanup(func() { SetExecSandbox(enabled, required, network) })

	capabilities := externalWatchEffectiveCapabilities(map[string]interface{}{
		"_tool_name": "watch_external",
		"command":    "gcloud builds describe build-1 --format=value(status)",
	}, ExecutionScope{WorkspaceID: "workspace-1", TrustLevel: executionenv.TrustUntrusted}, nil)
	if len(capabilities) != 0 {
		t.Fatalf("frozen capabilities = %#v, want none for an untrusted workspace", capabilities)
	}
}

// A live workspace grant is the untrusted path to the same capability. It stays
// grant-derived, so revoking the grant stops the watch before its next poll.
func TestExternalWatchFreezesGrantDerivedNetworkCapability(t *testing.T) {
	enabled, required, network := execSandboxPolicy()
	SetExecSandbox(enabled, required, false)
	t.Cleanup(func() { SetExecSandbox(enabled, required, network) })

	scope := ExecutionScope{WorkspaceID: "workspace-1", TrustLevel: executionenv.TrustUntrusted}
	grants := []executionenv.CapabilityGrant{{
		ID:                  "cap_1",
		Capability:          executionenv.CapabilityNetworkShared,
		ResourceFingerprint: "network-resource",
		ExpiresAt:           time.Now().Add(time.Hour),
	}}
	capabilities := externalWatchEffectiveCapabilities(map[string]interface{}{
		"_tool_name": "watch_external",
		"command":    "gcloud builds describe build-1 --format=value(status)",
	}, scope, grants)
	if len(capabilities) != 1 || capabilities[0] != executionenv.CapabilityNetworkShared {
		t.Fatalf("frozen capabilities = %#v, want the granted network capability", capabilities)
	}
	bindings := externalWatchCapabilityBindings(capabilities, scope, grants, time.Now().Add(time.Hour))
	if len(bindings) != 1 || bindings[0].Source != executionenv.CapabilitySourceGrant || bindings[0].GrantID != "cap_1" {
		t.Fatalf("capability provenance = %#v, want the durable grant", bindings)
	}
}
