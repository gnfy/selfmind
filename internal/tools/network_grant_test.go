package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"selfmind/internal/executionenv"
)

// networkOnlyCapabilityStore stands in for a person's stored grants when only
// network:shared is granted. The shared stub answers true for every capability,
// which would also hand out credential:read and hide the network question.
type networkOnlyCapabilityStore struct{}

func (networkOnlyCapabilityStore) HasExecutionCapability(_ context.Context, _, _, _, capability, _ string) (bool, error) {
	return capability == executionenv.CapabilityNetworkShared, nil
}

func (networkOnlyCapabilityStore) GrantExecutionCapability(context.Context, string, string, string, string, string, string, time.Time) error {
	return nil
}

// networkHarness drives one terminal call at a time through the real capability
// middleware under a chosen workspace trust, operator policy and grant state.
type networkHarness struct {
	t        *testing.T
	person   string
	asks     []ToolApprovalRequest
	answer   func(ToolApprovalRequest) ToolApprovalDecision
	executed []map[string]interface{}
	respond  func(args map[string]interface{}) (string, error)
	executor ToolExecutor
}

type networkHarnessOptions struct {
	trust  string
	policy *ExecSandboxPolicy
	store  ExecutionCapabilityStore
}

func newNetworkHarness(t *testing.T, opts networkHarnessOptions) *networkHarness {
	t.Helper()
	// The process default must not decide anything: every scope carries its own
	// frozen policy, as a Run does.
	withExecSandboxPolicy(t, true, false, true)
	if opts.trust == "" {
		opts.trust = executionenv.TrustUntrusted
	}
	if opts.policy == nil {
		opts.policy = &ExecSandboxPolicy{Enabled: true, AllowNetwork: true}
	}
	person := "net-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	h := &networkHarness{t: t, person: person}
	h.answer = func(ToolApprovalRequest) ToolApprovalDecision {
		return ToolApprovalDecision{Approved: true, Scope: "run"}
	}
	h.respond = func(map[string]interface{}) (string, error) { return "ok", nil }
	scope := ExecutionScope{
		TenantID: "tenant-net", PersonID: person, RunID: "run-" + person, WorkspaceID: "workspace-net",
		TrustLevel: opts.trust, SandboxPolicy: opts.policy, CapabilityStore: opts.store,
		Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			h.asks = append(h.asks, req)
			return h.answer(req), nil
		},
	}
	cleanup := SetExecutionScope(person, scope)
	t.Cleanup(cleanup)
	h.executor = ExecutionCapabilityMiddleware()(func(args map[string]interface{}) (string, error) {
		h.executed = append(h.executed, args)
		return h.respond(args)
	})
	return h
}

// call runs one command and returns what the middleware handed to the tool.
func (h *networkHarness) call(command string, extra ...interface{}) (map[string]interface{}, string, error) {
	h.t.Helper()
	return h.callTool("terminal", command, extra...)
}

func (h *networkHarness) callTool(tool, command string, extra ...interface{}) (map[string]interface{}, string, error) {
	h.t.Helper()
	args := map[string]interface{}{
		"_tenant_id": h.person, "_tool_name": tool,
		// Isolation is pinned so a host without a sandbox cannot turn the call
		// into host execution and skip the capability decision entirely.
		"_effective_sandbox_mode": string(SandboxIsolated),
	}
	if tool == "execute_code" {
		args["code"] = command
	} else {
		args["command"] = command
	}
	for i := 0; i+1 < len(extra); i += 2 {
		args[extra[i].(string)] = extra[i+1]
	}
	output, err := h.executor(args)
	return args, output, err
}

func argShared(args map[string]interface{}) bool {
	shared, _ := args["_network_shared"].(bool)
	return shared
}
func argDecision(args map[string]interface{}) string {
	decision, _ := args["_network_decision"].(string)
	return decision
}
func argPermission(args map[string]interface{}) string {
	permission, _ := args["_network_permission"].(string)
	return permission
}

// A stored or run network grant is a PERMISSION. It must not make every command
// in the workspace networked: purely local work stays contained, and only a
// command that reaches a remote service receives the network.
func TestNetworkGrantIsPermissionNotReason(t *testing.T) {
	local := []string{"go build ./...", "npm test", "ls -la", "cargo check", "pytest -q", "make lint", "git status", "git commit -m wip"}
	remote := []string{
		"npm ci", "git push origin main", "terraform plan", "pip install -r requirements.txt",
		"cd web && yarn add left-pad", "npm --prefix web install", "sudo apt-get install -y jq", "curl https://example.test/health",
	}
	for _, grant := range []string{"stored", "run"} {
		t.Run(grant, func(t *testing.T) {
			opts := networkHarnessOptions{}
			if grant == "stored" {
				opts.store = networkOnlyCapabilityStore{}
			}
			h := newNetworkHarness(t, opts)
			if grant == "run" {
				// A person's "yes, for this run" is the run grant.
				if args, _, err := h.call("curl https://example.test/health"); err != nil || !argShared(args) || len(h.asks) != 1 {
					t.Fatalf("seeding the run grant: shared=%v asks=%d err=%v", argShared(args), len(h.asks), err)
				}
			}
			asked := len(h.asks)
			for _, command := range local {
				args, _, err := h.call(command)
				if err != nil {
					t.Fatalf("%s: %v", command, err)
				}
				if argShared(args) || argDecision(args) != executionenv.NetworkWithheldNotBound || argPermission(args) != executionenv.NetworkPermissionGrant {
					t.Errorf("%q: a grant made a local command networked: shared=%v decision=%q permission=%q", command, argShared(args), argDecision(args), argPermission(args))
				}
			}
			for _, command := range remote {
				args, _, err := h.call(command)
				if err != nil {
					t.Fatalf("%s: %v", command, err)
				}
				if !argShared(args) || argDecision(args) != executionenv.NetworkDecisionShared {
					t.Errorf("%q: a grant did not network a command that reaches a remote service: shared=%v decision=%q", command, argShared(args), argDecision(args))
				}
			}
			if len(h.asks) != asked {
				t.Fatalf("an existing grant must not ask again: %d asks", len(h.asks)-asked)
			}
		})
	}
}

// No grant, no network: a command that does not need it runs contained and
// says why, and a plausible one is not silently given the network either.
func TestNetworkIsWithheldWithoutAGrantAndSaysWhy(t *testing.T) {
	h := newNetworkHarness(t, networkHarnessOptions{})
	for _, command := range []string{"go test ./...", "npm install", "git push origin main"} {
		args, _, err := h.call(command)
		if err != nil || argShared(args) || argDecision(args) != executionenv.NetworkWithheldNoGrant || argPermission(args) != "" {
			t.Errorf("%q: shared=%v decision=%q permission=%q err=%v", command, argShared(args), argDecision(args), argPermission(args), err)
		}
	}
	if len(h.asks) != 0 {
		t.Fatalf("a command that does not name a clear egress program must not ask before it runs: %d asks", len(h.asks))
	}
}

// The trusted-workspace path uses the same predicate as a grant: npm install and
// git push just work there, a build does not borrow the network, and the
// operator policy still wins over trust.
func TestTrustedWorkspaceUsesTheSamePredicate(t *testing.T) {
	h := newNetworkHarness(t, networkHarnessOptions{trust: executionenv.TrustTrusted})
	for _, command := range []string{"npm install", "git push origin main", "go mod download", "terraform apply", "pip install flask"} {
		args, _, err := h.call(command)
		if err != nil || !argShared(args) || argDecision(args) != executionenv.NetworkDecisionShared || argPermission(args) != executionenv.NetworkPermissionTrust {
			t.Errorf("%q in a trusted workspace: shared=%v decision=%q permission=%q err=%v", command, argShared(args), argDecision(args), argPermission(args), err)
		}
	}
	for _, command := range []string{"go test ./...", "npm test", "git status", "cargo build", "mvn test"} {
		args, _, err := h.call(command)
		if err != nil || argShared(args) || argDecision(args) != executionenv.NetworkWithheldNotBound || argPermission(args) != executionenv.NetworkPermissionTrust {
			t.Errorf("%q in a trusted workspace: shared=%v decision=%q permission=%q err=%v", command, argShared(args), argDecision(args), argPermission(args), err)
		}
	}
	if len(h.asks) != 0 {
		t.Fatalf("a trusted workspace under a permissive policy asks nothing: %d asks", len(h.asks))
	}
}

// exec_sandbox.allow_network: false still wins: trust is not permission under
// it. Only an approved capability lifts it, for the commands that need it.
func TestOperatorPolicyDisablingNetworkStillWins(t *testing.T) {
	policy := &ExecSandboxPolicy{Enabled: true, AllowNetwork: false}
	h := newNetworkHarness(t, networkHarnessOptions{trust: executionenv.TrustTrusted, policy: policy})
	for _, command := range []string{"npm install", "git push origin main", "go test ./..."} {
		args, _, err := h.call(command)
		if err != nil || argShared(args) || argDecision(args) != executionenv.NetworkWithheldPolicyDisabled || argPermission(args) != "" {
			t.Errorf("%q: trust overrode the disabled policy: shared=%v decision=%q permission=%q err=%v", command, argShared(args), argDecision(args), argPermission(args), err)
		}
	}
	if len(h.asks) != 0 {
		t.Fatalf("withholding by policy asks nothing before a plausible command runs: %d asks", len(h.asks))
	}
	// A clear egress program is approved before it runs, exactly as before.
	args, _, err := h.call("curl https://example.test/health")
	if err != nil || !argShared(args) || len(h.asks) != 1 || h.asks[0].ToolName != executionenv.CapabilityNetworkShared || argPermission(args) != executionenv.NetworkPermissionApproval {
		t.Fatalf("approved capability: shared=%v asks=%d permission=%q err=%v", argShared(args), len(h.asks), argPermission(args), err)
	}

	// The same policy with a stored grant: the grant is the approved exception,
	// and it applies to a plausible command only.
	granted := newNetworkHarness(t, networkHarnessOptions{trust: executionenv.TrustTrusted, policy: policy, store: networkOnlyCapabilityStore{}})
	if args, _, err := granted.call("npm install"); err != nil || !argShared(args) || argPermission(args) != executionenv.NetworkPermissionGrant {
		t.Fatalf("a stored grant lifts a disabled policy for a plausible command: shared=%v permission=%q err=%v", argShared(args), argPermission(args), err)
	}
	if args, _, err := granted.call("go test ./..."); err != nil || argShared(args) || argDecision(args) != executionenv.NetworkWithheldNotBound {
		t.Fatalf("the same grant networked a local command: shared=%v decision=%q err=%v", argShared(args), argDecision(args), err)
	}
}

// The model can ask for the network explicitly. The request never grants
// anything: a grant or trust still decides, and without one the existing
// one-time capability approval runs exactly as for a clearly-network command.
func TestExplicitNetworkRequestNeverGrantsAnything(t *testing.T) {
	t.Run("with a grant the call is networked", func(t *testing.T) {
		h := newNetworkHarness(t, networkHarnessOptions{store: networkOnlyCapabilityStore{}})
		for _, command := range []string{"go test ./...", "./scripts/integration.sh", "make verify"} {
			args, _, err := h.call(command, "network", true)
			if err != nil || !argShared(args) || argDecision(args) != executionenv.NetworkDecisionShared {
				t.Errorf("%q with network:true and a grant: shared=%v decision=%q err=%v", command, argShared(args), argDecision(args), err)
			}
			if args, _, _ := h.call(command, "network", false); argShared(args) {
				t.Errorf("%q with network:false was networked", command)
			}
		}
		if len(h.asks) != 0 {
			t.Fatalf("a grant answers the request: %d asks", len(h.asks))
		}
	})
	t.Run("in a trusted workspace", func(t *testing.T) {
		h := newNetworkHarness(t, networkHarnessOptions{trust: executionenv.TrustTrusted})
		args, _, err := h.call("go test ./...", "network", true)
		if err != nil || !argShared(args) || argPermission(args) != executionenv.NetworkPermissionTrust || len(h.asks) != 0 {
			t.Fatalf("trust answers the request: shared=%v permission=%q asks=%d err=%v", argShared(args), argPermission(args), len(h.asks), err)
		}
	})
	t.Run("without a grant it asks once, before the command runs", func(t *testing.T) {
		h := newNetworkHarness(t, networkHarnessOptions{})
		args, _, err := h.call("./scripts/integration.sh", "network", true)
		if err != nil || !argShared(args) || len(h.asks) != 1 || h.asks[0].ToolName != executionenv.CapabilityNetworkShared || len(h.executed) != 1 {
			t.Fatalf("request without a grant: shared=%v asks=%d executed=%d err=%v", argShared(args), len(h.asks), len(h.executed), err)
		}
		if argPermission(args) != executionenv.NetworkPermissionApproval {
			t.Fatalf("permission = %q, want approval", argPermission(args))
		}
		// The run grant the person gave permits; it does not make the next local
		// command networked.
		args, _, err = h.call("go test ./...")
		if err != nil || argShared(args) || argDecision(args) != executionenv.NetworkWithheldNotBound || len(h.asks) != 1 {
			t.Fatalf("a run grant from one request networked the next local command: shared=%v decision=%q asks=%d", argShared(args), argDecision(args), len(h.asks))
		}
	})
	t.Run("a refusal runs nothing", func(t *testing.T) {
		h := newNetworkHarness(t, networkHarnessOptions{})
		h.answer = func(ToolApprovalRequest) ToolApprovalDecision { return ToolApprovalDecision{Approved: false} }
		_, _, err := h.call("go test ./...", "network", true)
		if err == nil || !strings.Contains(err.Error(), "operation rejected") || len(h.executed) != 0 {
			t.Fatalf("a declined request must reject and not run: err=%v executed=%d", err, len(h.executed))
		}
	})
	t.Run("a disabled policy asks even in a trusted workspace", func(t *testing.T) {
		h := newNetworkHarness(t, networkHarnessOptions{trust: executionenv.TrustTrusted, policy: &ExecSandboxPolicy{Enabled: true}})
		args, _, err := h.call("go test ./...", "network", true)
		if err != nil || !argShared(args) || len(h.asks) != 1 {
			t.Fatalf("policy off, trusted, request: shared=%v asks=%d err=%v", argShared(args), len(h.asks), err)
		}
	})
	t.Run("only tools that declare it read it", func(t *testing.T) {
		h := newNetworkHarness(t, networkHarnessOptions{})
		args, _, err := h.callTool("shell", "print(1)", "network", true)
		if err != nil || argShared(args) || len(h.asks) != 0 {
			t.Fatalf("a tool that does not declare the parameter was networked by it: shared=%v asks=%d err=%v", argShared(args), len(h.asks), err)
		}
		// It is stripped, so an approval's displayed arguments cannot show a
		// request the tool never had.
		if _, kept := args["network"]; kept {
			t.Fatalf("an undeclared network argument was kept: %+v", args)
		}
		if args, _, _ = h.callTool("terminal", "go test ./...", "network", true); args["network"] != true {
			t.Fatal("terminal lost its declared network argument")
		}
		args, _, err = h.callTool("verify", "go test ./...", "network", true)
		if err != nil || !argShared(args) || len(h.asks) != 1 {
			t.Fatalf("verify takes the parameter: shared=%v asks=%d err=%v", argShared(args), len(h.asks), err)
		}
		// Anything but a real boolean true is not a request.
		for _, value := range []interface{}{"yes", 1, nil} {
			args, _, err = h.call("go test ./...", "network", value)
			if err != nil || argShared(args) {
				t.Fatalf("network=%v counted as a request: shared=%v err=%v", value, argShared(args), err)
			}
		}
	})
	t.Run("a model cannot forge the runtime decision", func(t *testing.T) {
		h := newNetworkHarness(t, networkHarnessOptions{})
		args, _, err := h.call("go test ./...", "_network_decision", executionenv.NetworkDecisionShared, "_network_permission", executionenv.NetworkPermissionGrant)
		if err != nil || argShared(args) || argDecision(args) != executionenv.NetworkWithheldNoGrant || argPermission(args) != "" {
			t.Fatalf("runtime-owned fields were taken from the call: decision=%q permission=%q err=%v", argDecision(args), argPermission(args), err)
		}
	})
}

// A legacy isolated view stays file-only: an explicit request does not change
// that, exactly as a clearly-network command does not.
func TestExplicitRequestCannotLeaveALegacyIsolatedView(t *testing.T) {
	withExecSandboxPolicy(t, true, false, true)
	person := "net-legacy-view"
	cleanup := SetExecutionScope(person, ExecutionScope{
		TenantID: "t", PersonID: person, RunID: "run-" + person, WorkspaceID: "w", WorkspaceRoot: "/managed/view", AllowedRoots: []string{"/managed/view"},
		RootBindings:    []executionenv.RootBinding{{Path: "/managed/view", Source: executionenv.RootSourceExecutionView}},
		CapabilityStore: networkOnlyCapabilityStore{}, TrustLevel: executionenv.TrustTrusted,
		SandboxPolicy: &ExecSandboxPolicy{Enabled: true, Required: true},
	})
	defer cleanup()
	calls := 0
	executor := ExecutionCapabilityMiddleware()(func(args map[string]interface{}) (string, error) {
		calls++
		if argShared(args) || argDecision(args) != executionenv.NetworkWithheldPolicyDisabled {
			t.Fatalf("legacy view call: shared=%v decision=%q", argShared(args), argDecision(args))
		}
		return "local", nil
	})
	args := map[string]interface{}{"_tenant_id": person, "_tool_name": "terminal", "command": "go test ./...", "network": true, "_effective_sandbox_mode": string(SandboxIsolated)}
	if _, err := executor(args); err == nil || calls != 0 {
		t.Fatalf("an explicit request left a file-only view: calls=%d err=%v", calls, err)
	}
	args = map[string]interface{}{"_tenant_id": person, "_tool_name": "terminal", "command": "go test ./...", "_effective_sandbox_mode": string(SandboxIsolated)}
	if _, err := executor(args); err != nil || calls != 1 {
		t.Fatalf("a local command in the view failed: calls=%d err=%v", calls, err)
	}
}

type recoveryFacts interface {
	ToolErrorCode() string
	ToolErrorCategory() string
	ToolFailurePhase() string
	ToolRetryability() string
	ToolEffectState() string
	ModelSafeMessage() string
}

func networkFailureFor(command, output string) func(map[string]interface{}) (string, error) {
	return func(args map[string]interface{}) (string, error) {
		if argShared(args) {
			return "downloaded, built", nil
		}
		return output, fmt.Errorf("command failed: exit status 101 (%s)", command)
	}
}

// When a command fails for want of network, a workspace that already permits it
// is not asked again: the call returns a typed result that says network was
// withheld by policy for this command, that it was not replayed, and how to run
// it networked. The failure keeps its classification and effect certainty.
func TestWithheldNetworkFailureIsATypedResultNotAnotherAsk(t *testing.T) {
	cases := []struct{ name, command, output string }{
		{"cold crate index", "cargo build", "error: failed to get `serde` as a dependency: dial tcp: lookup index.crates.io: no such host"},
		{"maven wrapper", "./gradlew build", "Could not resolve host: repo.maven.apache.org"},
		{"local script", "./scripts/check.sh", "curl: (6) Could not resolve host: example.test"},
	}
	for _, grant := range []string{"stored", "trusted"} {
		for _, tc := range cases {
			t.Run(grant+"/"+tc.name, func(t *testing.T) {
				opts := networkHarnessOptions{store: networkOnlyCapabilityStore{}}
				if grant == "trusted" {
					opts = networkHarnessOptions{trust: executionenv.TrustTrusted}
				}
				h := newNetworkHarness(t, opts)
				h.respond = networkFailureFor(tc.command, tc.output)
				_, output, err := h.call(tc.command)
				if err == nil {
					t.Fatal("the offline failure was swallowed")
				}
				if len(h.asks) != 0 || len(h.executed) != 1 {
					t.Fatalf("the command must not be asked about or replayed: asks=%d executed=%d", len(h.asks), len(h.executed))
				}
				if output != tc.output {
					t.Fatalf("the failed command's own output was not preserved: %q", output)
				}
				var facts recoveryFacts
				if !errors.As(err, &facts) {
					t.Fatalf("the failure is not typed: %v", err)
				}
				// It sets no category of its own: the failure keeps the class its text
				// already carries (sandbox_no_network from the engine), which is what
				// events and reports group it by.
				if facts.ToolErrorCode() != "network_withheld_by_policy" || facts.ToolErrorCategory() != "" {
					t.Fatalf("code=%q category=%q", facts.ToolErrorCode(), facts.ToolErrorCategory())
				}
				// The command ran and failed: it is neither "not dispatched" nor an
				// unknown effect, and a retry with network:true must stay possible.
				if facts.ToolEffectState() != "" || facts.ToolRetryability() == "different_strategy" {
					t.Fatalf("effect=%q retryability=%q would change the failure's meaning or block the retry", facts.ToolEffectState(), facts.ToolRetryability())
				}
				message := facts.ModelSafeMessage()
				for _, want := range []string{"withheld by policy", "permits", "not replayed", "network: true", "exit status 101"} {
					if !strings.Contains(message, want) {
						t.Fatalf("typed result does not say %q: %s", want, message)
					}
				}
				// The explicit retry runs networked, with the same grant.
				args, retried, retryErr := h.call(tc.command, "network", true)
				if retryErr != nil || !argShared(args) || retried != "downloaded, built" || len(h.asks) != 0 {
					t.Fatalf("retry with network:true: shared=%v out=%q asks=%d err=%v", argShared(args), retried, len(h.asks), retryErr)
				}
			})
		}
	}
}

// The typed result wraps the original failure instead of replacing it: a cause
// that is itself typed keeps its category, and the cause stays reachable.
func TestNetworkWithheldKeepsTheCauseItsClassAndIdentity(t *testing.T) {
	cause := newStableToolError(errors.New("command failed: exit status 2\nerror_class: sandbox_no_network"), "sandbox_no_network", "sandbox_no_network", "no network", "retry")
	err := newNetworkWithheldError(cause)
	var facts recoveryFacts
	if !errors.As(err, &facts) || facts.ToolErrorCode() != "network_withheld_by_policy" || facts.ToolErrorCategory() != "sandbox_no_network" {
		t.Fatalf("typed result lost the cause's class: %v", err)
	}
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "error_class: sandbox_no_network") || !strings.Contains(err.Error(), "network: true") {
		t.Fatalf("typed result is not the cause plus the note: %v", err)
	}
}

// Only a network-class failure takes this path; any other failure is returned as
// it was, and a command that did get the network is never second-guessed.
func TestOtherFailuresAreNotRewrittenAsNetworkWithheld(t *testing.T) {
	h := newNetworkHarness(t, networkHarnessOptions{store: networkOnlyCapabilityStore{}})
	h.respond = func(map[string]interface{}) (string, error) {
		return "--- FAIL: TestParse (0.00s)", errors.New("command failed: exit status 1")
	}
	_, _, err := h.call("go test ./...")
	var facts recoveryFacts
	if err == nil || errors.As(err, &facts) || len(h.asks) != 0 {
		t.Fatalf("an ordinary test failure was retyped or asked about: err=%v asks=%d", err, len(h.asks))
	}
	h.respond = func(map[string]interface{}) (string, error) {
		return "npm ERR! network request failed", errors.New("command failed: exit status 1\nerror_class: network")
	}
	if _, _, err := h.call("npm install"); err == nil || errors.As(err, &facts) || len(h.asks) != 0 {
		t.Fatalf("a networked command's own network failure must stay its own: err=%v asks=%d", err, len(h.asks))
	}
}

// With no permission at all, today's flow stays: ask once after the failure,
// never replay the command, and tell the model how to run it networked. The
// approval permits; it does not make the next local command networked.
func TestWithoutAGrantTheFailureAsksOnceAndRetryNeedsTheFlag(t *testing.T) {
	h := newNetworkHarness(t, networkHarnessOptions{})
	h.respond = networkFailureFor("cargo build", "dial tcp: lookup index.crates.io: no such host")
	_, _, err := h.call("cargo build")
	if err == nil || len(h.asks) != 1 || len(h.executed) != 1 || h.asks[0].ToolName != executionenv.CapabilityNetworkShared {
		t.Fatalf("first failure: err=%v asks=%d executed=%d", err, len(h.asks), len(h.executed))
	}
	for _, want := range []string{"network:shared was approved", "network: true", "not automatically replayed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the approval message does not say %q: %v", want, err)
		}
	}
	// Retried as told, it runs networked.
	args, out, err := h.call("cargo build", "network", true)
	if err != nil || !argShared(args) || out != "downloaded, built" || len(h.asks) != 1 {
		t.Fatalf("retry with network:true: shared=%v out=%q asks=%d err=%v", argShared(args), out, len(h.asks), err)
	}
	// Retried without the flag it stays offline, fails again, and now the grant
	// exists, so it is the typed result and not a second ask.
	_, _, err = h.call("cargo build")
	var facts recoveryFacts
	if err == nil || !errors.As(err, &facts) || facts.ToolErrorCode() != "network_withheld_by_policy" || len(h.asks) != 1 {
		t.Fatalf("retry without the flag: err=%v asks=%d", err, len(h.asks))
	}
}

// A person who declines after the failure has decided; the failed command's own
// output still reaches the model.
func TestDecliningAfterTheFailureIsFinal(t *testing.T) {
	h := newNetworkHarness(t, networkHarnessOptions{})
	h.answer = func(ToolApprovalRequest) ToolApprovalDecision { return ToolApprovalDecision{Approved: false} }
	h.respond = networkFailureFor("cargo build", "dial tcp: lookup index.crates.io: no such host")
	_, output, err := h.call("cargo build")
	if err == nil || !strings.Contains(err.Error(), "operation rejected") || output == "" {
		t.Fatalf("declined: err=%v output=%q", err, output)
	}
}
