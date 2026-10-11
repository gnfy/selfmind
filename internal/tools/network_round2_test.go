package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
)

// networkChain is the production order for one exec call: the capability
// middleware decides the network, then the approval funnel (hard floor, mode,
// containment, judge, human ask) decides whether the call runs. It counts what
// each layer did, so a test can say which layer asked.
type networkChain struct {
	t        *testing.T
	person   string
	asks     []ToolApprovalRequest
	judged   int
	ran      []map[string]interface{}
	executor ToolExecutor
}

type networkChainOptions struct {
	trust  string
	mode   ApprovalMode
	store  ExecutionCapabilityStore
	policy *ExecSandboxPolicy
	judge  ApprovalJudge
	intent RunIntentSnapshot
	// answer decides every human ask; the default approves for the run.
	answer func(ToolApprovalRequest) ToolApprovalDecision
	// respond is the tool body.
	respond func(map[string]interface{}) (string, error)
}

func newNetworkChain(t *testing.T, opts networkChainOptions) *networkChain {
	t.Helper()
	withExecSandboxPolicy(t, true, false, true)
	if opts.trust == "" {
		opts.trust = executionenv.TrustUntrusted
	}
	if opts.mode == "" {
		opts.mode = ApprovalFullAuto
	}
	if opts.policy == nil {
		opts.policy = &ExecSandboxPolicy{Enabled: true, AllowNetwork: true}
	}
	if opts.answer == nil {
		opts.answer = func(ToolApprovalRequest) ToolApprovalDecision {
			return ToolApprovalDecision{Approved: true, Scope: "run"}
		}
	}
	if opts.respond == nil {
		opts.respond = func(map[string]interface{}) (string, error) { return "ok", nil }
	}
	person := "chain-" + strings.NewReplacer("/", "-", " ", "-", ",", "-").Replace(t.Name())
	c := &networkChain{t: t, person: person}
	scope := ExecutionScope{
		TenantID: "tenant-chain", PersonID: person, RunID: "run-" + person, WorkspaceID: "workspace-chain",
		TrustLevel: opts.trust, SandboxPolicy: opts.policy, CapabilityStore: opts.store,
		ApprovalMode: opts.mode, Judge: opts.judge,
		IntentSnapshot: func() RunIntentSnapshot { return opts.intent },
		Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			c.asks = append(c.asks, req)
			return opts.answer(req), nil
		},
	}
	t.Cleanup(SetExecutionScope(person, scope))
	c.executor = ExecutionCapabilityMiddleware()(SmartApprovalMiddleware("")(func(args map[string]interface{}) (string, error) {
		c.ran = append(c.ran, args)
		return opts.respond(args)
	}))
	return c
}

func (c *networkChain) call(tool, command string, extra ...interface{}) (map[string]interface{}, string, error) {
	c.t.Helper()
	args := map[string]interface{}{
		"_tenant_id": c.person, "_tool_name": tool, "_effective_sandbox_mode": string(SandboxIsolated),
	}
	if tool == "execute_code" {
		args["code"] = command
	} else {
		args["command"] = command
	}
	for i := 0; i+1 < len(extra); i += 2 {
		args[extra[i].(string)] = extra[i+1]
	}
	output, err := c.executor(args)
	return args, output, err
}

// asksFor counts the human asks about one thing: the network capability, or the
// tool call itself.
func (c *networkChain) asksFor(toolName string) int {
	n := 0
	for _, ask := range c.asks {
		if ask.ToolName == toolName {
			n++
		}
	}
	return n
}

// ---- 1. execute_code can retry with network: true -------------------------

// execute_code is an execution tool a command reaches a remote service from, and
// the typed retry tells the model to set network: true on it. The tool takes the
// same optional boolean as terminal and verify, with the same meaning: it asks,
// and never grants.
func TestExecuteCodeDeclaresTheNetworkRequest(t *testing.T) {
	tool := NewExecuteCodeTool()
	property, ok := tool.Schema().Properties["network"]
	if !ok || property.Type != "boolean" {
		t.Fatalf("execute_code does not declare network:boolean: %+v", property)
	}
	for _, want := range []string{"network access", "permission", "grant", "approval"} {
		if !strings.Contains(strings.ToLower(property.Description), want) {
			t.Errorf("execute_code network description does not say %q: %s", want, property.Description)
		}
	}
	if err := ValidateArgs(tool.Schema(), map[string]interface{}{"code": "print(1)", "network": true}); err != nil {
		t.Fatalf("execute_code rejects its own parameter: %v", err)
	}
	if !networkRequestedByCall("execute_code", map[string]interface{}{"network": true}) {
		t.Fatal("network: true on execute_code is not read as a request")
	}
	if networkRequestedByCall("execute_code", map[string]interface{}{"network": "yes"}) {
		t.Fatal("a value that is not a boolean true counted as a request")
	}
	// The description no longer sends the model elsewhere for a network it can
	// now ask for itself.
	if strings.Contains(tool.Description(), "use normal tool calls for files, network") {
		t.Fatalf("execute_code description still says network needs another tool: %s", tool.Description())
	}
	if !strings.Contains(tool.Description(), "network: true") {
		t.Fatalf("execute_code description does not mention network: true: %s", tool.Description())
	}
}

// The reviewed reproduction: Python that calls a remote service, run offline in a
// workspace that permits network, fails with a typed result that says "retry with
// network: true". That retry must be networked in every permission state, and
// stay offline without the flag.
func TestExecuteCodeRetryWithNetworkTrueIsNetworkedInEveryPermissionState(t *testing.T) {
	const code = "import requests\nprint(requests.get('https://example.test/data').status_code)"
	const offlineOutput = "requests.exceptions.ConnectionError: Temporary failure in name resolution"
	states := []struct {
		name        string
		opts        networkHarnessOptions
		permission  string
		asksOnFirst int
	}{
		{"trusted workspace", networkHarnessOptions{trust: executionenv.TrustTrusted}, executionenv.NetworkPermissionTrust, 0},
		{"stored grant", networkHarnessOptions{store: networkOnlyCapabilityStore{}}, executionenv.NetworkPermissionGrant, 0},
		// With no grant the person is asked once, after the failure; their yes is a
		// run grant, which the retry then uses.
		{"no grant, approved after the failure", networkHarnessOptions{}, executionenv.NetworkPermissionGrant, 1},
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			h := newNetworkHarness(t, state.opts)
			h.respond = func(args map[string]interface{}) (string, error) {
				if argShared(args) {
					return "200", nil
				}
				return offlineOutput, fmt.Errorf("command failed: exit status 1 (python3 -I script.py)")
			}
			_, _, err := h.callTool("execute_code", code)
			if err == nil || len(h.asks) != state.asksOnFirst || len(h.executed) != 1 {
				t.Fatalf("first attempt: err=%v asks=%d executed=%d", err, len(h.asks), len(h.executed))
			}
			if !strings.Contains(err.Error(), "network: true") {
				t.Fatalf("the failure does not tell the model how to retry: %v", err)
			}
			args, out, err := h.callTool("execute_code", code, "network", true)
			if err != nil || out != "200" || !argShared(args) || argDecision(args) != executionenv.NetworkDecisionShared {
				t.Fatalf("the retry the failure asked for stayed offline: out=%q shared=%v decision=%q err=%v", out, argShared(args), argDecision(args), err)
			}
			if argPermission(args) != state.permission {
				t.Fatalf("permission = %q, want %q", argPermission(args), state.permission)
			}
			if len(h.asks) != state.asksOnFirst {
				t.Fatalf("the retry asked again: %d asks", len(h.asks))
			}
			// The flag asks for one call. Without it the same code is offline again.
			if args, _, _ := h.callTool("execute_code", code); argShared(args) {
				t.Fatal("a later call without the flag was networked")
			}
		})
	}
}

// The variants: asked for up front without a grant, the person is asked once
// before the code runs; a refusal runs nothing; local code never borrows the
// grant; and an explicit false is not a request.
func TestExecuteCodeNetworkRequestFollowsTheSameRulesAsTerminal(t *testing.T) {
	t.Run("an up-front request without a grant asks once before it runs", func(t *testing.T) {
		h := newNetworkHarness(t, networkHarnessOptions{})
		args, _, err := h.callTool("execute_code", "print(1)", "network", true)
		if err != nil || !argShared(args) || len(h.asks) != 1 || h.asks[0].ToolName != executionenv.CapabilityNetworkShared || argPermission(args) != executionenv.NetworkPermissionApproval {
			t.Fatalf("request: shared=%v asks=%d permission=%q err=%v", argShared(args), len(h.asks), argPermission(args), err)
		}
		if _, kept := args["network"]; !kept {
			t.Fatal("execute_code lost its declared network argument")
		}
	})
	t.Run("a refusal runs nothing", func(t *testing.T) {
		h := newNetworkHarness(t, networkHarnessOptions{})
		h.answer = func(ToolApprovalRequest) ToolApprovalDecision { return ToolApprovalDecision{Approved: false} }
		if _, _, err := h.callTool("execute_code", "print(1)", "network", true); err == nil || len(h.executed) != 0 {
			t.Fatalf("a declined request ran: err=%v executed=%d", err, len(h.executed))
		}
	})
	t.Run("local code does not borrow a grant", func(t *testing.T) {
		h := newNetworkHarness(t, networkHarnessOptions{store: networkOnlyCapabilityStore{}})
		args, _, err := h.callTool("execute_code", "print(sum(range(10)))")
		if err != nil || argShared(args) || argDecision(args) != executionenv.NetworkWithheldNotBound {
			t.Fatalf("local code: shared=%v decision=%q err=%v", argShared(args), argDecision(args), err)
		}
		if args, _, _ = h.callTool("execute_code", "print(1)", "network", false); argShared(args) {
			t.Fatal("network:false was networked")
		}
	})
	t.Run("only tools that declare it read it", func(t *testing.T) {
		h := newNetworkHarness(t, networkHarnessOptions{store: networkOnlyCapabilityStore{}})
		args, _, err := h.callTool("shell", "go test ./...", "network", true)
		if err != nil || argShared(args) {
			t.Fatalf("a tool that does not declare it was networked by it: shared=%v err=%v", argShared(args), err)
		}
		if _, kept := args["network"]; kept {
			t.Fatalf("an undeclared network argument was left in place: %+v", args)
		}
	})
}

// ---- 2. a "no network" prohibition reaches every networked call -----------

func TestSharedNetworkIsTheNetworkOperationClass(t *testing.T) {
	has := func(args map[string]interface{}) bool {
		for _, class := range operationClassesFor(stringArg(args, "_tool_name"), args, false) {
			if class == OpClassNetwork {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want bool
	}{
		{"curl, as before", map[string]interface{}{"_tool_name": "terminal", "command": "curl https://example.test"}, true},
		{"npm install, networked by name", map[string]interface{}{"_tool_name": "terminal", "command": "npm install", "_network_shared": true}, true},
		{"git push, networked by name", map[string]interface{}{"_tool_name": "terminal", "command": "git push origin main", "_network_shared": true}, true},
		{"a local command networked by an explicit request", map[string]interface{}{"_tool_name": "terminal", "command": "go test ./...", "_network_shared": true, "network": true}, true},
		{"execute_code networked by an explicit request", map[string]interface{}{"_tool_name": "execute_code", "code": "print(1)", "_network_shared": true}, true},
		{"verify networked", map[string]interface{}{"_tool_name": "verify", "command": "go test ./...", "_network_shared": true}, true},
		{"a local command that stayed offline", map[string]interface{}{"_tool_name": "terminal", "command": "go test ./...", "_network_shared": false}, false},
		{"npm install withheld for want of a grant", map[string]interface{}{"_tool_name": "terminal", "command": "npm install", "_network_shared": false}, false},
		{"no decision recorded", map[string]interface{}{"_tool_name": "terminal", "command": "go test ./..."}, false},
		{"not an exec tool", map[string]interface{}{"_tool_name": "write_file", "path": "x", "_network_shared": true}, false},
	} {
		if got := has(tc.args); got != tc.want {
			t.Errorf("%s: network class = %v, want %v (%v)", tc.name, got, tc.want, operationClassesFor(stringArg(tc.args, "_tool_name"), tc.args, false))
		}
	}
}

// "Do not use the network" forces a human on npm install, git push and any call
// with network: true, exactly as it already did on curl; a local command is not
// touched by it.
func TestNoNetworkProhibitionReachesNetworkedCalls(t *testing.T) {
	intent := RunIntentSnapshot{RawUserText: "refactor the parser, do not use the network", Source: "direct",
		ExplicitDeny: []string{"do not"},
		DenyScopes:   []DenyScope{{Marker: "do not", Clause: "do not use the network", Classes: []OperationClass{OpClassNetwork}, Resolved: true}}}
	refuse := func(ToolApprovalRequest) ToolApprovalDecision { return ToolApprovalDecision{Approved: false} }
	for _, mode := range []ApprovalMode{ApprovalFullAuto, ApprovalSmart, ApprovalOnRequest} {
		for _, state := range []struct {
			name string
			opts networkChainOptions
		}{
			{"trusted", networkChainOptions{trust: executionenv.TrustTrusted}},
			{"stored grant", networkChainOptions{store: networkOnlyCapabilityStore{}}},
		} {
			for _, tc := range []struct {
				command string
				extra   []interface{}
			}{
				{"npm install", nil}, {"git push origin main", nil}, {"curl https://example.test", nil},
				{"go test ./...", []interface{}{"network", true}},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", mode, state.name, tc.command), func(t *testing.T) {
					opts := state.opts
					opts.mode, opts.intent, opts.answer = mode, intent, refuse
					if mode == ApprovalSmart {
						opts.judge = &fakeJudge{reply: "APPROVE"}
					}
					c := newNetworkChain(t, opts)
					_, _, err := c.call("terminal", tc.command, tc.extra...)
					if err == nil || len(c.ran) != 0 || c.asksFor("terminal") != 1 {
						t.Fatalf("a networked call went past the prohibition: err=%v ran=%d asks=%d", err, len(c.ran), c.asksFor("terminal"))
					}
				})
			}
			// The same prohibition does not stop what never reaches the network.
			t.Run(fmt.Sprintf("%s/%s/local", mode, state.name), func(t *testing.T) {
				opts := state.opts
				opts.mode, opts.intent, opts.answer = mode, intent, refuse
				if mode == ApprovalSmart {
					opts.judge = &fakeJudge{reply: "APPROVE"}
				}
				if mode == ApprovalOnRequest {
					t.Skip("on-request asks for every command")
				}
				if mode == ApprovalSmart && !ExecSandboxAvailable() {
					t.Skip("smart mode releases a local command only under an enforceable sandbox")
				}
				c := newNetworkChain(t, opts)
				if _, _, err := c.call("terminal", "go test ./..."); err != nil || len(c.ran) != 1 || c.asksFor("terminal") != 0 {
					t.Fatalf("a local command was stopped by a no-network prohibition: err=%v ran=%d asks=%d", err, len(c.ran), c.asksFor("terminal"))
				}
			})
		}
	}
}

// ---- 3. a decision is not re-typed as a withheld network -------------------

func TestDecisionsAndWaitsAreNotRetypedAsNetworkWithheld(t *testing.T) {
	text := "the sandbox network is disabled, so the effect cannot be verified"
	decisions := map[string]error{
		"triage deny":        rejectOperation(rejectionCodeTriage, "operation rejected: blocked by safety triage: "+text),
		"person said no":     rejectOperation(rejectionCodeApproval, "operation rejected: "+text),
		"capability refused": rejectOperation(rejectionCodeCapability, "operation rejected: network:shared was not approved ("+text+")"),
		"parked approval":    &runPauseError{cause: errors.New("approval is pending: " + text), reason: "approval", message: text, needApproval: true},
		"not dispatched": newStableToolRecoveryError(errors.New("preparation refused: "+text), "preparation_refused", "environment", text, "fix it",
			"preparation", "corrected_input", "not_dispatched", false),
		"a decision for the person": newStableToolRecoveryError(errors.New("held: "+text), "held", "environment", text, "wait",
			"authorization", "user_decision", "", false),
	}
	for name, decision := range decisions {
		for _, state := range []struct {
			name string
			opts networkHarnessOptions
		}{
			{"permitted", networkHarnessOptions{store: networkOnlyCapabilityStore{}}},
			{"no grant", networkHarnessOptions{}},
		} {
			t.Run(name+"/"+state.name, func(t *testing.T) {
				h := newNetworkHarness(t, state.opts)
				h.respond = func(map[string]interface{}) (string, error) { return "", decision }
				_, _, err := h.call("./scripts/check.sh")
				if !errors.Is(err, decision) && err != decision {
					t.Fatalf("the decision was replaced: %v", err)
				}
				var facts recoveryFacts
				if errors.As(err, &facts) && facts.ToolErrorCode() == networkWithheldCode {
					t.Fatalf("a decision was re-typed as network_withheld_by_policy: %v", err)
				}
				if strings.Contains(err.Error(), "Retry with network: true") || strings.Contains(err.Error(), "network:shared was approved") {
					t.Fatalf("a decision was given a retry instruction: %v", err)
				}
				if len(h.asks) != 0 {
					t.Fatalf("a decision triggered a network approval: %d asks", len(h.asks))
				}
			})
		}
	}
	// The same words in a failure the command itself produced are still a network
	// failure: only a decision or a wait is exempt.
	h := newNetworkHarness(t, networkHarnessOptions{store: networkOnlyCapabilityStore{}})
	h.respond = func(map[string]interface{}) (string, error) {
		return "curl: (6) Could not resolve host", errors.New("command failed: exit status 6\nerror_class: sandbox_no_network")
	}
	_, _, err := h.call("./scripts/check.sh")
	var facts recoveryFacts
	if err == nil || !errors.As(err, &facts) || facts.ToolErrorCode() != networkWithheldCode {
		t.Fatalf("the command's own network failure lost its typed result: %v", err)
	}
}

// ---- 4. the hard floor comes before the network ask ------------------------

func TestHardFloorIsCheckedBeforeTheNetworkAsk(t *testing.T) {
	for _, mode := range []ApprovalMode{ApprovalFullAuto, ApprovalSmart, ApprovalOnRequest, ApprovalReadOnly, ApprovalAutoEdit} {
		t.Run(string(mode), func(t *testing.T) {
			opts := networkChainOptions{mode: mode, intent: RunIntentSnapshot{RawUserText: "clean up", Source: "direct"}}
			if mode == ApprovalSmart {
				opts.judge = &fakeJudge{reply: "APPROVE"}
			}
			c := newNetworkChain(t, opts)
			_, _, err := c.call("terminal", "rm -rf /", "network", true)
			if err == nil || !strings.Contains(err.Error(), "blocked by safety policy") || len(c.ran) != 0 {
				t.Fatalf("the hard floor did not stop the call: err=%v ran=%d", err, len(c.ran))
			}
			if c.asksFor(executionenv.CapabilityNetworkShared) != 0 || len(c.asks) != 0 {
				t.Fatalf("a call the floor blocks first asked about network: %+v", c.asks)
			}
		})
	}
	t.Run("credential access is not asked about either", func(t *testing.T) {
		c := newNetworkChain(t, networkChainOptions{})
		if _, _, err := c.call("terminal", "gcloud projects list && rm -rf /"); err == nil || len(c.asks) != 0 || len(c.ran) != 0 {
			t.Fatalf("err=%v asks=%+v ran=%d", err, c.asks, len(c.ran))
		}
	})
	// The ask is still asked for a call the floor lets through: this is what the
	// ordering test changes.
	t.Run("an ordinary call still asks first", func(t *testing.T) {
		c := newNetworkChain(t, networkChainOptions{})
		if _, _, err := c.call("terminal", "rm -rf build", "network", true); err != nil || c.asksFor(executionenv.CapabilityNetworkShared) != 1 {
			t.Fatalf("network ask: err=%v asks=%d", err, c.asksFor(executionenv.CapabilityNetworkShared))
		}
	})
}

// ---- 5. the typed note says local steps already ran ------------------------

func TestWithheldNoteWarnsThatLocalStepsAlreadyRan(t *testing.T) {
	for name, message := range map[string]string{
		"typed result": newNetworkWithheldError(errors.New("command failed: exit status 1")).Error(),
	} {
		for _, want := range []string{"changes state before it reaches the network", "inspect that state first", "already ran", "again"} {
			if !strings.Contains(message, want) {
				t.Errorf("%s does not say %q: %s", name, want, message)
			}
		}
	}
	// The ask-after-failure path leaves the same retry; it carries the same care.
	h := newNetworkHarness(t, networkHarnessOptions{})
	h.respond = networkFailureFor("./scripts/release.sh", "dial tcp: lookup example.test: no such host")
	_, _, err := h.call("./scripts/release.sh")
	if err == nil || !strings.Contains(err.Error(), "inspect that state first") {
		t.Fatalf("the approval-after-failure message does not warn about repeated local steps: %v", err)
	}
	// And the note still says what it said before.
	for _, want := range []string{"withheld by policy", "not replayed", "network: true"} {
		if !strings.Contains(newNetworkWithheldError(errors.New("x")).Error(), want) {
			t.Errorf("the note lost %q", want)
		}
	}
}

// ---- 8. the explicit flag alone does not release an observation ------------

func TestExplicitFlagAloneDoesNotReleaseAnObservationOnlyCall(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("containment requires an enforceable sandbox on this host")
	}
	withExecSandboxPolicy(t, true, true, true)
	assess := func(command string, extra ...interface{}) ContainmentAssessment {
		args := map[string]interface{}{"_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated), "_network_shared": true, "command": command}
		for i := 0; i+1 < len(extra); i += 2 {
			args[extra[i].(string)] = extra[i+1]
		}
		return assessExecContainment("terminal", args)
	}
	// The predicate says these are local; the network is shared only because the
	// call asked, and an ask is not a proof of observation.
	for _, command := range []string{"ls -la", "git status", "rg TODO .", "cat go.mod"} {
		got := assess(command, "network", true)
		if got.ObservationOnly || got.AutoApprove() || got.Network != containmentNetworkShared {
			t.Errorf("%q: an explicit network request released an observation: %+v", command, got)
		}
	}
	// A call whose network is explained by its command keeps the release it
	// always had, flag or no flag.
	for _, command := range []string{"git ls-remote origin", "kubectl get pods", "gh pr list"} {
		for _, extra := range [][]interface{}{nil, {"network", true}} {
			if got := assess(command, extra...); !got.ObservationOnly || !got.AutoApprove() {
				t.Errorf("%q %v: a call networked by its own command lost the observation release: %+v", command, extra, got)
			}
		}
	}
	// Without the flag the call is judged as before.
	if got := assess("ls -la"); !got.ObservationOnly || !got.AutoApprove() {
		t.Errorf("an observation that did not ask for network lost its release: %+v", got)
	}
	// Network shared by something else than the flag is not this case.
	if got := assess("ls -la", "network", false); !got.AutoApprove() {
		t.Errorf("network:false changed the assessment: %+v", got)
	}
}

// The same through the funnel: a smart-mode call that asks for the network is
// judged (or asked) instead of running unreviewed, and one that did not ask is
// released as before.
func TestExplicitFlagOnAnObservationIsReviewedInSmartMode(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("containment requires an enforceable sandbox on this host")
	}
	intent := RunIntentSnapshot{RawUserText: "look around", Source: "direct", ModelAuthorization: true}
	run := func(t *testing.T, extra ...interface{}) (judged int, asked int, ran bool) {
		judge := &fakeJudge{reply: `{"risk_level":"low","user_authorization":"high","outcome":"approve","rationale":"a read-only look"}`}
		c := newNetworkChain(t, networkChainOptions{mode: ApprovalSmart, store: networkOnlyCapabilityStore{}, judge: judge, intent: intent})
		if _, _, err := c.call("terminal", "ls -la", extra...); err != nil {
			t.Fatalf("call: %v", err)
		}
		return judge.calls, c.asksFor("terminal"), len(c.ran) == 1
	}
	if judged, asked, ran := run(t); judged != 0 || asked != 0 || !ran {
		t.Fatalf("a plain observation lost its release: judged=%d asked=%d ran=%v", judged, asked, ran)
	}
	if judged, asked, ran := run(t, "network", true); judged == 0 && asked == 0 || !ran {
		t.Fatalf("an explicit network request on an observation ran unreviewed: judged=%d asked=%d ran=%v", judged, asked, ran)
	}
}

// ---- 9. forged runtime decisions are cleared on every path -----------------

func TestCapabilityMiddlewareClearsForgedDecisionOnEveryEarlyReturn(t *testing.T) {
	forged := func(extra map[string]interface{}) map[string]interface{} {
		args := map[string]interface{}{
			"_tool_name": "terminal", "command": "go test ./...",
			"_network_decision": executionenv.NetworkDecisionShared, "_network_permission": executionenv.NetworkPermissionGrant,
		}
		for key, value := range extra {
			args[key] = value
		}
		return args
	}
	paths := map[string]map[string]interface{}{
		"an invalid sandbox mode":      forged(map[string]interface{}{"sandbox": "no-such-mode"}),
		"a sandbox plan refusal":       forged(map[string]interface{}{"_effective_sandbox_mode": string(SandboxIsolated), sandboxPlanRefusalArg: "isolation is required but unavailable"}),
		"host execution":               forged(map[string]interface{}{"sandbox": "host"}),
		"a call outside any workspace": forged(map[string]interface{}{"_effective_sandbox_mode": string(SandboxIsolated)}),
	}
	for name, args := range paths {
		t.Run(name, func(t *testing.T) {
			withExecSandboxPolicy(t, true, false, false)
			args["_tenant_id"] = "nobody-" + name
			executor := ExecutionCapabilityMiddleware()(func(map[string]interface{}) (string, error) { return "ok", nil })
			_, _ = executor(args)
			// Whatever the path left is the runtime's own record, never the
			// caller's: the forged grant permission is gone on all of them.
			if argPermission(args) == executionenv.NetworkPermissionGrant {
				t.Fatalf("the caller's network permission survived: %+v", args)
			}
			if decision := argDecision(args); decision == executionenv.NetworkDecisionShared && name != "host execution" {
				t.Fatalf("the caller's network decision survived: %+v", args)
			}
		})
	}
}
