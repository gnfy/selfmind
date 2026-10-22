package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
)

// Each backend declares whether it realizes mount-backed tool state itself.
// Preparation asks the backend rather than the platform.
func TestBackendsDeclareWhetherTheyMountToolState(t *testing.T) {
	if !(bubblewrapBackend{}).MountsToolState() {
		t.Fatal("bubblewrap mounts private overlays for tool state")
	}
	if (seatbeltBackend{}).MountsToolState() || (hostBackend{}).MountsToolState() {
		t.Fatal("seatbelt and host execution cannot mount tool state")
	}
}

// In-place state writes are a non-mounting backend's realization. A mounting
// backend that received them would write the host where it promises a private
// overlay, so it refuses the plan instead.
func TestMountingBackendRefusesInPlaceStateWrites(t *testing.T) {
	material := ProcessMaterial{env: []string{"PATH=/usr/bin:/bin"}}
	_, err := (bubblewrapBackend{}).Command(context.Background(), []string{"/bin/sh", "-c", "true"},
		SandboxPlan{HostStateWrites: []string{"/home/u/.aws/sso/cache"}}, material)
	if !errors.Is(err, errSandboxUnavailable) {
		t.Fatalf("bubblewrap must refuse in-place state writes, got %v", err)
	}
}

func TestSeatbeltBackendGrantsInPlaceStateWrites(t *testing.T) {
	if !(seatbeltBackend{}).Available() {
		t.Skip("seatbelt unavailable")
	}
	state := canonicalPath(t, t.TempDir())
	cmd, err := (seatbeltBackend{}).Command(context.Background(), []string{"/bin/sh", "-c", "true"},
		SandboxPlan{NetworkMode: "isolated", HostStateWrites: []string{state}}, ProcessMaterial{env: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Args, "-DSTATE_WRITE_0="+state) {
		t.Fatalf("the declared state subpath must reach the policy: %v", cmd.Args)
	}
	// A mount-backed plan is still refused: in-place writes are the only form
	// seatbelt accepts.
	if _, err := (seatbeltBackend{}).Command(context.Background(), []string{"/bin/sh", "-c", "true"},
		SandboxPlan{OverlayMounts: []SandboxOverlayMount{{Source: state, Target: state}}}, ProcessMaterial{}); !errors.Is(err, errSandboxUnavailable) {
		t.Fatalf("seatbelt must still refuse overlay mounts, got %v", err)
	}
}

// /diag must report the state commands actually run in: with exec_sandbox
// disabled nothing is sandbox-runnable, whatever the host could enforce.
func TestCredentialProfileDiagnosticsReportADisabledSandbox(t *testing.T) {
	base := t.TempDir()
	home, _, _ := fakeGcloudHome(t, base)
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte(ssoProfileConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	installDiagnosticSnapshot(t, home)
	withExecSandboxPolicy(t, false, false, false)
	// Both a profile with in-place state and one without report the policy.
	for _, id := range []string{"aws", "gcloud"} {
		diagnostic := diagnosticFor(t, CredentialProfileDiagnostics(), id)
		if diagnostic.Runnable || !strings.Contains(diagnostic.Reason, "disabled") ||
			len(diagnostic.InPlaceWrites)+len(diagnostic.OverlayTargets) != 0 {
			t.Fatalf("%s must report the disabled sandbox: %+v", id, diagnostic)
		}
	}
}

func installDiagnosticSnapshot(t *testing.T, home string) {
	t.Helper()
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	registry.Install([]string{"PATH=/usr/bin:/bin", "HOME=" + home}, "inherited", "principal-diag", nil)
}

func diagnosticFor(t *testing.T, diagnostics []CredentialProfileDiagnostic, id string) CredentialProfileDiagnostic {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Profile == id {
			return diagnostic
		}
	}
	t.Fatalf("no diagnostic for %s in %+v", id, diagnostics)
	return CredentialProfileDiagnostic{}
}

// /diag reports, per credential profile on this host, whether this platform
// can run it sandboxed and which host paths a sandboxed run would write —
// paths only, never contents.
func TestCredentialProfileDiagnosticsShowRunnabilityAndWritablePaths(t *testing.T) {
	base := t.TempDir()
	home, _, _ := fakeGcloudHome(t, base)
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte(ssoProfileConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	installDiagnosticSnapshot(t, home)
	withExecSandboxPolicy(t, true, false, false)

	diagnostics := CredentialProfileDiagnostics()
	aws := diagnosticFor(t, diagnostics, "aws")
	if !aws.HostState {
		t.Fatal("the host's AWS state must be reported")
	}
	if aws.Runnable != ExecSandboxAvailable() {
		t.Fatalf("aws runnable=%v on a host whose isolation availability is %v (%s)", aws.Runnable, ExecSandboxAvailable(), aws.Reason)
	}
	root := canonicalPath(t, filepath.Join(home, ".aws"))
	want := []string{filepath.Join(root, "sso", "cache"), filepath.Join(root, "cli", "cache")}
	switch {
	case !ExecSandboxAvailable():
		if len(aws.InPlaceWrites)+len(aws.OverlayTargets) != 0 || aws.Reason == "" {
			t.Fatalf("without an isolation backend nothing is writable and the reason is stated: %+v", aws)
		}
	case writesHostStateInPlace():
		if strings.Join(aws.InPlaceWrites, "|") != strings.Join(want, "|") || len(aws.OverlayTargets) != 0 {
			t.Fatalf("seatbelt must report the in-place subpaths: %+v", aws)
		}
	default:
		if len(aws.InPlaceWrites) != 0 || len(aws.OverlayTargets) != 2 {
			t.Fatalf("a mounting backend must report private overlays, not host writes: %+v", aws)
		}
	}
	gcloud := diagnosticFor(t, diagnostics, "gcloud")
	if !gcloud.HostState || len(gcloud.InPlaceWrites) != 0 || len(gcloud.OverlayTargets) != 0 {
		t.Fatalf("gcloud state is copied into the run overlay; no host path is writable: %+v", gcloud)
	}
	if gh := diagnosticFor(t, diagnostics, "gh"); gh.HostState {
		t.Fatalf("a profile without host state must say so: %+v", gh)
	}
	for _, diagnostic := range diagnostics {
		if strings.Contains(strings.Join(append(append([]string{diagnostic.Reason}, diagnostic.InPlaceWrites...), diagnostic.OverlayTargets...), " "), "awsapps") {
			t.Fatalf("diagnostics leaked configuration content: %+v", diagnostic)
		}
	}

	// The input that must change the result on an in-place backend: a cache
	// that resolves outside its root cannot be expressed as a subpath write.
	if !writesHostStateInPlace() || !ExecSandboxAvailable() {
		return
	}
	if err := os.MkdirAll(filepath.Join(home, ".aws", "sso"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home, ".aws", "sso", "cache")); err != nil {
		t.Fatal(err)
	}
	escaped := diagnosticFor(t, CredentialProfileDiagnostics(), "aws")
	if escaped.Runnable || !strings.Contains(escaped.Reason, "outside") || len(escaped.InPlaceWrites) != 0 {
		t.Fatalf("an unexpressible profile must report why it cannot run sandboxed: %+v", escaped)
	}
}
