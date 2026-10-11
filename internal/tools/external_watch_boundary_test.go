package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// preflightWatchRegistration runs the registration preflight for a watch whose
// funnel approved the given boundary, under a Run whose frozen policy is
// scopePolicy (nil: the process policy) and a daemon whose current policy is
// the one withExecSandboxPolicy installed. It returns the marker the check
// creates when it runs, and whether it ran on the host.
func preflightWatchRegistration(t *testing.T, name string, approved SandboxMode, approvedReason string, scopePolicy *ExecSandboxPolicy) (marker string, hostRun bool, err error) {
	t.Helper()
	dir := canonicalPath(t, t.TempDir())
	marker = filepath.Join(dir, "preflight-ran")
	command := "touch " + marker + " && printf PENDING"
	tenant := "tenant-watch-" + name
	t.Cleanup(SetExecutionScope(tenant, ExecutionScope{
		TenantID: tenant, PersonID: "person-watch", WorkspaceRoot: dir, AllowedRoots: []string{dir},
		SandboxPolicy: scopePolicy,
	}))
	args := map[string]interface{}{
		"_tenant_id": tenant, "_tool_name": "watch_external", "command": command,
		"_effective_sandbox_mode": string(approved), effectiveSandboxReasonArg: approvedReason,
	}
	observation := &externalWatchPreflightObservation{}
	_, err = preflightExternalWatchPatterns(args, command, dir, externalWatchPreflightPatterns{
		Success: "SUCCEEDED", Failure: "FAILED", Observation: observation,
	}, 10)
	return marker, observation.HostNetworkShared, err
}

// L2: a watch registers only if its unattended polls will run under the
// boundary the registration was approved for. The preflight could run on the
// host after an approved auto→host decision while every later poll resolved
// to isolation and was refused (or the reverse), so the watch registered but
// could never poll — or polled outside what was approved.
//
// The polls' policy is the one the registering Run froze onto the watch,
// narrowed — never widened — by the daemon's current one. The registration is
// held to that policy, not to the daemon's alone.
func TestWatchRegistrationRunsUnderTheBoundaryItsPollsWillUse(t *testing.T) {
	for _, tc := range []struct {
		name           string
		approved       SandboxMode
		approvedReason string
		processEnabled bool
		scopePolicy    *ExecSandboxPolicy
	}{
		{
			name: "approved-host-polls-isolated", approved: SandboxHost,
			approvedReason: "seatbelt cannot enforce the isolated plan: aws: ~/.aws/sso/cache resolves outside its declared state root",
			processEnabled: true,
		},
		// The daemon isolates what the registering Run did not: the polls are
		// narrowed to isolation, so a host approval no longer describes them.
		{
			name: "approved-host-polls-narrowed-by-the-daemon", approved: SandboxHost,
			approvedReason: "exec sandbox disabled by configuration",
			processEnabled: true, scopePolicy: &ExecSandboxPolicy{Enabled: false},
		},
		// The same invariant the other way round: approved for isolation, while
		// the policy the polls run under (the Run's, then the daemon's) does not
		// isolate.
		{
			name: "approved-isolated-polls-host", approved: SandboxIsolated,
			processEnabled: false, scopePolicy: &ExecSandboxPolicy{Enabled: false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireIsolationBackend(t)
			withExecSandboxPolicy(t, tc.processEnabled, false, true)
			marker, _, err := preflightWatchRegistration(t, tc.name, tc.approved, tc.approvedReason, tc.scopePolicy)
			if refusal := requireNotDispatched(t, err); refusal.ToolErrorCode() != "watch_boundary_mismatch" {
				t.Fatalf("want the registration-boundary refusal, got %s: %q", refusal.ToolErrorCode(), refusal.ModelSafeMessage())
			}
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatal("the preflight ran under a boundary its polls will not use")
			}
		})
	}

	// The constraints that must change the result: the polls share the
	// registration's boundary, so the preflight runs under it.
	for _, tc := range []struct {
		name           string
		approved       SandboxMode
		approvedReason string
		processEnabled bool
		scopePolicy    *ExecSandboxPolicy
		wantHost       bool
	}{
		// Approved for the host, and the polls run on the host too.
		{
			name: "approved-host-polls-host", approved: SandboxHost,
			approvedReason: "exec sandbox disabled by configuration", wantHost: true,
		},
		// The Run's frozen policy isolates while the daemon's current one no
		// longer does. A configuration change after registration never widens a
		// poll, so its polls inherit the Run's isolation — the boundary the
		// registration was approved for — and the preflight runs inside it.
		{
			name: "approved-isolated-polls-keep-the-runs-isolation", approved: SandboxIsolated,
			processEnabled: false, scopePolicy: &ExecSandboxPolicy{Enabled: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireIsolationBackend(t)
			withExecSandboxPolicy(t, tc.processEnabled, false, true)
			marker, hostRun, err := preflightWatchRegistration(t, tc.name, tc.approved, tc.approvedReason, tc.scopePolicy)
			if err != nil {
				t.Fatalf("a registration whose polls share its boundary must preflight: %v", err)
			}
			if _, statErr := os.Stat(marker); statErr != nil {
				t.Fatal("the matching preflight did not run")
			}
			if hostRun != tc.wantHost {
				t.Fatalf("the preflight ran on the host = %v, want %v", hostRun, tc.wantHost)
			}
		})
	}
}
