package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/tools"
	"selfmind/internal/tools/sandbox"
)

// A continuation restores the authority its parent was admitted with — mode,
// origin, strategy, standing-grant cutoff — and none of that is the execution
// boundary. The boundary is decided at dispatch from the policy in force and
// the call itself, and pinned: a continuation restored into a looser mode than
// the person has now is not thereby let past it.

// terminalCall dispatches one terminal call through the production order —
// capability resolution, approval, the real terminal tool — in the scope of the
// Run the caller is running inside. between runs after approval and before the
// tool starts.
type terminalCall struct {
	args map[string]interface{}
	out  string
	err  error
}

func dispatchTerminal(personID, runID, cwd, command, sandbox string, between func()) terminalCall {
	call := terminalCall{args: map[string]interface{}{
		"_tenant_id": personID, "_tool_name": "terminal", "command": command, "cwd": cwd, "timeout": 30,
	}}
	if runID != "" {
		call.args["_invocation_scope"] = kernel.ToolInvocationScope{ExecutionScopeKey: tools.ExecutionScopeKeyForRun(runID)}
	}
	if sandbox != "" {
		call.args["sandbox"] = sandbox
	}
	body := func(args map[string]interface{}) (string, error) {
		if between != nil {
			between()
		}
		return tools.NewExecuteCommandTool().Execute(args)
	}
	call.out, call.err = tools.ExecutionCapabilityMiddleware()(tools.SmartApprovalMiddleware("")(body))(call.args)
	return call
}

func (c terminalCall) sandboxMode() string {
	mode, _ := c.args["_sandbox_mode"].(string)
	return mode
}

// An explicit per-run full-auto Run that parks continues under that mode even
// though the person's own preference is stricter: the preference is older than
// the work, so it does not narrow it. Restoring that looser mode does not let
// the continuation's calls past the policy the operator enforces: an explicit
// host request is refused at dispatch when isolation is required, and an
// ordinary call keeps running inside the sandbox.
func TestRestoredContinuationMeetsTheSandboxPolicyAtDispatch(t *testing.T) {
	for _, tc := range []struct {
		name              string
		enabled, required bool
		hostRuns          bool
	}{
		{name: "isolation-required-after-admission", enabled: true, required: true},
		// The constraint that must change the result: with the sandbox off the
		// same restored full-auto continuation reaches the host.
		{name: "sandbox-off", hostRuns: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdmissionProbeServer(t, 1)
			ctx := context.Background()
			dir := t.TempDir()
			hostMarker, autoMarker := filepath.Join(dir, "host.marker"), filepath.Join(dir, "auto.marker")
			var hostCall, autoCall terminalCall
			// Inside the continuation's own model call, in the scope its Run
			// installed from the restored admission.
			s.provider.during = func(call int) {
				if call != 2 {
					return
				}
				hostCall = dispatchTerminal(s.identity.PersonID, "", dir, "touch "+hostMarker, "host", nil)
				autoCall = dispatchTerminal(s.identity.PersonID, "", dir, "touch "+autoMarker, "", nil)
			}
			// The person's own preference is stricter, and older than this work.
			s.setModeAt(t, "smart", time.Now().Unix()-3600)
			withSandboxPolicy(t, tc.enabled, tc.required, false)
			reply, code := s.daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
				Channel: "session-pin", ClientCWD: dir, Content: "tidy the design notes", ApprovalMode: "full-auto"})
			if code != 200 || reply.Outcome == nil || reply.Outcome.CompletionReason != "provider_wait" {
				t.Fatalf("parked turn = %d %+v", code, reply)
			}
			child := s.drainDue(t, "provider-wait:"+reply.Run.ID)
			if resumed := s.provider.probe(t, 2); resumed.judged {
				t.Fatalf("fixture: the continuation must run under its explicit full-auto mode, not the person's older smart one: %+v", resumed)
			}
			if got := s.admission(t, child.ID); got.ApprovalMode != "full-auto" {
				t.Fatalf("fixture: continuation admission = %+v", got)
			}

			if tc.hostRuns {
				if hostCall.err != nil || hostCall.sandboxMode() != string(tools.SandboxHost) {
					t.Fatalf("with the sandbox off the host call must run on the host: mode=%q err=%v (%s)", hostCall.sandboxMode(), hostCall.err, hostCall.out)
				}
				if _, err := os.Stat(hostMarker); err != nil {
					t.Fatal("the host call did not run")
				}
				return
			}
			if hostCall.err == nil {
				t.Fatalf("a restored full-auto continuation reached the host past a required sandbox: %s", hostCall.out)
			}
			if code := notDispatchedCode(t, hostCall.err); code != "sandbox_isolation_unavailable" {
				t.Fatalf("the refusal must say isolation is required, got %s: %v", code, hostCall.err)
			}
			if _, err := os.Stat(hostMarker); err == nil {
				t.Fatal("the refused host call ran")
			}
			if !tools.ExecSandboxAvailable() {
				return
			}
			if autoCall.err != nil || autoCall.sandboxMode() != string(tools.SandboxIsolated) {
				t.Fatalf("an ordinary call keeps running inside the sandbox: mode=%q err=%v (%s)", autoCall.sandboxMode(), autoCall.err, autoCall.out)
			}
		})
	}
}

// In full-auto nobody reviews a call's in-place credential writes: the only
// guard is the write set the approval funnel recorded, which dispatch holds the
// plan to. A continuation restored into full-auto from its parked parent is the
// Run that leans on it hardest, so it must meet the same pin: a plan that grew
// since the funnel decided is refused before anything is created, and an
// isolated boundary the backend can no longer enforce is never run on the host.
func TestRestoredFullAutoContinuationMeetsTheApprovedWritesAndBoundaryAtDispatch(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		config   string
		between  func(t *testing.T, host *awsHost)
		wantCode string
	}{
		{
			// The funnel saw a static configuration, so it approved no in-place
			// writes; the host's configuration then gained an SSO profile.
			name: "write-set-grows-after-the-funnel-decided", config: awsStaticConfig,
			between:  func(t *testing.T, host *awsHost) { host.writeConfig(t, awsSSOConfig) },
			wantCode: "approved_host_writes_changed",
		},
		{
			// The funnel approved isolation with its two caches; then the SSO
			// cache became a link out of its declared root, which no subpath
			// write can express.
			name: "state-escapes-its-root-after-the-funnel-decided", config: awsSSOConfig,
			between: func(t *testing.T, host *awsHost) {
				outside := filepath.Join(filepath.Dir(host.home), "elsewhere")
				if err := os.MkdirAll(outside, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(host.sso), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, host.sso); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: "sandbox_plan_unsupported",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newAWSHost(t, "continuation-"+tc.name, tc.config)
			server, store, identity, _, _ := newClarifyTestServer(t)
			server.ExecutionRuntime = tools.NewExecutionRuntime("", executionenv.RuntimeRoot(), sandbox.PrivateState{}, nil)
			withSandboxPolicy(t, true, false, false)
			// The parked parent was admitted in full-auto; the person's own mode
			// is on-request now.
			if err := store.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, personSettingApprovalMode, "on-request"); err != nil {
				t.Fatal(err)
			}
			parent := control.AdmissionSnapshot{Version: control.AdmissionSnapshotVersion, ApprovalMode: "full-auto",
				Strategy: control.AdmissionStrategy{Class: "general_task", ToolMode: "full", PlanPolicy: "optional", WebPolicy: "disabled", ChannelMode: "interactive"},
				Input:    control.AdmissionInput{Summary: "deploy the service"}}
			run := &control.Run{ID: "run-continuation-" + strings.ReplaceAll(tc.name, "_", "-"), Channel: "cli"}
			workspace := &control.Workspace{ID: "workspace", LocalPath: host.workspace, TrustLevel: executionenv.TrustTrusted}
			cleanup := server.coordinator().installExecutionScope(ctx, identity, nil, run, workspace,
				underAdmission(api.MessageRequest{Channel: "cli"}, parent), &host.lease)
			defer cleanup()

			// A person asked here would mean the restore did not apply the
			// parent's mode; fail instead of waiting for an answer.
			finished := make(chan terminalCall, 1)
			go func() {
				finished <- dispatchTerminal(identity.PersonID, run.ID, host.workspace, awsCommand, "", func() { tc.between(t, host) })
			}()
			var call terminalCall
			select {
			case call = <-finished:
			case <-time.After(30 * time.Second):
				t.Fatal("the continuation waited on a person: the parked full-auto mode was not restored")
			}
			if call.err == nil || notDispatchedCode(t, call.err) != tc.wantCode {
				t.Fatalf("want a %s refusal that started nothing, got err=%v (%s)", tc.wantCode, call.err, call.out)
			}
			if call.sandboxMode() == string(tools.SandboxHost) {
				t.Fatalf("an approved isolated boundary ran on the host: %s", call.out)
			}
			if tc.wantCode == "approved_host_writes_changed" {
				host.requireNoCache(t, "writes the approval never covered were made")
			}
			if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(host.home), "elsewhere")); len(entries) != 0 {
				t.Fatalf("the escaping target was written: %v", entries)
			}
		})
	}

	// The constraint that must change the result: nothing changes between the
	// funnel and dispatch, so the same restored continuation is released without
	// a person and writes exactly the caches it was shown.
	host := newAWSHost(t, "continuation-unchanged", awsSSOConfig)
	server, _, identity, _, _ := newClarifyTestServer(t)
	server.ExecutionRuntime = tools.NewExecutionRuntime("", executionenv.RuntimeRoot(), sandbox.PrivateState{}, nil)
	withSandboxPolicy(t, true, false, false)
	parent := control.AdmissionSnapshot{Version: control.AdmissionSnapshotVersion, ApprovalMode: "full-auto",
		Strategy: control.AdmissionStrategy{Class: "general_task", ToolMode: "full", PlanPolicy: "optional", WebPolicy: "disabled", ChannelMode: "interactive"},
		Input:    control.AdmissionInput{Summary: "deploy the service"}}
	run := &control.Run{ID: "run-continuation-unchanged", Channel: "cli"}
	workspace := &control.Workspace{ID: "workspace", LocalPath: host.workspace, TrustLevel: executionenv.TrustTrusted}
	cleanup := server.coordinator().installExecutionScope(ctx, identity, nil, run, workspace,
		underAdmission(api.MessageRequest{Channel: "cli"}, parent), &host.lease)
	defer cleanup()
	call := dispatchTerminal(identity.PersonID, run.ID, host.workspace, awsCommand, "", nil)
	if call.err != nil || !strings.Contains(call.out, "cache write ok") || call.sandboxMode() != string(tools.SandboxIsolated) {
		t.Fatalf("an unchanged plan must run isolated: mode=%q err=%v (%s)", call.sandboxMode(), call.err, call.out)
	}
	if _, err := os.Stat(filepath.Join(host.sso, "stub.json")); err != nil {
		t.Fatalf("the approved SSO cache was not written: %v", err)
	}
}
