package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/kernel"
)

// Every call records why it did or did not get the network, on the paths that
// leave the capability decision before the command and on those that return
// early.
func TestEveryExecutionCallRecordsItsNetworkDecision(t *testing.T) {
	t.Run("host execution shares the daemon's network and says so", func(t *testing.T) {
		h := newNetworkHarness(t, networkHarnessOptions{})
		args, _, err := h.call("go test ./...", "sandbox", "host", "_effective_sandbox_mode", string(SandboxHost))
		if err != nil || argDecision(args) != executionenv.NetworkDecisionShared {
			t.Fatalf("host call: decision=%q err=%v", argDecision(args), err)
		}
	})
	t.Run("a call outside any workspace follows the operator policy", func(t *testing.T) {
		for _, tc := range []struct {
			allow bool
			want  string
		}{{true, executionenv.NetworkDecisionShared}, {false, executionenv.NetworkWithheldPolicyDisabled}} {
			withExecSandboxPolicy(t, true, false, tc.allow)
			executor := ExecutionCapabilityMiddleware()(func(map[string]interface{}) (string, error) { return "ok", nil })
			args := map[string]interface{}{"_tenant_id": "no-such-person", "_tool_name": "terminal", "command": "go test ./...", "_effective_sandbox_mode": string(SandboxIsolated)}
			if _, err := executor(args); err != nil || argDecision(args) != tc.want {
				t.Fatalf("allow=%v: decision=%q err=%v", tc.allow, argDecision(args), err)
			}
		}
	})
}

// The engine puts the decision on the sandbox event and on the typed process
// facts of the result, which is what durable evidence and the daily summaries
// read.
func TestEngineRecordsTheNetworkDecisionOnEventAndFacts(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("enforced sandbox unavailable on this host")
	}
	withExecSandboxPolicy(t, true, false, true)
	for _, tc := range []struct {
		name     string
		shared   bool
		decision string
		perm     string
		network  string
	}{
		{"withheld although permitted", false, executionenv.NetworkWithheldNotBound, executionenv.NetworkPermissionGrant, "isolated"},
		{"withheld without a grant", false, executionenv.NetworkWithheldNoGrant, "", "isolated"},
		{"shared", true, executionenv.NetworkDecisionShared, executionenv.NetworkPermissionTrust, "shared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := make(chan string, 64)
			args := map[string]interface{}{
				"command": "printf recorded", "cwd": t.TempDir(), "_tool_call_id": "call-network-evidence",
				"_context":                kernel.WithEventChannel(context.Background(), events),
				"_effective_sandbox_mode": string(SandboxIsolated),
				"_network_shared":         tc.shared, "_network_decision": tc.decision, "_network_permission": tc.perm,
			}
			result, err := NewExecuteCommandTool().ExecuteResult(args)
			if err != nil || !strings.Contains(result.Output, "recorded") {
				t.Fatalf("command: %q %v", result.Output, err)
			}
			if result.Process == nil || result.Process.NetworkDecision != tc.decision || result.Process.NetworkPermission != tc.perm || result.Process.NetworkMode != tc.network {
				t.Fatalf("process facts = %+v", result.Process)
			}
			close(events)
			recorded := false
			for event := range events {
				if strings.Contains(event, `"tool.sandbox"`) && strings.Contains(event, `"network_decision":"`+tc.decision+`"`) {
					recorded = true
				}
			}
			if !recorded {
				t.Fatalf("the sandbox event does not record %q", tc.decision)
			}
		})
	}
}

// The whole path a provider call takes: a string "true" is coerced to the
// declared boolean, the capability middleware decides, and the process facts of
// the real command say which way it went.
func TestDispatchedTerminalCarriesTheRequestDecisionAndFacts(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("enforced sandbox unavailable on this host")
	}
	withExecSandboxPolicy(t, true, false, true)
	root := t.TempDir()
	person := "net-dispatch-terminal"
	cleanup := SetExecutionScope(person, ExecutionScope{
		TenantID: "t", PersonID: person, RunID: "run-" + person, WorkspaceID: "w", WorkspaceRoot: root, AllowedRoots: []string{root},
		TrustLevel: executionenv.TrustTrusted, SandboxPolicy: &ExecSandboxPolicy{Enabled: true, AllowNetwork: true},
	})
	defer cleanup()
	registry := NewRegistry()
	registry.UseMiddleware(ExecutionCapabilityMiddleware())
	registry.Register(NewExecuteCommandTool())
	run := func(network interface{}) *kernel.ToolProcessResult {
		t.Helper()
		args := map[string]interface{}{"command": "printf ok", "cwd": root, "_tenant_id": person}
		if network != nil {
			args["network"] = network
		}
		result, err := registry.DispatchResult("terminal", args)
		if err != nil || result.Process == nil || !strings.Contains(result.Output, "ok") {
			t.Fatalf("dispatch: %+v %v", result, err)
		}
		return result.Process
	}
	offline := run(nil)
	if offline.NetworkMode != "isolated" || offline.NetworkDecision != executionenv.NetworkWithheldNotBound || offline.NetworkPermission != executionenv.NetworkPermissionTrust {
		t.Fatalf("a local command in a trusted workspace: %+v", offline)
	}
	for _, requested := range []interface{}{true, "true"} {
		networked := run(requested)
		if networked.NetworkMode != "shared" || networked.NetworkDecision != executionenv.NetworkDecisionShared || networked.NetworkPermission != executionenv.NetworkPermissionTrust {
			t.Fatalf("network=%v: %+v", requested, networked)
		}
	}
	if declined := run("false"); declined.NetworkMode != "isolated" {
		t.Fatalf(`network="false" was read as a request: %+v`, declined)
	}
}

// The model reads it too: a passing check that ran offline although a network
// grant permits it, and whose command can fetch dependencies, says so in its own
// result, before the model relies on the pass. Nothing else gets the sentence:
// not a check that cannot fetch anything (vet, lint, formatting), whatever it ran.
func TestPassingCheckResultSaysWhenItRanOfflineDespiteAGrant(t *testing.T) {
	zero := 0
	run := func(tool, command string, process kernel.ToolProcessResult, runErr error) string {
		t.Helper()
		events := make(chan string, 4)
		ctx := kernel.WithEventChannel(context.Background(), events)
		exec := EvidenceMiddleware()(func(map[string]interface{}) (kernel.ToolDispatchResult, error) {
			process.Started, process.ExitCode = true, &zero
			return kernel.ToolDispatchResult{Output: "ok", Process: &process}, runErr
		})
		result, _ := exec(map[string]interface{}{"_context": ctx, "_tool_name": tool, "_tool_call_id": "check", "command": command, "cwd": "/workspace", "kind": "test"})
		return result.Output
	}
	offline := kernel.ToolProcessResult{NetworkDecision: executionenv.NetworkWithheldNotBound, NetworkPermission: executionenv.NetworkPermissionGrant}
	for _, command := range []string{"go test ./...", "cargo build", "npm test", "cd web && yarn build", "mvn test"} {
		out := run("verify", command, offline, nil)
		for _, want := range []string{"without network", "network grant permits", "network: true"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%q result does not say %q: %q", command, want, out)
			}
		}
	}
	// The same permitted-and-offline pass says nothing when the command has no way
	// to fetch: the sentence would be noise a reader learns to skip.
	for _, command := range []string{"go vet ./...", "gofmt -l .", "golangci-lint run", "cargo fmt --check", "tsc --noEmit", "pytest -q", "make lint", "grep -r TODO ."} {
		if out := run("verify", command, offline, nil); strings.Contains(out, "without network") {
			t.Errorf("%q cannot fetch dependencies but its pass carries the offline sentence: %q", command, out)
		}
	}
	for name, tc := range map[string]struct {
		tool    string
		process kernel.ToolProcessResult
		err     error
	}{
		"networked":      {"verify", kernel.ToolProcessResult{NetworkDecision: executionenv.NetworkDecisionShared, NetworkPermission: executionenv.NetworkPermissionGrant}, nil},
		"trusted":        {"verify", kernel.ToolProcessResult{NetworkDecision: executionenv.NetworkWithheldNotBound, NetworkPermission: executionenv.NetworkPermissionTrust}, nil},
		"no permission":  {"verify", kernel.ToolProcessResult{NetworkDecision: executionenv.NetworkWithheldNoGrant}, nil},
		"failed check":   {"verify", offline, errors.New("command failed: exit status 1")},
		"ordinary shell": {"terminal", offline, nil},
	} {
		if out := run(tc.tool, "go test ./...", tc.process, tc.err); strings.Contains(out, "without network") {
			t.Errorf("%s: unrelated result carries the offline sentence: %q", name, out)
		}
	}
}

// Host execution carries the daemon's network by construction; the record says
// shared even though no capability decision was involved.
func TestHostExecutionRecordsSharedNetworkWithoutACapabilityDecision(t *testing.T) {
	withExecSandboxPolicy(t, false, false, false)
	result, err := Execute(context.Background(), ExecutionRequest{
		ToolName: "terminal", Payload: "printf host-shape", Shell: true, CWD: t.TempDir(), Sandbox: SandboxAuto,
		ToolProfile: ToolProfile{Class: ToolExecutionStandard},
	}, nil)
	if err != nil || result.NetworkDecision != executionenv.NetworkDecisionShared {
		t.Fatalf("host decision = %q err=%v", result.NetworkDecision, err)
	}
}

// terminal, verify and execute_code declare the request, with one meaning: it
// asks and never grants.
func TestExecutionToolsDeclareTheNetworkRequest(t *testing.T) {
	for _, tool := range []Tool{NewExecuteCommandTool(), NewVerifyTool(), NewExecuteCodeTool()} {
		property, ok := tool.Schema().Properties["network"]
		if !ok || property.Type != "boolean" {
			t.Fatalf("%s does not declare network:boolean: %+v", tool.Name(), property)
		}
		for _, want := range []string{"network access", "permission", "grant", "approval"} {
			if !strings.Contains(strings.ToLower(property.Description), want) {
				t.Errorf("%s network description does not say %q: %s", tool.Name(), want, property.Description)
			}
		}
		payload := "command"
		if tool.Name() == "execute_code" {
			payload = "code"
		}
		if err := ValidateArgs(tool.Schema(), map[string]interface{}{payload: "go test ./...", "network": true}); err != nil {
			t.Fatalf("%s rejects its own parameter: %v", tool.Name(), err)
		}
		if !networkRequestDeclaredBy(tool.Name()) {
			t.Errorf("%s declares network but the capability middleware strips it", tool.Name())
		}
	}
	// The closed verify schema still refuses what it does not declare.
	if err := ValidateArgs(NewVerifyTool().Schema(), map[string]interface{}{"command": "go test ./...", "networks": true}); err == nil {
		t.Fatal("verify accepted an undeclared parameter")
	}
	// The model is told when to use it: cold caches and dependency downloads.
	description := NewExecuteCommandTool().Schema().Properties["network"].Description + " " + NewVerifyTool().Schema().Properties["network"].Description + " " + NewVerifyTool().Description()
	for _, want := range []string{"dependenc", "offline"} {
		if !strings.Contains(strings.ToLower(description), want) {
			t.Errorf("the tool descriptions never mention %q", want)
		}
	}
}

// The system-prompt note tells the model how network behaves now: shared with
// commands that reach a remote service, requested with network: true.
func TestPromptNoteDescribesNetworkAsPermissionAndRequest(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("enforced sandbox unavailable on this host")
	}
	for _, allow := range []bool{true, false} {
		withExecSandboxPolicy(t, true, false, allow)
		note := ExecSandboxPromptNote()
		for _, want := range []string{"network: true", "remote service"} {
			if !strings.Contains(note, want) {
				t.Errorf("allow_network=%v note does not say %q: %s", allow, want, note)
			}
		}
	}
}
