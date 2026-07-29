package httpapi

import (
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/tools"
)

// A durable watch outlives its run and survives restarts, so it is the execution
// path most likely to straddle an environment change — and the failure mode is
// not an error but a check that SUCCEEDS against the wrong account. The identity
// recorded at registration must therefore be verified before every check.
func TestExternalWatchDetectsEnvironmentChange(t *testing.T) {
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })

	accountA := registry.Install([]string{"PATH=/usr/bin", "HOME=/home/u", "CLOUDSDK_CONFIG=/home/u/.config/gcloud"},
		"inherited", "principal-account-A", []string{"environment:GCLOUD_TOKEN"})

	watch := control.ExternalWatch{
		ID:                     "watch_1",
		PrincipalFingerprint:   accountA.PrincipalFingerprint,
		EnvironmentFingerprint: accountA.EnvironmentFingerprint,
		CredentialSourceHash:   accountA.CredentialSourceHash,
	}

	// Same environment: the watch proceeds.
	if changed := externalWatchEnvironmentChange(watch); len(changed) != 0 {
		t.Fatalf("an unchanged environment must not stop a watch: %v", changed)
	}

	// The operator switches account. The daemon restarts and installs a new
	// snapshot; the watch must NOT quietly start reporting on account B.
	registry.Install([]string{"PATH=/usr/bin", "HOME=/home/u", "CLOUDSDK_CONFIG=/home/u/.config/gcloud"},
		"inherited", "principal-account-B", []string{"environment:GCLOUD_TOKEN"})
	changed := externalWatchEnvironmentChange(watch)
	if len(changed) == 0 {
		t.Fatal("an account change must stop the watch instead of checking as someone else")
	}
	if strings.Join(changed, ",") != "account/profile" {
		t.Fatalf("unexpected change description: %v", changed)
	}

	// A credential source moving is also a change: the same account read from a
	// different config location is a different execution identity.
	registry.Install([]string{"PATH=/usr/bin", "HOME=/home/u", "CLOUDSDK_CONFIG=/home/u/.config/gcloud-alt"},
		"inherited", "principal-account-A", []string{"environment:GCLOUD_TOKEN"})
	changed = externalWatchEnvironmentChange(watch)
	if len(changed) == 0 || !strings.Contains(strings.Join(changed, ","), "credential source") {
		t.Fatalf("a credential source change must be detected: %v", changed)
	}
}

// Watches registered before identity existed carry no fingerprints. Parking them
// would strand work the operator already started for a property that was never
// recorded, so they are grandfathered.
func TestExternalWatchWithoutRecordedIdentityIsGrandfathered(t *testing.T) {
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	registry.Install([]string{"PATH=/usr/bin"}, "inherited", "principal-new", nil)

	if changed := externalWatchEnvironmentChange(control.ExternalWatch{ID: "watch_legacy"}); len(changed) != 0 {
		t.Fatalf("a legacy watch must not be parked for an unrecorded property: %v", changed)
	}
}

// The recorded identity must be non-secret: fingerprints and a snapshot id only.
func TestExternalWatchIdentityCarriesNoSecrets(t *testing.T) {
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })

	snapshot := tools.InstallEnvironmentSnapshot([]string{
		"PATH=/usr/bin",
		"GCLOUD_TOKEN=super-secret-value",
		"CLOUDSDK_CONFIG=/home/u/.config/gcloud",
	}, "inherited")

	watch := control.ExternalWatch{
		EnvironmentSnapshotID:  snapshot.ID,
		EnvironmentGeneration:  snapshot.Generation,
		PrincipalFingerprint:   snapshot.PrincipalFingerprint,
		EnvironmentFingerprint: snapshot.EnvironmentFingerprint,
		CredentialSourceHash:   snapshot.CredentialSourceHash,
	}
	rendered := strings.Join([]string{
		watch.EnvironmentSnapshotID, watch.PrincipalFingerprint,
		watch.EnvironmentFingerprint, watch.CredentialSourceHash,
	}, " ")
	for _, forbidden := range []string{"super-secret-value", "GCLOUD_TOKEN"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("watch identity leaked %q: %s", forbidden, rendered)
		}
	}
	if watch.EnvironmentFingerprint == "" || watch.CredentialSourceHash == "" {
		t.Fatal("identity must actually be populated, or the check is a no-op")
	}
}
