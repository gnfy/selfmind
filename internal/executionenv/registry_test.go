package executionenv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryBindsLeaseToOneSnapshot(t *testing.T) {
	r := NewRegistry()
	realA, realB := t.TempDir(), t.TempDir()
	first := r.Install([]string{"PATH=" + realA, "HOME=/home/u"}, "inherited", "principal-a", []string{"environment:GCLOUD_PROJECT"})
	if first.ID == "" || first.Generation != 1 {
		t.Fatalf("unexpected first snapshot: %+v", first)
	}
	r.BindLease("lease-1", first.ID)

	// A genuinely different environment creates a NEW generation, but the run
	// already bound must keep resolving its original values — that is the whole
	// point of binding through the lease.
	second := r.Install([]string{"PATH=" + realA + string(os.PathListSeparator) + realB, "HOME=/home/u"}, "login-shell", "principal-a", nil)
	if second.Generation != 2 {
		t.Fatalf("expected a new generation, got %d", second.Generation)
	}
	bound, ok := r.ForLease("lease-1")
	if !ok || bound.ID != first.ID {
		t.Fatalf("lease must stay bound to its original snapshot, got %+v", bound)
	}
	if got := strings.Join(bound.Env(), " "); strings.Contains(got, realB) {
		t.Fatalf("bound snapshot leaked the newer environment: %q", got)
	}

	r.ReleaseLease("lease-1")
	if _, ok := r.ForLease("lease-1"); ok {
		t.Fatal("released lease must not resolve")
	}
}

// An identical re-sample must not burn a generation: an idle refresh would
// otherwise move every new run onto a fresh binding for no reason. It must also
// not mutate the snapshot — a bound run resolves these values, so in-place
// refresh would defeat the binding it depends on.
func TestRegistryIdenticalSampleKeepsGenerationAndValues(t *testing.T) {
	r := NewRegistry()
	env := []string{"PATH=/usr/bin", "HOME=/home/u", "TOKEN=abc"}
	first := r.Install(env, "inherited", "p", nil)
	second := r.Install([]string{"PATH=/usr/bin", "HOME=/home/u", "TOKEN=rotated"}, "login-shell", "p", nil)
	if second.Generation != first.Generation || second.ID != first.ID {
		t.Fatalf("identical environment must keep its snapshot: %d/%s vs %d/%s",
			first.Generation, first.ID, second.Generation, second.ID)
	}
	if got := strings.Join(second.Env(), " "); !strings.Contains(got, "TOKEN=abc") {
		t.Fatalf("an installed snapshot must be immutable, got %q", got)
	}
}

// The live failure this pins: the daemon's PATH contained
// /run/user/1000/fnm_multishells/<pid>_<ts>/bin. Hashing PATH verbatim would
// change the fingerprint on every restart, so every recovered run would be
// parked as environment_changed even though nothing meaningful moved.
func TestEnvironmentFingerprintIgnoresSessionScopedPaths(t *testing.T) {
	real := t.TempDir()
	r := NewRegistry()
	base := fmt.Sprintf("PATH=%s%c/run/user/1000/fnm_multishells/111_1785211899779/bin", real, os.PathListSeparator)
	next := fmt.Sprintf("PATH=%s%c/run/user/1000/fnm_multishells/999_1785299999999/bin", real, os.PathListSeparator)

	a := r.Sample([]string{base, "HOME=/home/u"}, "inherited", "p", nil)
	b := r.Sample([]string{next, "HOME=/home/u"}, "inherited", "p", nil)
	if a.EnvironmentFingerprint != b.EnvironmentFingerprint {
		t.Fatalf("session-scoped path change must not alter the fingerprint:\n%s\n%s",
			a.EnvironmentFingerprint, b.EnvironmentFingerprint)
	}
	if a.VolatileCount != 1 {
		t.Fatalf("volatile entries must be counted for diagnostics, got %d", a.VolatileCount)
	}

	// A real toolchain directory appearing IS a change.
	other := t.TempDir()
	c := r.Sample([]string{base + string(os.PathListSeparator) + other, "HOME=/home/u"}, "inherited", "p", nil)
	if c.EnvironmentFingerprint == a.EnvironmentFingerprint {
		t.Fatal("a new real PATH directory must change the fingerprint")
	}
}

// A PATH entry that no longer exists must not keep the fingerprint alive: it is
// exactly as unusable as a session-scoped one.
func TestEnvironmentFingerprintDropsMissingPathEntries(t *testing.T) {
	real := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")
	r := NewRegistry()
	withMissing := r.Sample([]string{fmt.Sprintf("PATH=%s%c%s", real, os.PathListSeparator, missing)}, "inherited", "p", nil)
	withoutMissing := r.Sample([]string{"PATH=" + real}, "inherited", "p", nil)
	if withMissing.EnvironmentFingerprint != withoutMissing.EnvironmentFingerprint {
		t.Fatal("a nonexistent PATH entry must not affect the fingerprint")
	}
}

func TestCredentialSourceHashTracksLocationNotValue(t *testing.T) {
	r := NewRegistry()
	base := []string{"PATH=/usr/bin", "CLOUDSDK_CONFIG=/home/u/.config/gcloud", "GCLOUD_TOKEN=secret-1"}
	rotated := []string{"PATH=/usr/bin", "CLOUDSDK_CONFIG=/home/u/.config/gcloud", "GCLOUD_TOKEN=secret-2"}
	moved := []string{"PATH=/usr/bin", "CLOUDSDK_CONFIG=/home/u/.config/gcloud-alt", "GCLOUD_TOKEN=secret-1"}

	a := r.Sample(base, "inherited", "p", []string{"environment:GCLOUD_TOKEN"})
	b := r.Sample(rotated, "inherited", "p", []string{"environment:GCLOUD_TOKEN"})
	c := r.Sample(moved, "inherited", "p", []string{"environment:GCLOUD_TOKEN"})

	if a.CredentialSourceHash != b.CredentialSourceHash {
		t.Fatal("rotating a token inside the same source must not change the credential source hash")
	}
	if a.CredentialSourceHash == c.CredentialSourceHash {
		t.Fatal("moving the config location must change the credential source hash")
	}
	// A secret value must never reach a fingerprint input.
	for _, snapshot := range []*Snapshot{a, b, c} {
		if strings.Contains(snapshot.CredentialSourceHash, "secret") {
			t.Fatal("credential value leaked into the hash")
		}
	}
}

func TestMatchesRequiresAllThreeFingerprints(t *testing.T) {
	r := NewRegistry()
	base := r.Sample([]string{"PATH=/usr/bin", "HOME=/h"}, "inherited", "principal-a", []string{"environment:A"})
	same := r.Sample([]string{"PATH=/usr/bin", "HOME=/h"}, "inherited", "principal-a", []string{"environment:A"})
	if !base.Matches(same) {
		t.Fatal("identical samples must match")
	}
	otherPrincipal := r.Sample([]string{"PATH=/usr/bin", "HOME=/h"}, "inherited", "principal-b", []string{"environment:A"})
	if base.Matches(otherPrincipal) {
		t.Fatal("a different account must not match")
	}
	otherSource := r.Sample([]string{"PATH=/usr/bin", "HOME=/h"}, "inherited", "principal-a", []string{"environment:B"})
	if base.Matches(otherSource) {
		t.Fatal("a different credential source must not match")
	}
}

func TestDescribeEnvironmentChangeNamesDimensions(t *testing.T) {
	lease := &Lease{PrincipalFingerprint: "a", EnvironmentFingerprint: "b", CredentialSourceHash: "c"}
	snapshot := &Snapshot{PrincipalFingerprint: "z", EnvironmentFingerprint: "b", CredentialSourceHash: "y"}
	changed := DescribeEnvironmentChange(lease, snapshot)
	if len(changed) != 2 || changed[0] != "account/profile" || changed[1] != "credential source" {
		t.Fatalf("unexpected change description: %v", changed)
	}
	err := &EnvironmentChangedError{LeaseID: "lease-1", Changed: changed}
	if !strings.Contains(err.Error(), "environment_changed") {
		t.Fatalf("error text must carry the structured reason: %q", err.Error())
	}
}
