package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools/sandbox"
)

// An isolation backend that disappeared after a watch registered.
type goneBackend struct{ SandboxBackend }

func (goneBackend) Available() bool { return false }

func withIsolationBackendGone(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		previous := backendBubblewrap
		backendBubblewrap = goneBackend{previous}
		t.Cleanup(func() { backendBubblewrap = previous })
	case "darwin":
		previous := backendSeatbelt
		backendSeatbelt = goneBackend{previous}
		t.Cleanup(func() { backendSeatbelt = previous })
	default:
		t.Skip("no isolation backend on this platform")
	}
}

// pollMarker polls a check that touches a marker and prints PENDING from the
// binding alone, and reports whether the marker appeared.
func pollMarker(t *testing.T, binding executionenv.Binding) (ExecutionResult, error, bool) {
	t.Helper()
	dir := canonicalPath(t, t.TempDir())
	marker := filepath.Join(dir, "poll.marker")
	binding.Version, binding.ID = executionenv.BindingVersion, "binding:pin"
	binding.TenantID, binding.PersonID, binding.WorkspaceID = "tenant-durable", "person-durable", "ws-durable"
	binding.TrustLevel = executionenv.TrustUntrusted
	result, err := RunDurableCheck(WithExecutionRuntime(t.Context(), NewExecutionRuntime("durable-pin", t.TempDir(), sandbox.PrivateState{}, nil)), "touch "+marker+" && printf PENDING", dir, true, DurableExecutionScope{
		TenantID: "tenant-durable", PersonID: "person-durable", WorkspaceID: "ws-durable", Binding: binding,
	})
	_, statErr := os.Stat(marker)
	return result, err, statErr == nil
}

func installPollRegistry(t *testing.T) {
	t.Helper()
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	registry.Install([]string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}, "inherited", "principal-durable", nil)
}

// The registration's approval resolved a boundary, and the poll that follows
// has no approval of its own. A poll registered as contained must never become
// a host run because the isolation backend is gone; the boundary it was
// approved for rides on its binding.
func TestDurablePollIsPinnedToTheBoundaryItsRegistrationWasApprovedFor(t *testing.T) {
	requireIsolationBackend(t)
	installPollRegistry(t)
	frozen := &executionenv.SandboxPolicy{Enabled: true}

	for _, tc := range []struct {
		name     string
		pin      string
		frozen   *executionenv.SandboxPolicy
		enabled  bool
		required bool
		gone     bool
		wantCode string
		wantMode SandboxMode
	}{
		// The default configuration (enabled, not required): the isolation
		// backend is gone, so optional isolation would resolve to the host.
		{name: "isolated-pin-backend-gone", pin: "isolated", frozen: frozen, enabled: true, gone: true, wantCode: "sandbox_boundary_unavailable"},
		// The same after the operator also switched the sandbox off.
		{name: "isolated-pin-backend-gone-and-sandbox-off", pin: "isolated", frozen: frozen, gone: true, wantCode: "sandbox_boundary_unavailable"},
		{name: "isolated-pin-backend-present", pin: "isolated", frozen: frozen, enabled: true, wantMode: SandboxIsolated},
		// A boundary this binary cannot interpret is not guessed at.
		{name: "unrecognised-pin", pin: "remote", frozen: frozen, enabled: true, wantCode: "sandbox_boundary_unavailable"},
		// A host approval survives the operator enabling the sandbox, and is
		// refused once the policy requires isolation.
		{name: "host-pin-sandbox-enabled-later", pin: "host", frozen: &executionenv.SandboxPolicy{}, enabled: true, wantMode: SandboxHost},
		{name: "host-pin-required-later", pin: "host", frozen: &executionenv.SandboxPolicy{}, enabled: true, required: true, wantCode: "sandbox_isolation_unavailable"},
		{name: "host-pin-unchanged", pin: "host", frozen: &executionenv.SandboxPolicy{}, wantMode: SandboxHost},
		// A row registered before the boundary was recorded behaves as it did:
		// the policy alone decides, so a vanished backend means the host and a
		// newly enabled sandbox narrows it.
		{name: "no-pin-backend-gone", frozen: frozen, enabled: true, gone: true, wantMode: SandboxHost},
		{name: "no-pin-sandbox-enabled-later", frozen: &executionenv.SandboxPolicy{}, enabled: true, wantMode: SandboxIsolated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withExecSandboxPolicy(t, tc.enabled, tc.required, true)
			if tc.gone {
				withIsolationBackendGone(t)
			}
			result, err, ran := pollMarker(t, executionenv.Binding{SandboxPolicy: tc.frozen, ApprovedBoundary: tc.pin})
			if tc.wantCode != "" {
				if refusal := requireNotDispatched(t, err); refusal.ToolErrorCode() != tc.wantCode {
					t.Fatalf("want %s, got %s: %v", tc.wantCode, refusal.ToolErrorCode(), err)
				}
				if ran || result.Started || result.FailureClass != "environment" {
					t.Fatalf("a poll whose approved boundary is unavailable must not start: ran=%v started=%v class=%q", ran, result.Started, result.FailureClass)
				}
				return
			}
			if err != nil || !ran || strings.TrimSpace(result.Output) != "PENDING" {
				t.Fatalf("poll: %v ran=%v (%q)", err, ran, result.Output)
			}
			if result.Plan.Mode != tc.wantMode {
				t.Fatalf("poll ran %q, want %q", result.Plan.Mode, tc.wantMode)
			}
		})
	}
}
