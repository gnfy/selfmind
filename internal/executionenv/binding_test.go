package executionenv

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestResolveBindingKeepsExactSnapshotWhenCurrentChanges(t *testing.T) {
	registry := NewRegistry()
	first := registry.Install([]string{"HOME=/home/user", "MARKER=first", "API_TOKEN=secret-a"},
		"inherited", "principal-a", []string{"environment:API_TOKEN"})
	lease := Lease{
		ID:                     "lease-1",
		TenantID:               "default",
		PersonID:               "person-1",
		EnvironmentSnapshotID:  first.ID,
		EnvironmentGeneration:  first.Generation,
		PrincipalFingerprint:   first.PrincipalFingerprint,
		EnvironmentFingerprint: first.EnvironmentFingerprint,
		CredentialSourceHash:   first.CredentialSourceHash,
	}
	binding := BindingFromLease("binding-1", lease, TrustTrusted,
		[]string{CapabilityNetworkShared}, first)

	registry.Install([]string{"HOME=/home/other", "MARKER=second"},
		"inherited", "principal-b", nil)
	resolved, err := ResolveBinding(registry, binding)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != first.ID {
		t.Fatalf("resolved snapshot = %q, want original %q", resolved.ID, first.ID)
	}
	if got := strings.Join(resolved.Env(), "\n"); !strings.Contains(got, "MARKER=first") {
		t.Fatalf("durable binding adopted the current environment: %s", got)
	}

	rendered, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret-a", "API_TOKEN=secret"} {
		if strings.Contains(string(rendered), forbidden) {
			t.Fatalf("binding persisted secret material %q: %s", forbidden, rendered)
		}
	}
}

func TestResolveBindingRebindsCompatibleSnapshotAfterRestart(t *testing.T) {
	oldRegistry := NewRegistry()
	old := oldRegistry.Install([]string{"HOME=/home/user"}, "inherited", "principal-a", nil)
	binding := BindingFromLease("binding-1", Lease{ID: "lease-1"}, TrustTrusted, nil, old)

	restarted := NewRegistry()
	current := restarted.Install([]string{"HOME=/home/user"}, "inherited", "principal-a", nil)
	resolved, err := ResolveBinding(restarted, binding)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != current.ID {
		t.Fatalf("resolved snapshot = %q, want restart snapshot %q", resolved.ID, current.ID)
	}
	if rebound, ok := restarted.ForLease(binding.ID); !ok || rebound.ID != current.ID {
		t.Fatalf("binding was not installed for later polls: %#v, %v", rebound, ok)
	}
}

func TestResolveBindingRejectsDifferentPrincipalAfterRestart(t *testing.T) {
	oldRegistry := NewRegistry()
	old := oldRegistry.Install([]string{"HOME=/home/user"}, "inherited", "principal-a", nil)
	binding := BindingFromLease("binding-1", Lease{ID: "lease-1"}, TrustTrusted, nil, old)

	restarted := NewRegistry()
	restarted.Install([]string{"HOME=/home/user"}, "inherited", "principal-b", nil)
	_, err := ResolveBinding(restarted, binding)
	var changed *EnvironmentChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("expected EnvironmentChangedError, got %v", err)
	}
	if len(changed.Changed) != 1 || changed.Changed[0] != "account/profile" {
		t.Fatalf("changed dimensions = %v", changed.Changed)
	}
}
