package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools/sandbox"
)

const judgeApproves = `{"outcome":"approve","risk_level":"low","user_authorization":"high","rationale":"The person asked to run the project tooling."}`

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// requireNotDispatched asserts the typed refusal a model must receive when a
// call never started: a preparation-phase failure with no effect.
func requireNotDispatched(t *testing.T, err error) *stableToolError {
	t.Helper()
	var classified *stableToolError
	if !errors.As(err, &classified) || classified.ToolEffectState() != "not_dispatched" || classified.ToolStateChanged() {
		t.Fatalf("want a typed not_dispatched refusal, got %v", err)
	}
	return classified
}

// The approval request and the judge prompt must describe the boundary that
// will actually be enforced. On a backend that writes credential state in
// place, the writable host subpaths are part of that boundary.
func TestApprovalDescribesTheCredentialSubpathsTheSandboxWillWrite(t *testing.T) {
	for _, tc := range []struct{ name, config string }{
		{"sso", ssoProfileConfig},
		// Another configuration that needs writable state.
		{"role", roleProfileConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireIsolationBackend(t)
			withExecSandboxPolicy(t, true, false, false)
			base := fixtureBase(t)
			home := awsStateHome(t, base, tc.config)
			run := newBoundaryRun(t, "describe-"+tc.name, home, writeStub(t, base, "aws", awsCredentialStateStub),
				executionenv.TrustTrusted, ApprovalSmart, nil)
			if err := os.WriteFile(filepath.Join(run.workspace, "deploy.sh"), []byte("#!/bin/sh\naws --profile ops sts get-caller-identity\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			run.approve = false
			if _, _, err := run.call("./deploy.sh", nil); err == nil {
				t.Fatal("a declined ask must not run")
			}
			if run.judge.calls != 1 || len(run.asks) != 1 {
				t.Fatalf("an opaque credentialed script is reviewed then asked: judge=%d asks=%d", run.judge.calls, len(run.asks))
			}
			containment := run.asks[0].Containment
			if !strings.Contains(containment, "filesystem=isolated") || !strings.Contains(containment, "credentials=selected") {
				t.Fatalf("the ask must describe the isolated, credentialed boundary: %q", containment)
			}
			if !strings.Contains(run.judge.lastArg, "Execution containment: "+containment) {
				t.Fatalf("the judge must review the same boundary the person is shown:\n%s", run.judge.lastArg)
			}
			awsRoot := canonicalPath(t, filepath.Join(home, ".aws"))
			var named []string
			if _, writes, found := strings.Cut(containment, "host_credential_writes="); found {
				writes, _, _ = strings.Cut(writes, ", ")
				named = strings.Split(writes, ";")
			}
			want := []string(nil)
			if writesHostStateInPlace() {
				want = []string{filepath.Join(awsRoot, "sso", "cache"), filepath.Join(awsRoot, "cli", "cache")}
			}
			if strings.Join(named, "|") != strings.Join(want, "|") {
				t.Fatalf("the boundary names writable host paths %v, want exactly %v: %q", named, want, containment)
			}
		})
	}
}

// No approval may be requested for an isolated plan the backend cannot
// enforce. A Run that requires isolation on a host that cannot provide it is
// refused before the judge and the person are consulted.
func TestUnenforceableRequiredIsolationIsRefusedBeforeAnyApproval(t *testing.T) {
	base := fixtureBase(t)
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	withExecSandboxPolicy(t, false, true, false)
	run := newBoundaryRun(t, "required-disabled", home, "", executionenv.TrustTrusted, ApprovalSmart, nil)
	args, out, err := run.call("printf hello > marker.txt", nil)
	requireNotDispatched(t, err)
	if run.judge.calls != 0 || len(run.asks) != 0 {
		t.Fatalf("an unenforceable plan was sent for approval: judge=%d asks=%d", run.judge.calls, len(run.asks))
	}
	if mode := stringArg(args, "_sandbox_mode"); mode != "" {
		t.Fatalf("a refused call recorded sandbox mode %q (%s)", mode, out)
	}
	if _, statErr := os.Stat(filepath.Join(run.workspace, "marker.txt")); statErr == nil {
		t.Fatal("the refused command ran")
	}

	// The input that must change the result: the same Run on a host that can
	// enforce the plan is contained and released without any ask.
	if !ExecSandboxAvailable() {
		return
	}
	withExecSandboxPolicy(t, true, true, false)
	args, out, err = run.call("printf hello > marker.txt", nil)
	if err != nil || stringArg(args, "_sandbox_mode") != string(SandboxIsolated) {
		t.Fatalf("an enforceable required plan must run isolated: %v (%s)", err, out)
	}
	if run.judge.calls != 0 || len(run.asks) != 0 {
		t.Fatalf("a contained call must not ask: judge=%d asks=%d", run.judge.calls, len(run.asks))
	}
}

// symlinkedCacheHome makes the declared SSO cache a symlink that resolves
// outside the declared state root: no subpath write can express it.
func symlinkedCacheHome(t *testing.T, base, config, link string) (home, outside string) {
	t.Helper()
	home = awsStateHome(t, base, config)
	outside = filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, ".aws", filepath.FromSlash(link))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	return home, outside
}

// Feasibility is decided before the judge and the person. A plan whose
// credential state cannot be expressed as declared-subpath writes is refused
// with its specific reason when isolation is required, and is presented as
// host execution — never as an isolated call — when isolation is optional.
func TestInPlaceStateThatEscapesItsRootIsDecidedBeforeApproval(t *testing.T) {
	for _, tc := range []struct{ name, link string }{
		{"cache-link", "sso/cache"},
		// The same escape one directory up.
		{"parent-link", "sso"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireIsolationBackend(t)
			if !writesHostStateInPlace() {
				t.Skip("a mounting backend shadows the host path, so the link cannot escape")
			}
			base := fixtureBase(t)
			home, outside := symlinkedCacheHome(t, base, ssoProfileConfig, tc.link)
			bin := writeStub(t, base, "aws", awsCredentialStateStub)

			withExecSandboxPolicy(t, true, true, false)
			run := newBoundaryRun(t, "escape-required-"+tc.name, home, bin, executionenv.TrustTrusted, ApprovalSmart, nil)
			if err := os.WriteFile(filepath.Join(run.workspace, "deploy.sh"), []byte("#!/bin/sh\naws --profile sso-dev sts get-caller-identity\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			_, _, err := run.call("./deploy.sh", nil)
			classified := requireNotDispatched(t, err)
			if run.judge.calls != 0 || len(run.asks) != 0 {
				t.Fatalf("an unenforceable plan was sent for approval: judge=%d asks=%d", run.judge.calls, len(run.asks))
			}
			if !strings.Contains(classified.ModelSafeMessage(), "outside") {
				t.Fatalf("the refusal must name the specific reason: %q", classified.ModelSafeMessage())
			}
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Fatalf("the escaping target was written: %v", entries)
			}

			// Optional isolation: the call is presented, judged and asked as
			// host execution with the reason, never released as isolated.
			withExecSandboxPolicy(t, true, false, false)
			auto := newBoundaryRun(t, "escape-auto-"+tc.name, home, bin, executionenv.TrustTrusted, ApprovalSmart, nil)
			if err := os.WriteFile(filepath.Join(auto.workspace, "deploy.sh"), []byte("#!/bin/sh\necho host deploy\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			args, out, err := auto.call("./deploy.sh", nil)
			if err != nil {
				t.Fatalf("an approved host call must run: %v (%s)", err, out)
			}
			if len(auto.asks) != 1 || !strings.Contains(auto.asks[0].Containment, "filesystem=host") {
				t.Fatalf("the ask must present the host boundary: %+v", auto.asks)
			}
			effective, _ := auto.asks[0].Args["effective_sandbox"].(string)
			if !strings.Contains(effective, "host") || !strings.Contains(effective, "outside") {
				t.Fatalf("the ask must say why isolation is unavailable: %q", effective)
			}
			if mode := stringArg(args, "_sandbox_mode"); mode != string(SandboxHost) {
				t.Fatalf("the approved boundary was host, ran %q", mode)
			}
		})
	}
}

// Boundary pinning: execution uses exactly the boundary that was approved.
// The approval assumed isolation; when the backend cannot enforce it at
// dispatch, the call is not dispatched and never falls back to the host —
// whichever authority released it and at capacity 1.
func TestApprovedIsolationIsNeverWidenedToTheHostAtDispatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mode  ApprovalMode
		judge string
		grant bool
		state bool
	}{
		{name: "human", mode: ApprovalOnRequest},
		{name: "stored-grant", mode: ApprovalOnRequest, grant: true},
		{name: "judge", mode: ApprovalSmart, judge: judgeApproves, state: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireIsolationBackend(t)
			withExecSandboxPolicy(t, true, false, false)
			base := fixtureBase(t)
			home := filepath.Join(base, "home")
			if tc.state {
				// Operator state on the host makes an opaque script
				// credential-bearing, so smart mode sends it to the judge.
				home, _, _ = fakeGcloudHome(t, base)
			} else if err := os.MkdirAll(home, 0o755); err != nil {
				t.Fatal(err)
			}
			policy := &ExecSandboxPolicy{Enabled: true}
			grants := newFakeGrantStore()
			run := newBoundaryRun(t, "pin-"+tc.name, home, "", executionenv.TrustTrusted, tc.mode, func(scope *ExecutionScope) {
				scope.SandboxPolicy = policy
				scope.Grants = grants
			})
			if tc.judge != "" {
				run.judge.reply = tc.judge
			}
			command := "touch dispatched.marker"
			if tc.state {
				command = "./mark.sh"
				if err := os.WriteFile(filepath.Join(run.workspace, "mark.sh"), []byte("#!/bin/sh\ntouch dispatched.marker\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.grant {
				probe := map[string]interface{}{"_tenant_id": run.tenant, "_tool_name": "terminal", "command": command, "cwd": run.workspace}
				annotateEffectiveSandboxMode(probe)
				scope, _ := currentExecutionScopeAny(probe)
				key := approvalPatternKeyForScope("terminal", probe, "", scope, true)
				if key == "" || !strings.Contains(key, "class=") {
					t.Fatalf("fixture: %q must mint a sandboxed class, got %q", command, key)
				}
				grants.granted["person|person-boundary|"+key] = true
			}
			// The backend loses the ability to enforce the approved plan between
			// approval and dispatch.
			args, out, err := run.call(command, func() { policy.Enabled = false })
			if refusal := requireNotDispatched(t, err); refusal.ToolErrorCode() != "sandbox_boundary_unavailable" ||
				!strings.Contains(refusal.ModelSafeMessage(), "did not run on the host") {
				t.Fatalf("the model must be told the approved boundary was unavailable: %s %q", refusal.ToolErrorCode(), refusal.ModelSafeMessage())
			}
			switch {
			case tc.grant && len(run.asks) != 0:
				t.Fatalf("the stored grant should have released the call, asks=%d", len(run.asks))
			case tc.judge != "" && (run.judge.calls != 1 || len(run.asks) != 0):
				t.Fatalf("the judge should have released the call: judge=%d asks=%d", run.judge.calls, len(run.asks))
			case !tc.grant && tc.judge == "" && len(run.asks) != 1:
				t.Fatalf("the person should have released the call, asks=%d", len(run.asks))
			}
			if mode := stringArg(args, "_sandbox_mode"); mode == string(SandboxHost) {
				t.Fatalf("an isolated approval ran on the host: %s", out)
			}
			if _, statErr := os.Stat(filepath.Join(run.workspace, "dispatched.marker")); statErr == nil {
				t.Fatal("the command ran although its approved boundary was unavailable")
			}
		})
	}
}

// A daemon-owned check has no approval at poll time, so a plan its backend
// cannot enforce must never become a host run either.
func TestDurableCheckNeverFallsBackToTheHost(t *testing.T) {
	requireIsolationBackend(t)
	if !writesHostStateInPlace() {
		t.Skip("a mounting backend can enforce this plan")
	}
	withExecSandboxPolicy(t, true, false, true)
	base := fixtureBase(t)
	home, outside := symlinkedCacheHome(t, base, ssoProfileConfig, "sso/cache")
	bin := writeStub(t, base, "aws", awsCredentialStateStub)
	realHome, _ := os.UserHomeDir()
	runtimeRoot := filepath.Join(realHome, ".selfmind-test-runtime", "durable-"+t.Name())
	if err := executionenv.SetRuntimeRoot(runtimeRoot); err != nil {
		t.Skipf("runtime root unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	registry.Install([]string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home}, "inherited", "principal-durable", nil)
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := WithExecutionRuntime(t.Context(), NewExecutionRuntime(t.TempDir(), runtimeRoot, sandbox.PrivateState{}, nil))
	result, err := RunDurableCheck(ctx, "aws --profile sso-dev sts get-caller-identity", workspace, true, DurableExecutionScope{
		ScratchKey: "watch-durable", TenantID: "tenant-durable", PersonID: "person-durable",
		WorkspaceID: "ws-durable", TrustLevel: executionenv.TrustTrusted,
	})
	if err == nil || result.Started {
		t.Fatalf("an unenforceable durable plan must not start: started=%v err=%v out=%s", result.Started, err, result.Output)
	}
	if refusal := requireNotDispatched(t, err); refusal.ToolErrorCode() != "sandbox_plan_unsupported" || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("the refusal must name the unexpressible state: %v", err)
	}
	if result.Plan.Mode == SandboxHost || result.FailureClass != "environment" {
		t.Fatalf("a durable check fell back to the host or lost its environment class: %+v", result)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("the escaping target was written: %v", entries)
	}
}
