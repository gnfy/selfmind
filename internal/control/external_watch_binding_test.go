package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"selfmind/internal/executionenv"
)

func TestExternalWatchPersistsSecretFreeExecutionBinding(t *testing.T) {
	ctx := context.Background()
	store, identity, task, run := newRecoveryFixture(t)
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	binding := executionenv.Binding{
		Version:               executionenv.BindingVersion,
		SourceLeaseID:         "lease-1",
		EnvironmentProfile:    "operator",
		CredentialRefs:        []executionenv.CredentialRef{{Kind: "environment", Source: "API_TOKEN"}},
		PrincipalFingerprint:  "principal-fingerprint",
		ExecutionCapabilities: []string{executionenv.CapabilityNetworkShared},
		CapabilityBindings: []executionenv.CapabilityBinding{{
			Capability: executionenv.CapabilityNetworkShared,
			Source:     executionenv.CapabilitySourceRegistration,
			ExpiresAt:  expiresAt,
		}},
		TrustLevel:             executionenv.TrustUntrusted,
		EnvironmentSnapshotID:  "snapshot-1",
		EnvironmentGeneration:  7,
		EnvironmentFingerprint: "environment-fingerprint",
		CredentialSourceHash:   "credential-source-hash",
	}
	created, err := store.CreateExternalWatch(ctx, ExternalWatch{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		TaskID: task.ID, RunID: run.ID, Channel: "cli",
		CWD: t.TempDir(), Command: "check", SuccessPattern: "^SUCCESS$",
		TimeoutAt: time.Now().Add(time.Hour), ExecutionBinding: binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetExternalWatch(ctx, identity.TenantID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.ExecutionBinding.ID != "binding:"+created.ID {
		t.Fatalf("execution binding did not round trip: %#v", loaded)
	}
	if loaded.ExecutionBinding.TenantID != identity.TenantID || loaded.ExecutionBinding.PersonID != identity.PersonID {
		t.Fatalf("binding identity was not normalized from watch ownership: %#v", loaded.ExecutionBinding)
	}
	if len(loaded.ExecutionBinding.CapabilityBindings) != 1 ||
		!loaded.ExecutionBinding.CapabilityBindings[0].ExpiresAt.Equal(expiresAt) {
		t.Fatalf("capability provenance did not round trip: %#v", loaded.ExecutionBinding.CapabilityBindings)
	}
	rendered, err := json.Marshal(loaded.ExecutionBinding)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "secret-value") {
		t.Fatalf("binding contains secret material: %s", rendered)
	}
}
