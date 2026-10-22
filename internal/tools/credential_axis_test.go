package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
)

// credentialAxisRun installs one smart-mode run with a real lease: a runtime
// root outside /tmp, an environment snapshot whose HOME is the fixture home, and
// a lease bound to it, which is everything execution preparation reads. The
// judge escalates, so a call that is not contained reaches the person.
type credentialAxisRun struct {
	tenant    string
	workspace string
	asks      int
	judge     *fakeJudge
}

func newCredentialAxisRun(t *testing.T, name, home, trust string, capabilities ...string) *credentialAxisRun {
	t.Helper()
	return newCredentialAxisRunWith(t, name, home, trust, nil, capabilities...)
}

func newCredentialAxisRunWith(t *testing.T, name, home, trust string, adjust func(*ExecutionScope), capabilities ...string) *credentialAxisRun {
	t.Helper()
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory for a runtime root outside /tmp")
	}
	root := filepath.Join(realHome, ".selfmind-test-runtime", "credential-axis-"+name)
	if err := executionenv.SetRuntimeRoot(root); err != nil {
		t.Skipf("runtime root unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	snapshot := registry.Install([]string{"PATH=/usr/bin:/bin", "HOME=" + home}, "inherited", "principal-axis", nil)
	lease := "lease-axis-" + name
	registry.BindLease(lease, snapshot.ID)

	run := &credentialAxisRun{
		tenant:    "tenant-axis-" + name,
		workspace: filepath.Join(filepath.Dir(home), "workspace"),
		judge:     &fakeJudge{reply: `{"outcome":"escalate","risk_level":"low","user_authorization":"unknown","rationale":"Needs a person."}`},
	}
	if err := os.MkdirAll(run.workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{
		TenantID: run.tenant, PersonID: "person-axis", TaskID: "task-axis", RunID: "run-axis-" + name,
		WorkspaceID: "ws-axis", WorkspaceRoot: run.workspace, AllowedRoots: []string{run.workspace},
		LeaseID: lease, TrustLevel: trust, Capabilities: capabilities,
		ApprovalMode: ApprovalSmart, Judge: run.judge, Grants: newFakeGrantStore(), StandingGrants: InteractiveStandingGrants(),
		IntentSnapshot: func() RunIntentSnapshot {
			return RunIntentSnapshot{ModelAuthorization: true, RawUserText: "Build the project."}
		},
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			run.asks++
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr-axis"}, nil
		},
	}
	if adjust != nil {
		adjust(&scope)
	}
	t.Cleanup(SetExecutionScope(run.tenant, scope))
	return run
}

func (r *credentialAxisRun) args(command string) map[string]interface{} {
	return map[string]interface{}{"_tenant_id": r.tenant, "_tool_name": "terminal", "command": command, "cwd": r.workspace}
}

// call runs the production order: capability resolution, then approval.
func (r *credentialAxisRun) call(t *testing.T, command string) (ran bool, args map[string]interface{}) {
	t.Helper()
	args = r.args(command)
	exec := ExecutionCapabilityMiddleware()(SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) {
		ran = true
		return "ok", nil
	}))
	if _, err := exec(args); err != nil {
		t.Fatalf("%q: %v", command, err)
	}
	return ran, args
}

// attachesOperatorState reports what preparation actually handed the call:
// copied credential state or a read-only mapped credential file.
func attachesOperatorState(material execMaterial) bool {
	return material.CopiedStateFiles > 0 || len(material.ReadOnlyPaths) > 0
}

// operatorHome is a host home holding gcloud and AWS credential state.
func operatorHome(t *testing.T) string {
	t.Helper()
	home, _, _ := fakeGcloudHome(t, fixtureBase(t))
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "credentials"), []byte("[default]\naws_access_key_id=AKIAEXAMPLE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// The WP-F review finding: a payload that hides what it runs is prepared with
// every operator profile the host has, so in a trusted workspace
// `npm run build` received the operator's gcloud and AWS state, yet the
// containment assessment read only the named programs, called it
// credentials=none, and smart mode released it as contained. Contained means
// confined writes, no network and no attached credentials, so it must not be.
func TestOpaqueCommandReceivingOperatorStateIsNotContained(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("containment requires an enforceable sandbox on this host")
	}
	withExecSandboxPolicy(t, true, true, false)
	run := newCredentialAxisRun(t, "trusted", operatorHome(t), executionenv.TrustTrusted)

	// A decidable payload that names no credential tool is still contained.
	if ran, args := run.call(t, "go build ./..."); !ran || run.asks != 0 || run.judge.calls != 0 {
		t.Fatalf("go build must stay contained: ran=%v asks=%d judge=%d %s", ran, run.asks, run.judge.calls, assessExecContainment("terminal", args).Summary())
	}
	for _, command := range []string{"npm run build", "NODE_OPTIONS=--require=./hook.js npm run build"} {
		asks, judged := run.asks, run.judge.calls
		ran, args := run.call(t, command)
		if !ran || run.asks != asks+1 || run.judge.calls == judged {
			t.Errorf("%q received operator state and was released as contained: asks=%d judge=%d", command, run.asks-asks, run.judge.calls-judged)
		}
		if got := assessExecContainment("terminal", args); got.Credentials != containmentCredentialsSelected || got.AutoApprove() {
			t.Errorf("%q assessed %s", command, got.Summary())
		}
	}
}

// The assessment and preparation make one decision. Changed wording, paths and
// programs keep the answer; the trust decision that changes what preparation
// hands over changes the assessment with it.
func TestContainmentCredentialAxisIsPreparationsDecision(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("containment requires an enforceable sandbox on this host")
	}
	withExecSandboxPolicy(t, true, true, false)
	home := operatorHome(t)
	commands := []string{
		"npm run build", "make release", "./deploy.sh", "python3 tool.py", "find . -exec ./fix.sh {} +",
		"go build ./...", "gofmt -l .", "printf hello", "cat notes.txt",
	}
	for _, tc := range []struct {
		name         string
		trust        string
		capabilities []string
		opaqueGets   bool
	}{
		{name: "trusted", trust: executionenv.TrustTrusted, opaqueGets: true},
		{name: "untrusted", trust: executionenv.TrustUntrusted, opaqueGets: false},
		{name: "untrusted-with-credential-read", trust: executionenv.TrustUntrusted,
			capabilities: []string{executionenv.CapabilityCredentialRead}, opaqueGets: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newCredentialAxisRun(t, tc.name, home, tc.trust, tc.capabilities...)
			for _, command := range commands {
				args := run.args(command)
				annotateEffectiveSandboxMode(args)
				args["_effective_sandbox_mode"] = string(SandboxIsolated)
				material := execMaterialForArgs(args, run.workspace)
				if material.ProfileError != nil {
					t.Fatalf("%q: %v", command, material.ProfileError)
				}
				prepared := attachesOperatorState(material)
				_, opaque := execCommandProgramSet("terminal", args)
				if prepared != (opaque && tc.opaqueGets) {
					t.Fatalf("fixture: %q prepared=%v opaque=%v (profiles %v)", command, prepared, opaque, material.Profiles)
				}
				assessed := assessExecContainment("terminal", args).Credentials == containmentCredentialsSelected
				if assessed != prepared {
					t.Errorf("%q: preparation attaches operator state=%v but containment says credentials=%v",
						command, prepared, map[bool]string{true: "selected", false: "none"}[assessed])
				}
			}
		})
	}
}

// Without a run lease nothing is prepared, so nothing is attached: the axis does
// not guess from the host when preparation would not touch it.
func TestCredentialAxisWithoutLeaseFollowsPreparation(t *testing.T) {
	withExecSandboxPolicy(t, true, true, false)
	cleanup := SetExecutionScope("person-axis-nolease", ExecutionScope{
		TenantID: "tenant-axis-nolease", PersonID: "person-axis-nolease", WorkspaceID: "ws", WorkspaceRoot: t.TempDir(),
		TrustLevel: executionenv.TrustTrusted,
	})
	defer cleanup()
	args := map[string]interface{}{"_tenant_id": "person-axis-nolease", "_tool_name": "terminal", "command": "npm run build",
		"_effective_sandbox_mode": string(SandboxIsolated)}
	if material := execMaterialForArgs(args, ""); len(material.Profiles) != 0 {
		t.Fatalf("fixture: preparation without a lease applied %v", material.Profiles)
	}
	if got := assessExecContainment("terminal", args); got.Credentials != containmentCredentialsNone {
		t.Fatalf("no lease, nothing prepared, yet %s", got.Summary())
	}
	if !strings.Contains(assessExecContainment("terminal", args).Summary(), "credentials=none") {
		t.Fatal("summary must report the axis")
	}
}

// A parallel Run approves credential-bearing commands exactly as a single Run
// does (D7): cross-Run protection comes from the external-effect claim taken
// after approval, never from a parallel ask. In full-auto, neither a local
// build that receives host-inventory state nor a credential CLI's mutation
// asks in a parallel Run unless the same call asks in a single Run.
func TestParallelRunAsksAboutCredentialsAsASingleRunDoes(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("containment requires an enforceable sandbox on this host")
	}
	withExecSandboxPolicy(t, true, true, false)
	asks := map[bool][]int{}
	for _, parallel := range []bool{false, true} {
		run := newCredentialAxisRunWith(t, fmt.Sprintf("parallel-%v", parallel), operatorHome(t), executionenv.TrustTrusted, func(scope *ExecutionScope) {
			scope.ApprovalMode = ApprovalFullAuto
			scope.ParallelWork = parallel
		})
		for _, command := range []string{"npm run build", "kubectl delete pod web"} {
			before := run.asks
			if ran, args := run.call(t, command); !ran {
				t.Fatalf("parallel=%v: %q did not run: %s", parallel, command, assessExecContainment("terminal", args).Summary())
			}
			asks[parallel] = append(asks[parallel], run.asks-before)
		}
	}
	if !slices.Equal(asks[true], asks[false]) || asks[true][0] != 0 {
		t.Fatalf("asks in a parallel run %v differ from a single run %v, or a local build asked", asks[true], asks[false])
	}
}
