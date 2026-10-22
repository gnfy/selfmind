package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/tools/sandbox"
)

// npmDeployStub stands in for `npm run deploy`: a deploy script whose AWS call
// refreshes the SSO token cache.
const npmDeployStub = "#!/bin/sh\n" +
	"[ \"$1 $2\" = \"run deploy\" ] || { echo \"unknown: $*\"; exit 2; }\n" +
	"mkdir -p \"$HOME/.aws/sso/cache\" 2>/dev/null\n" +
	"if printf '{\"token\":\"deploy\"}' > \"$HOME/.aws/sso/cache/deploy.json\" 2>/dev/null; then echo 'cache written'; else echo 'cache write denied'; fi\n" +
	"echo deployed\n"

const staticAWSConfig = "[default]\nregion=us-east-1\n"

func requireInPlaceBackend(t *testing.T) {
	t.Helper()
	requireIsolationBackend(t)
	if !writesHostStateInPlace() {
		t.Skip("a mounting backend never writes host credential state in place")
	}
}

func writeAWSConfig(t *testing.T, home, config string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The review probe (M1): on-request mode, a trusted workspace, `npm run deploy`
// remembered while the AWS configuration was static (no host writes shown). The
// configuration then gained an SSO profile, and the second call ran with no ask
// and wrote the operator's real SSO cache. A remembered class, rule or stored
// grant covers the boundary it was approved for, including its in-place
// credential writes: a changed set asks again, an unchanged one does not.
func TestRememberedApprovalNeverCoversNewHostCredentialWrites(t *testing.T) {
	remember := func(scope string, rule bool) func(ToolApprovalRequest) ToolApprovalDecision {
		return func(request ToolApprovalRequest) ToolApprovalDecision {
			decision := ToolApprovalDecision{Approved: true, ApprovalID: "apr-remember", Scope: scope}
			if rule && len(request.RuleCandidates) > 0 {
				decision.GrantKey = request.RuleCandidates[0].Key
			}
			return decision
		}
	}
	for _, tc := range []struct {
		name   string
		decide func(ToolApprovalRequest) ToolApprovalDecision
		stored bool
	}{
		{name: "run-class", decide: remember("run", false)},
		// The same invariant through a stored workspace class and a run rule.
		{name: "workspace-class", decide: remember("workspace", false), stored: true},
		{name: "run-rule", decide: remember("run", true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireInPlaceBackend(t)
			withExecSandboxPolicy(t, true, false, false)
			base := fixtureBase(t)
			home := awsStateHome(t, base, staticAWSConfig)
			bin := writeStub(t, base, "npm", npmDeployStub)
			var store *control.Store
			if tc.stored {
				var err error
				if store, err = control.OpenStore(t.TempDir()); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
			}
			run := newBoundaryRun(t, "grant-"+tc.name, home, bin, executionenv.TrustTrusted, ApprovalOnRequest, func(scope *ExecutionScope) {
				if store != nil {
					scope.Grants = store
				}
			})
			run.decide = tc.decide

			if _, out, err := run.call("npm run deploy", nil); err != nil || len(run.asks) != 1 {
				t.Fatalf("first call must ask once and run: asks=%d err=%v (%s)", len(run.asks), err, out)
			}
			if strings.Contains(run.asks[0].Containment, "host_credential_writes") {
				t.Fatalf("fixture: a static configuration needs no host writes: %q", run.asks[0].Containment)
			}
			if tc.name == "run-rule" && len(run.asks[0].RuleCandidates) == 0 {
				t.Fatal("fixture: the first ask must offer a rule")
			}

			// The same command now needs the SSO token cache written in place.
			writeAWSConfig(t, home, ssoProfileConfig)
			if _, out, err := run.call("npm run deploy", nil); err != nil {
				t.Fatalf("the re-approved call must run: %v (%s)", err, out)
			}
			if len(run.asks) != 2 {
				t.Fatalf("a remembered approval released new host credential writes without asking (asks=%d)", len(run.asks))
			}
			if !strings.Contains(run.asks[1].Containment, "host_credential_writes=") {
				t.Fatalf("the new ask must name the writes: %q", run.asks[1].Containment)
			}

			// The constraint that must change the result: remembered WITH this
			// write set, the next identical call is released.
			if _, out, err := run.call("npm run deploy", nil); err != nil || len(run.asks) != 2 {
				t.Fatalf("an approval for the same write set must be reused: asks=%d err=%v (%s)", len(run.asks), err, out)
			}
		})
	}
}

// The approval covered the writes it was shown; dispatch refuses a plan whose
// in-place writes are not a subset of them, whatever released the call.
func TestDispatchRefusesHostCredentialWritesTheApprovalDidNotCover(t *testing.T) {
	for _, tc := range []struct {
		name    string
		before  func(t *testing.T, home string)
		between func(t *testing.T, home string)
	}{
		{
			name:    "config-gains-sso",
			before:  func(t *testing.T, home string) { writeAWSConfig(t, home, staticAWSConfig) },
			between: func(t *testing.T, home string) { writeAWSConfig(t, home, ssoProfileConfig) },
		},
		{
			// Another precondition with the same invariant: the state root itself
			// appears between approval and dispatch.
			name:    "root-appears",
			before:  func(t *testing.T, home string) {},
			between: func(t *testing.T, home string) { writeAWSConfig(t, home, roleProfileConfig) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireInPlaceBackend(t)
			withExecSandboxPolicy(t, true, false, false)
			base := fixtureBase(t)
			home := filepath.Join(base, "home")
			if err := os.MkdirAll(home, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.before(t, home)
			bin := writeStub(t, base, "npm", npmDeployStub)
			run := newBoundaryRun(t, "subset-"+tc.name, home, bin, executionenv.TrustTrusted, ApprovalOnRequest, nil)
			_, out, err := run.call("npm run deploy", func() { tc.between(t, home) })
			refusal := requireNotDispatched(t, err)
			if refusal.ToolErrorCode() != "approved_host_writes_changed" {
				t.Fatalf("want the write-set refusal, got %s: %q (%s)", refusal.ToolErrorCode(), refusal.ModelSafeMessage(), out)
			}
			if len(run.asks) != 1 || strings.Contains(run.asks[0].Containment, "host_credential_writes") {
				t.Fatalf("fixture: the approval must have covered no host writes: %+v", run.asks)
			}
			if _, statErr := os.Stat(filepath.Join(home, ".aws", "sso", "cache")); statErr == nil {
				t.Fatal("state the approval never covered was written or created on the host")
			}
		})
	}

	// The constraint that must change the result: nothing changes between
	// approval and dispatch, so the approved writes cover the plan.
	requireInPlaceBackend(t)
	withExecSandboxPolicy(t, true, false, false)
	base := fixtureBase(t)
	home := awsStateHome(t, base, ssoProfileConfig)
	run := newBoundaryRun(t, "subset-unchanged", home, writeStub(t, base, "npm", npmDeployStub), executionenv.TrustTrusted, ApprovalOnRequest, nil)
	if _, out, err := run.call("npm run deploy", nil); err != nil || !strings.Contains(out, "cache written") {
		t.Fatalf("approved writes must run: %v (%s)", err, out)
	}
}

// The review probe (item 2): in an untrusted workspace, granting credential:read
// is what makes a safe observation write the operator's real SSO cache in place,
// and smart mode then releases that observation with no second ask. The
// credential:read ask is therefore the only decision the person sees, so it
// leads with the write, and the run grant it leaves covers only that write
// set: a changed set asks again, an unchanged one does not.
func TestCredentialReadAskLeadsWithAndIsBoundToItsHostWrites(t *testing.T) {
	requireInPlaceBackend(t)
	withExecSandboxPolicy(t, true, false, false)
	base := fixtureBase(t)
	staticProfile := "[default]\nregion=us-east-1\n[profile sso-dev]\nregion=us-east-1\n"
	bin := writeStub(t, base, "aws", awsCredentialStateStub)

	// Asked fresh, the only ask leads with the write it enables.
	fresh := newBoundaryRun(t, "credential-read-fresh", awsStateHome(t, filepath.Join(base, "fresh"), ssoProfileConfig), bin,
		executionenv.TrustUntrusted, ApprovalSmart, nil)
	if _, out, err := fresh.call("aws sts get-caller-identity --profile sso-dev", nil); err != nil || len(fresh.asks) != 1 {
		t.Fatalf("a safe observation must ask once, for credential access: asks=%+v err=%v (%s)", fresh.asks, err, out)
	}
	if fact := "writes credential caches in place: ~/.aws/sso/cache"; fresh.asks[0].ToolName != executionenv.CapabilityCredentialRead ||
		!strings.HasPrefix(fresh.asks[0].Reason, fact) {
		t.Fatalf("the credential ask hid the write it enables: %s %q", fresh.asks[0].ToolName, fresh.asks[0].Reason)
	}

	home := awsStateHome(t, base, staticProfile)
	run := newBoundaryRun(t, "credential-read-writes", home, bin, executionenv.TrustUntrusted, ApprovalSmart, nil)
	run.decide = func(request ToolApprovalRequest) ToolApprovalDecision {
		if request.ToolName == executionenv.CapabilityCredentialRead && request.GrantClass != "" {
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr-credential", Scope: "run"}
		}
		return ToolApprovalDecision{Approved: true, ApprovalID: "apr-once"}
	}
	credentialAsks := func() []ToolApprovalRequest {
		var asks []ToolApprovalRequest
		for _, ask := range run.asks {
			if ask.ToolName == executionenv.CapabilityCredentialRead {
				asks = append(asks, ask)
			}
		}
		return asks
	}
	const command = "aws sts get-caller-identity --profile sso-dev"

	// A configuration that writes nothing: the ask is the plain capability
	// question and its run grant names no writes.
	_, _, _ = run.call(command, nil)
	asks := credentialAsks()
	if len(asks) != 1 || strings.Contains(asks[0].Reason, "in place") || strings.Contains(asks[0].GrantClass, "in place") {
		t.Fatalf("fixture: a static profile writes no credential cache: %+v", asks)
	}

	// The same observation now refreshes the SSO token cache in place.
	writeAWSConfig(t, home, ssoProfileConfig)
	if _, out, err := run.call(command, nil); err != nil || !strings.Contains(out, "cache write ok: sso") {
		t.Fatalf("the approved observation must run: %v (%s)", err, out)
	}
	asks = credentialAsks()
	if len(asks) != 2 {
		t.Fatalf("a run grant made for no writes released in-place credential writes (credential asks=%d)", len(asks))
	}
	if want := "writes credential caches in place: ~/.aws/sso/cache"; !strings.HasPrefix(asks[1].Reason, want) {
		t.Fatalf("the credential ask must lead with the write, got %q", asks[1].Reason)
	}
	if grant := asks[1].GrantClass; !strings.Contains(grant, "while writing ~/.aws/sso/cache") || !strings.HasSuffix(grant, " in place") {
		t.Fatalf("the remember option must name the writes it covers, got %q", asks[1].GrantClass)
	}

	// The constraint that must change the result: the same write set reuses
	// the run grant.
	if _, out, err := run.call(command, nil); err != nil || len(credentialAsks()) != 2 {
		t.Fatalf("a run grant for the same writes must be reused: asks=%d err=%v (%s)", len(credentialAsks()), err, out)
	}

	// An assume-role profile churns the same two caches, so it is the same
	// write set and the same grant.
	writeAWSConfig(t, home, roleProfileConfig)
	if _, out, err := run.call("aws sts get-caller-identity --profile ops-admin", nil); err != nil || len(credentialAsks()) != 2 {
		t.Fatalf("the same write set through another profile must reuse the grant: asks=%d err=%v (%s)", len(credentialAsks()), err, out)
	}
	// Any other write set is another grant, whatever order it is listed in.
	both := []string{"/h/.aws/sso/cache", "/h/.aws/cli/cache"}
	bound := credentialCapabilityFingerprint("ws", "terminal", map[string]interface{}{"command": command}, both)
	for _, other := range [][]string{nil, {"/h/.aws/cli/cache"}, {"/h/.aws/sso/cache", "/h/.aws/cli/cache", "/h/.aws/extra"}} {
		if credentialCapabilityFingerprint("ws", "terminal", map[string]interface{}{"command": command}, other) == bound {
			t.Fatalf("a grant for %v covered %v", both, other)
		}
	}
	if credentialCapabilityFingerprint("ws", "terminal", map[string]interface{}{"command": command}, []string{both[1], both[0]}) != bound {
		t.Fatal("the same write set listed in another order must be the same grant")
	}
}

// The review probe (item 3): a durable watch poll ran with no approval record,
// so the dispatch subset check never applied and a poll went on writing the
// operator's real credential cache after the configuration grew a write the
// registration was never approved for. A poll is held to the write set its
// registration recorded on the binding: a larger plan is refused with the
// environment class before anything is created; the recorded set runs.
func TestDurablePollIsHeldToTheRegisteredHostWrites(t *testing.T) {
	requireInPlaceBackend(t)
	withExecSandboxPolicy(t, true, false, true)
	base := fixtureBase(t)
	home := awsStateHome(t, base, ssoProfileConfig)
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
	sso := filepath.Join(canonicalPath(t, home), ".aws", "sso", "cache")
	cli := filepath.Join(canonicalPath(t, home), ".aws", "cli", "cache")
	bound := func(writes ...string) executionenv.Binding {
		return executionenv.Binding{Version: executionenv.BindingVersion, ID: "binding:watch-writes",
			TenantID: "tenant-durable", PersonID: "person-durable", WorkspaceID: "ws-durable",
			TrustLevel: executionenv.TrustTrusted, HostStateWrites: writes}
	}
	poll := func(name string, binding executionenv.Binding) (ExecutionResult, error) {
		return RunDurableCheck(WithExecutionRuntime(t.Context(), NewExecutionRuntime("", runtimeRoot, sandbox.PrivateState{}, nil)), "aws sts get-caller-identity --profile sso-dev", workspace, true, DurableExecutionScope{
			ScratchKey: "watch-" + name, TenantID: "tenant-durable", PersonID: "person-durable",
			WorkspaceID: "ws-durable", TrustLevel: executionenv.TrustTrusted, Binding: binding,
		})
	}
	for _, tc := range []struct {
		name    string
		binding executionenv.Binding
	}{
		{"registered-without-writes", bound()},
		// The same invariant when the set grows by one path.
		{"registered-with-fewer-writes", bound(sso)},
		// A row that predates bindings recorded no approved writes at all.
		{"legacy-row", executionenv.Binding{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := poll(tc.name, tc.binding)
			if err == nil || result.Started {
				t.Fatalf("a poll that writes beyond its registration must not start: started=%v err=%v (%s)", result.Started, err, result.Output)
			}
			if refusal := requireNotDispatched(t, err); refusal.ToolErrorCode() != "approved_host_writes_changed" {
				t.Fatalf("want the write-set refusal, got %s: %v", refusal.ToolErrorCode(), err)
			}
			if result.FailureClass != "environment" || result.RecoveryAttempted {
				t.Fatalf("the refusal must park as an environment failure without a retry: %+v", result)
			}
			for _, path := range []string{sso, cli} {
				if _, statErr := os.Stat(path); statErr == nil {
					t.Fatalf("%s was created for a poll the registration never covered", path)
				}
			}
		})
	}

	// The constraint that must change the result: the registration covered
	// exactly these writes.
	if result, err := poll("registered-with-these-writes", bound(cli, sso)); err != nil || !strings.Contains(result.Output, "cache write ok: sso") {
		t.Fatalf("a poll within its registered writes must run: %v (%s)", err, result.Output)
	}
}

// Registration freezes the write set its approval covered onto the binding the
// polls are later held to.
func TestExternalWatchRegistrationRecordsTheApprovedHostWrites(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("enforced sandbox unavailable")
	}
	for _, tc := range []struct {
		name     string
		approved []string
	}{
		{"approved-writes", []string{"/h/.aws/sso/cache", "/h/.aws/cli/cache"}},
		// An approval that covered no writes records none.
		{"approved-none", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := control.OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			person, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "watch-writes-"+tc.name, "Watch")
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: person.TenantID, PersonID: person.PersonID, Title: "Watch a deploy", Channel: "cli"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.StartRun(ctx, task, "cli", "Watch a deploy")
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(fixtureBase(t), "home")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			tenant, workspace := profileExecScope(t, home, "", executionenv.TrustTrusted)
			previous, _, _ := lookupExecutionScope(tenant)
			snapshot, ok := executionenv.DefaultRegistry().ForLease(previous.LeaseID)
			if !ok {
				t.Fatal("fixture snapshot missing")
			}
			lease, err := store.MaterializeExecutionLease(ctx, executionenv.Lease{ID: previous.LeaseID, TenantID: person.TenantID, PersonID: person.PersonID,
				RunID: run.ID, WorkspaceID: "ws-watch-writes", EnvironmentSnapshotID: snapshot.ID, EnvironmentGeneration: snapshot.Generation,
				PrincipalFingerprint: snapshot.PrincipalFingerprint, EnvironmentFingerprint: snapshot.EnvironmentFingerprint, CredentialSourceHash: snapshot.CredentialSourceHash})
			if err != nil {
				t.Fatal(err)
			}
			scope := previous
			scope.TenantID, scope.PersonID, scope.TaskID, scope.RunID, scope.WorkspaceID = person.TenantID, person.PersonID, task.ID, run.ID, lease.WorkspaceID
			scope.ParallelWork = true
			scope.SandboxPolicy = &ExecSandboxPolicy{Enabled: true, Required: true}
			defer SetExecutionScope(tenant, scope)()
			if err := os.WriteFile(filepath.Join(workspace, "observed.state"), []byte("RUNNING\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := NewExternalWatchTool(store).Execute(map[string]interface{}{
				"_tenant_id": tenant, "command": "cat observed.state", "cwd": workspace,
				"terminal_success_pattern": "^SUCCEEDED$", "terminal_failure_pattern": "^FAILED$",
				approvedHostWritesArg: tc.approved,
			})
			var response struct {
				WatchID string `json:"watch_id"`
			}
			if err != nil || json.Unmarshal([]byte(result), &response) != nil || response.WatchID == "" {
				t.Fatalf("registration: %v (%s)", err, result)
			}
			watch, err := store.GetExternalWatch(ctx, person.TenantID, response.WatchID)
			if err != nil {
				t.Fatal(err)
			}
			if got := watch.ExecutionBinding.HostStateWrites; !slices.Equal(got, tc.approved) {
				t.Fatalf("the binding must carry the approved writes %v, got %v", tc.approved, got)
			}
			// The same registration freezes the policy its Run ran under: the two
			// facts a poll is held to are recorded together.
			if got := watch.ExecutionBinding.SandboxPolicy; got == nil || !got.Enabled || !got.Required {
				t.Fatalf("the binding must carry the Run's frozen policy beside the writes, got %+v", got)
			}
		})
	}
}

// resumeByFingerprint holds parked approvals the person has since approved,
// each a one-shot authorization for exactly the action it was parked for.
type resumeByFingerprint struct{ authorized map[string]bool }

func (s *resumeByFingerprint) ClaimApprovalResumeAuthorization(_ context.Context, _, _, _, fingerprint string) (string, string, string, bool, error) {
	if !s.authorized[fingerprint] {
		return "", "", "", false, nil
	}
	delete(s.authorized, fingerprint)
	return "apr-parked", "once", "", true, nil
}

// The decisions not keyed by a class — a judge's allow, an exact-run grant, a
// parked approval's resume authorization — carry the call's containment
// summary, which names its in-place credential writes. Each is reused while the
// write set is unchanged and decided again once the configuration makes the
// same command write the operator's real cache.
func TestReusedDecisionsAreBoundToTheHostCredentialWrites(t *testing.T) {
	start := func(t *testing.T, name string, mode ApprovalMode, adjust func(*ExecutionScope)) (*boundaryRun, string) {
		t.Helper()
		requireInPlaceBackend(t)
		withExecSandboxPolicy(t, true, false, false)
		base := fixtureBase(t)
		home := awsStateHome(t, base, staticAWSConfig)
		return newBoundaryRun(t, "reuse-"+name, home, writeStub(t, base, "npm", npmDeployStub), executionenv.TrustTrusted, mode, adjust), home
	}
	steps := []struct {
		config string
		want   int
	}{{staticAWSConfig, 1}, {staticAWSConfig, 1}, {ssoProfileConfig, 2}, {ssoProfileConfig, 2}}

	t.Run("judge", func(t *testing.T) {
		run, home := start(t, "judge", ApprovalSmart, nil)
		run.judge.reply = judgeApproves
		for index, step := range steps {
			writeAWSConfig(t, home, step.config)
			if _, out, err := run.call("npm run deploy", nil); err != nil {
				t.Fatalf("step %d: the judged call must run: %v (%s)", index, err, out)
			}
			if run.judge.calls != step.want {
				t.Fatalf("step %d: judge calls=%d, want %d", index, run.judge.calls, step.want)
			}
		}
		if len(run.asks) != 0 {
			t.Fatalf("fixture: the judge's allow releases without a person: %+v", run.asks)
		}
	})

	t.Run("exact-run", func(t *testing.T) {
		// An injected variable never backs a remembered class, so the only
		// reusable answer offered is this exact action for this run.
		const command = "NODE_OPTIONS=--require=./hook.js npm run deploy"
		run, home := start(t, "exact-run", ApprovalOnRequest, nil)
		run.decide = func(ToolApprovalRequest) ToolApprovalDecision {
			return ToolApprovalDecision{Approved: true, ApprovalID: "apr-exact", Scope: "run"}
		}
		for index, step := range steps {
			writeAWSConfig(t, home, step.config)
			if _, out, err := run.call(command, nil); err != nil {
				t.Fatalf("step %d: the approved call must run: %v (%s)", index, err, out)
			}
			if len(run.asks) != step.want {
				t.Fatalf("step %d: asks=%d, want %d", index, len(run.asks), step.want)
			}
		}
		if run.asks[0].RunGrantClass != "this exact action for this run" {
			t.Fatalf("fixture: the ask must offer only the exact-run grant, got %q", run.asks[0].RunGrantClass)
		}
		if !strings.Contains(run.asks[1].Containment, "host_credential_writes=") {
			t.Fatalf("the second ask must name the writes: %q", run.asks[1].Containment)
		}
	})

	t.Run("parked-resume", func(t *testing.T) {
		resume := &resumeByFingerprint{authorized: map[string]bool{}}
		run, home := start(t, "parked-resume", ApprovalOnRequest, func(scope *ExecutionScope) { scope.ResumeAuthorizations = resume })
		run.approve = false
		// The call parks; the person approves it afterwards.
		if _, _, err := run.call("npm run deploy", nil); err == nil || len(run.asks) != 1 {
			t.Fatalf("fixture: the first call must park on a person: asks=%d err=%v", len(run.asks), err)
		}
		resume.authorized[run.asks[0].AuthorizationFingerprint] = true
		// The regenerated action now writes the real cache in place: the
		// authorization parked for no writes does not cover it.
		writeAWSConfig(t, home, ssoProfileConfig)
		if _, _, err := run.call("npm run deploy", nil); err == nil || len(run.asks) != 2 {
			t.Fatalf("a parked approval released new host credential writes: asks=%d err=%v", len(run.asks), err)
		}
		if !resume.authorized[run.asks[0].AuthorizationFingerprint] {
			t.Fatal("the authorization parked for no writes was consumed by another action")
		}
		// The constraint that must change the result: an authorization parked
		// for these writes releases the regenerated action.
		resume.authorized[run.asks[1].AuthorizationFingerprint] = true
		if _, out, err := run.call("npm run deploy", nil); err != nil || len(run.asks) != 2 || !strings.Contains(out, "cache written") {
			t.Fatalf("an authorization for the same writes must be claimed: asks=%d err=%v (%s)", len(run.asks), err, out)
		}
	})
}

// The engine's typed-refusal branch: a preparation refusal that is a decision
// about the plan — here, in-place writes the releasing approval did not cover —
// ends the call with the environment class and no recovery retry, before any
// process starts. An ordinary preparation failure would be retried once and
// lose the class.
func TestEngineEndsATypedPreparationRefusalWithoutRetry(t *testing.T) {
	requireInPlaceBackend(t)
	withExecSandboxPolicy(t, true, false, false)
	base := fixtureBase(t)
	home := awsStateHome(t, base, ssoProfileConfig)
	run := newBoundaryRun(t, "engine-refusal", home, writeStub(t, base, "npm", npmDeployStub), executionenv.TrustTrusted, ApprovalOnRequest, nil)
	execute := func(approved []string) (ExecutionResult, error) {
		return Execute(context.Background(), ExecutionRequest{
			ToolName: "terminal", Payload: "npm run deploy", Shell: true, CWD: run.workspace,
			WorkspaceRoots: []string{run.workspace}, Sandbox: SandboxAuto, Timeout: 30 * time.Second,
			ToolProfile: ToolProfile{Class: ToolExecutionStandard, Timeout: 30 * time.Second},
		}, map[string]interface{}{
			"_tenant_id": run.tenant, "_tool_name": "terminal", "command": "npm run deploy", "cwd": run.workspace,
			approvedHostWritesArg: approved,
		})
	}
	result, err := execute([]string{})
	if refusal := requireNotDispatched(t, err); refusal.ToolErrorCode() != "approved_host_writes_changed" {
		t.Fatalf("want the write-set refusal, got %s: %v", refusal.ToolErrorCode(), err)
	}
	if result.Started || result.FailureClass != "environment" || result.RecoveryAttempted || result.RecoveryOutcome != RecoveryNotEligible {
		t.Fatalf("a typed refusal must end the call without a retry and keep its class: %+v", result)
	}

	// The constraint that must change the result: the approval covered these
	// writes, so the same request runs.
	approved := []string{filepath.Join(canonicalPath(t, home), ".aws", "sso", "cache"), filepath.Join(canonicalPath(t, home), ".aws", "cli", "cache")}
	if result, err := execute(approved); err != nil || !strings.Contains(result.Output, "cache written") {
		t.Fatalf("covered writes must run: %v (%s)", err, result.Output)
	}
}

// Item 4: a phase bundle mints one exact run-local key per declared command.
// Like every other reusable key it is bound to the in-place credential writes
// the command makes, and its ask says so. A bundle approved for these writes
// releases the call; one approved before the writes appeared does not, which is
// the dispatch-side binding of the declared-effect key.
func TestPhaseBundleIsBoundToTheDeclaredCommandsHostWrites(t *testing.T) {
	for _, tc := range []struct {
		name, atBundle, atCall string
		released               bool
	}{
		{"bundle-for-these-writes", ssoProfileConfig, ssoProfileConfig, true},
		// The same invariant from the other side: the writes appear after the
		// bundle was approved.
		{"writes-appear-after-the-bundle", staticAWSConfig, ssoProfileConfig, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireInPlaceBackend(t)
			withExecSandboxPolicy(t, true, false, false)
			base := fixtureBase(t)
			home := awsStateHome(t, base, tc.atBundle)
			bin := writeStub(t, base, "npm", npmDeployStub)
			run := newBoundaryRun(t, "bundle-"+tc.name, home, bin, executionenv.TrustTrusted, ApprovalOnRequest, nil)
			run.decide = func(request ToolApprovalRequest) ToolApprovalDecision {
				if request.ToolName == "request_permissions" {
					return ToolApprovalDecision{Approved: true, ApprovalID: "apr-phase", Scope: "run", Outcome: ApprovalOutcomeApproved}
				}
				return ToolApprovalDecision{Approved: true, ApprovalID: "apr-once"}
			}
			registry := NewRegistry()
			registry.Register(NewRequestPermissionsTool())
			registry.Register(NewExecuteCommandTool())
			declared := MarshalArgs(map[string]interface{}{"command": "npm run deploy", "cwd": run.workspace, "timeout": 30})
			if out, err := registry.Dispatch("request_permissions", map[string]interface{}{
				"_tenant_id": run.tenant,
				"effects":    []interface{}{map[string]interface{}{"tool": "terminal", "arguments_json": declared}},
				"reason":     "deploy this release",
			}); err != nil || len(run.asks) != 1 {
				t.Fatalf("the bundle must be asked once and granted: asks=%d err=%v (%s)", len(run.asks), err, out)
			}
			bundle := run.asks[0]
			label, _ := bundle.Args["permissions"].(string)
			writes := strings.Contains(label, "while writing ~/.aws/sso/cache")
			if writes != (tc.atBundle == ssoProfileConfig) || writes != strings.HasPrefix(bundle.Reason, "writes credential caches in place: ~/.aws/sso/cache") {
				t.Fatalf("the bundle must show exactly the writes its command makes: reason=%q label=%q", bundle.Reason, label)
			}

			writeAWSConfig(t, home, tc.atCall)
			_, out, err := run.call("npm run deploy", nil)
			if err != nil || !strings.Contains(out, "cache written") {
				t.Fatalf("the declared command must run: %v (%s)", err, out)
			}
			if released := len(run.asks) == 1; released != tc.released {
				t.Fatalf("bundle released=%v, want %v (asks=%+v)", released, tc.released, run.asks)
			}
			if !tc.released && !strings.Contains(run.asks[1].Containment, "host_credential_writes=") {
				t.Fatalf("the new ask must name the writes: %q", run.asks[1].Containment)
			}
		})
	}
}
