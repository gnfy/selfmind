package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// A plan may be logged, stored, or one day sent to another execution node.
// Environment values must therefore be structurally incapable of riding along.
func TestSandboxPlanSerializationCarriesNoEnvironment(t *testing.T) {
	material := execMaterial{
		WritableRoots:    []string{"/work/ws"},
		ReadOnlyPaths:    []string{"/home/u/.kube/config"},
		ScratchTmp:       "/home/u/.selfmind/runtime/leases/lease-1/tmp",
		Env:              []string{"PATH=/usr/bin", "GCLOUD_TOKEN=super-secret", "HOME=/home/u"},
		Profiles:         []string{"gcloud"},
		ProfileNotes:     []string{"credentials withheld"},
		CopiedStateFiles: 6,
		SnapshotID:       "envsnap_1_abcd",
		Generation:       1,
		ScratchHandle:    "lease-1",
	}
	plan := planFromMaterial(material, SandboxDecision{Mode: SandboxIsolated, NetworkShared: true})

	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(encoded)
	for _, forbidden := range []string{"super-secret", "GCLOUD_TOKEN", "PATH=", "HOME="} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("serialized plan leaked environment state (%q): %s", forbidden, rendered)
		}
	}
	// The handle, not a node-local absolute path, is what durable state keeps.
	if plan.ScratchHandle != "lease-1" {
		t.Fatalf("plan must carry the scratch HANDLE, got %q", plan.ScratchHandle)
	}
	if strings.Contains(rendered, material.ScratchTmp) {
		t.Fatalf("plan must not embed a node-local scratch path: %s", rendered)
	}
	if plan.Version != SandboxPlanVersion || plan.Backend != bubblewrapBackendName || plan.NetworkMode != "shared" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	// A plan with an empty binding cannot be audited or verified by whoever runs
	// it, so the identity fields must actually be populated.
	if plan.SnapshotID != "envsnap_1_abcd" || plan.Generation != 1 {
		t.Fatalf("plan must name its environment binding, got %+v", plan)
	}
}

// ProcessMaterial is the value that DOES carry the environment, so it must not
// be serializable and must hand out copies only.
func TestProcessMaterialDoesNotSerializeAndCopies(t *testing.T) {
	material := ProcessMaterial{env: []string{"PATH=/usr/bin", "TOKEN=secret"}, scratchTmp: "/scratch"}
	encoded, err := json.Marshal(material)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("ProcessMaterial must serialize to nothing, got %s", encoded)
	}
	copied := material.Env()
	copied[0] = "PATH=/tampered"
	if material.Env()[0] != "PATH=/usr/bin" {
		t.Fatal("Env must return a defensive copy")
	}
}

func TestSandboxBackendSelection(t *testing.T) {
	if SandboxBackendName(SandboxIsolated) != bubblewrapBackendName {
		t.Fatal("isolated mode must select the bubblewrap backend")
	}
	if SandboxBackendName(SandboxHost) != hostBackendName {
		t.Fatal("host mode must select the host backend")
	}
	// The host backend is always available; that is what makes it the fallback.
	if !backendHost.Available() {
		t.Fatal("host backend must always be available")
	}
}

// The reason a command left the sandbox is what makes "avoidable host escapes"
// measurable; the raw host share mixes a deliberate login with a defect.
func TestClassifyHostEscape(t *testing.T) {
	host := SandboxDecision{Mode: SandboxHost}
	cases := []struct {
		name     string
		programs []string
		payload  string
		want     string
	}{
		{"interactive login", []string{"gcloud"}, "gcloud auth login", HostEscapeLogin},
		{"gui", []string{"xdg-open"}, "xdg-open report.html", HostEscapeGUI},
		{"host write", []string{"tee"}, "sudo tee /etc/hosts", HostEscapeHostWrite},
		{"isolated mode has no reason", nil, "gcloud builds list", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := host
			if tc.want == "" {
				decision = SandboxDecision{Mode: SandboxIsolated}
			}
			if got := classifyHostEscape(decision, tc.programs, tc.payload); got != tc.want {
				t.Fatalf("classifyHostEscape = %q, want %q", got, tc.want)
			}
		})
	}
	// On a host that can isolate, an unexplained escape is the avoidable kind —
	// that is the number worth driving to zero.
	if ExecSandboxAvailable() {
		if got := classifyHostEscape(host, []string{"gcloud"}, "gcloud builds list"); got != HostEscapeSandboxGap {
			t.Fatalf("unexplained escape must classify as a sandbox gap, got %q", got)
		}
	}
}

// Recovery is stage-based and bounded. "Produced output" is the decidable proxy
// for "already had effects": a command that printed anything is never replayed.
func TestShouldRecoverExecution(t *testing.T) {
	isolated := SandboxDecision{Mode: SandboxIsolated}
	withProfile := execMaterial{Profiles: []string{"gcloud"}}
	denial := errors.New("command failed: exit status 1")
	denialOutput := ""

	if !shouldRecoverExecution(isolated, withProfile, 1,
		errors.New("command failed: read-only file system on /home/u/.config/gcloud"), denialOutput, context.Background()) {
		t.Fatal("a silent state-write denial with a matching profile must be recoverable once")
	}
	if shouldRecoverExecution(isolated, withProfile, 1,
		errors.New("command failed: read-only file system"), "partial results already printed", context.Background()) {
		t.Fatal("a command that produced output must never be replayed")
	}
	if shouldRecoverExecution(SandboxDecision{Mode: SandboxHost}, withProfile, 1, denial, denialOutput, context.Background()) {
		t.Fatal("host execution has no sandbox state to re-prepare")
	}
	if shouldRecoverExecution(isolated, execMaterial{}, 1,
		errors.New("command failed: read-only file system"), denialOutput, context.Background()) {
		t.Fatal("without a matching profile, re-preparing cannot change the outcome")
	}
	if shouldRecoverExecution(isolated, withProfile, 127, errors.New("command failed: not found"), denialOutput, context.Background()) {
		t.Fatal("a shell-level rejection is not a sandbox denial")
	}
	if shouldRecoverExecution(isolated, withProfile, 1, nil, denialOutput, context.Background()) {
		t.Fatal("a successful command must not be retried")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if shouldRecoverExecution(isolated, withProfile, 1,
		errors.New("command failed: read-only file system"), denialOutput, cancelled) {
		t.Fatal("a cancelled turn must not start another attempt")
	}
}

// The execution policy must be resolvable PER REQUEST: a single process-wide
// setting cannot serve executions that need different boundaries.
func TestExecSandboxPolicyPerRequest(t *testing.T) {
	enabled, required, network := execSandboxPolicy()
	SetExecSandbox(false, false, false)
	t.Cleanup(func() { SetExecSandbox(enabled, required, network) })

	tenant := "tenant-policy-" + t.Name()
	cleanup := SetExecutionScope(tenant, ExecutionScope{
		TenantID:      tenant,
		WorkspaceRoot: t.TempDir(),
		SandboxPolicy: &ExecSandboxPolicy{Enabled: true, Required: true, AllowNetwork: true},
	})
	t.Cleanup(cleanup)

	args := map[string]interface{}{"_tenant_id": tenant}
	gotEnabled, gotRequired, gotNetwork := execSandboxPolicyForArgs(args)
	if !gotEnabled || !gotRequired || !gotNetwork {
		t.Fatalf("the request's own policy must win: %v %v %v", gotEnabled, gotRequired, gotNetwork)
	}
	// A call with no scope keeps the process default.
	if e, r, n := execSandboxPolicyForArgs(nil); e || r || n {
		t.Fatalf("a call with no scope must fall back to the process policy: %v %v %v", e, r, n)
	}
}

// A run-scoped key resolves exactly its own scope, which is what a process
// serving several executions requires.
func TestExecutionScopeResolvesByRunKey(t *testing.T) {
	person := "person-scope-key"
	first := SetExecutionScope(person, ExecutionScope{
		TenantID: person, RunID: "run-A", WorkspaceRoot: t.TempDir(), TaskID: "task-A",
	})
	t.Cleanup(first)
	// A later run for the same person overwrites the person-keyed entry...
	second := SetExecutionScope(person, ExecutionScope{
		TenantID: person, RunID: "run-B", WorkspaceRoot: t.TempDir(), TaskID: "task-B",
	})
	t.Cleanup(second)

	// ...but a caller that knows its run still resolves its own scope.
	ctx := WithExecutionScopeKey(context.Background(), ExecutionScopeKeyForRun("run-A"))
	scope, ok := currentExecutionScopeAny(map[string]interface{}{"_tenant_id": person, "_context": ctx})
	if !ok || scope.TaskID != "task-A" {
		t.Fatalf("run-scoped lookup must return run A's scope, got %+v", scope)
	}
	// Without a run key the person-keyed entry (the newest) is used, as before.
	scope, ok = currentExecutionScopeAny(map[string]interface{}{"_tenant_id": person})
	if !ok || scope.TaskID != "task-B" {
		t.Fatalf("person lookup must return the newest scope, got %+v", scope)
	}
}

// The engine must have ONE production entry, or "unified" is only true of the
// types. This asserts the property mechanically: nothing outside Execute may
// construct a sandboxed command.
func TestExecuteIsTheOnlySandboxConstructionSite(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	offenders := make([]string, 0)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, readErr := os.ReadFile(name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "sandboxedCommandWithMaterial(") {
				continue
			}
			if strings.Contains(line, "func sandboxedCommandWithMaterial") {
				continue
			}
			// exec_sandbox.go keeps one legacy adapter used only by tests.
			if name == "execution_engine.go" || name == "exec_sandbox.go" {
				continue
			}
			offenders = append(offenders, name+": "+strings.TrimSpace(line))
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("only Execute may build a sandboxed command; found:\n%s", strings.Join(offenders, "\n"))
	}
}

// Execute must be reachable for every shape of execution the engine serves: a
// shell command, an argv, and a daemon-owned durable check.
func TestExecuteAcceptsEveryRequestShape(t *testing.T) {
	workspace := t.TempDir()
	shell, err := Execute(context.Background(), ExecutionRequest{
		ToolName: "terminal", Payload: "printf shell-shape", Shell: true,
		CWD: workspace, WorkspaceRoots: []string{workspace}, Sandbox: SandboxAuto,
		Timeout: 20 * time.Second, ToolProfile: ToolProfile{Class: ToolExecutionStandard},
	}, nil)
	if err != nil || !strings.Contains(shell.Output, "shell-shape") {
		t.Fatalf("shell request did not produce its output\n%s", describeExecution(shell, err))
	}
	if shell.Plan.Version != SandboxPlanVersion || shell.Plan.Mode == "" {
		t.Fatalf("every execution must produce a plan: %+v", shell.Plan)
	}

	argv, err := Execute(context.Background(), ExecutionRequest{
		ToolName: "execute_code", Command: []string{"printf", "argv-shape"},
		CWD: workspace, WorkspaceRoots: []string{workspace}, Sandbox: SandboxAuto,
		Timeout: 20 * time.Second, ToolProfile: ToolProfile{Class: ToolExecutionStandard},
	}, nil)
	if err != nil || !strings.Contains(argv.Output, "argv-shape") {
		t.Fatalf("argv request did not produce its output\n%s", describeExecution(argv, err))
	}

	durable, err := Execute(context.Background(), ExecutionRequest{
		ToolName: "watch_external", Payload: "printf durable-shape", Shell: true,
		CWD: workspace, WorkspaceRoots: []string{workspace}, Sandbox: SandboxAuto,
		Durable:     &DurableExecutionScope{TenantID: "t", PersonID: "p"},
		Timeout:     20 * time.Second,
		ToolProfile: ToolProfile{Class: ToolExecutionStandard},
	}, nil)
	if err != nil || !strings.Contains(durable.Output, "durable-shape") {
		t.Fatalf("durable request did not produce its output\n%s", describeExecution(durable, err))
	}
}

// describeExecution renders everything Execute reports about one run. The
// original assertion printed only the error and the output, so a CI failure
// where err was nil and output was empty said nothing at all about WHY: not
// the exit code, not whether the sandbox was used or escaped, not whether
// recovery ran. Every field below is already on ExecutionResult; the test just
// was not showing them.
func describeExecution(result ExecutionResult, err error) string {
	return fmt.Sprintf(
		"  err:                %v\n"+
			"  exit_code:          %d\n"+
			"  output (%3d bytes): %q\n"+
			"  failure_class:      %q\n"+
			"  plan.version:       %d\n"+
			"  plan.mode:          %q\n"+
			"  host_escape_reason: %q\n"+
			"  recovery_attempted: %v\n"+
			"  recovery_outcome:   %q\n"+
			"  profiles_matched:   %v\n"+
			"  scratch_bytes:      %d\n"+
			"  sandbox_available:  %v",
		err, result.ExitCode, len(result.Output), result.Output, result.FailureClass,
		result.Plan.Version, result.Plan.Mode, result.HostEscapeReason,
		result.RecoveryAttempted, result.RecoveryOutcome, result.ProfilesMatched,
		result.ScratchBytes, ExecSandboxAvailable())
}
