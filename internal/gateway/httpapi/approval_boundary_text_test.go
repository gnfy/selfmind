package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/tools"
)

// imApprovalFromFunnel runs the production approval funnel for one terminal
// call in a trusted workspace whose host holds the given AWS configuration, and
// renders the captured ask as the IM notification a person answers from.
func imApprovalFromFunnel(t *testing.T, name, config, command string) (tools.ToolApprovalRequest, string) {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	// The runtime root may not live under the system temp directory.
	scratchHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory for a runtime root outside the temp directory")
	}
	runtimeRoot := filepath.Join(scratchHome, ".selfmind-test-runtime", "im-approval-"+name)
	previousRoot := executionenv.RuntimeRoot()
	if err := executionenv.SetRuntimeRoot(runtimeRoot); err != nil {
		t.Skipf("runtime root unavailable: %v", err)
	}
	t.Cleanup(func() {
		if previousRoot != "" {
			_ = executionenv.SetRuntimeRoot(previousRoot)
		}
		_ = os.RemoveAll(runtimeRoot)
	})
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	snapshot := registry.Install([]string{"PATH=/usr/bin:/bin", "HOME=" + home}, "inherited", "principal-im", nil)
	lease := "lease-im-" + name
	registry.BindLease(lease, snapshot.ID)

	enabled, required, _ := tools.ExecSandboxStatus()
	network := tools.ExecSandboxAllowsNetwork()
	tools.SetExecSandbox(true, false, false)
	t.Cleanup(func() { tools.SetExecSandbox(enabled, required, network) })

	var asked []tools.ToolApprovalRequest
	tenant := "tenant-im-" + name
	t.Cleanup(tools.SetExecutionScope(tenant, tools.ExecutionScope{
		TenantID: tenant, PersonID: "person-im", TaskID: "task-im", RunID: "run-im-" + name,
		WorkspaceID: "ws-im", WorkspaceRoot: workspace, AllowedRoots: []string{workspace},
		LeaseID: lease, TrustLevel: executionenv.TrustTrusted, ApprovalMode: tools.ApprovalOnRequest,
		// A current explicit deny adds its own review fact, so the reason the
		// funnel gathers is longer than any surface's line.
		IntentSnapshot: func() tools.RunIntentSnapshot {
			return tools.RunIntentSnapshot{
				ModelAuthorization: true, RawUserText: "do not run commands yet",
				ExplicitDeny: []string{"do not run commands yet"},
				DenyScopes: []tools.DenyScope{{Marker: "do not", Clause: "do not run commands yet",
					Classes: []tools.OperationClass{tools.OpClassExec}, Resolved: true}},
			}
		},
		Approval: func(_ context.Context, request tools.ToolApprovalRequest) (tools.ToolApprovalDecision, error) {
			asked = append(asked, request)
			return tools.ToolApprovalDecision{Approved: false, ApprovalID: "apr-im"}, nil
		},
	}))
	exec := tools.ExecutionCapabilityMiddleware()(tools.SmartApprovalMiddleware("")(func(map[string]interface{}) (string, error) {
		return "ok", nil
	}))
	_, _ = exec(map[string]interface{}{"_tenant_id": tenant, "_tool_name": "terminal", "command": command, "cwd": workspace})
	if len(asked) != 1 {
		t.Fatalf("the funnel must ask exactly once, asked %d", len(asked))
	}
	request := asked[0]
	approval := control.ApprovalRequest{ActionType: "tool_call", Payload: mustJSON(map[string]interface{}{
		"tool": request.ToolName, "reason": request.Reason, "containment": request.Containment,
		"args": tools.ApprovalPersistentArgs(request.ToolName, request.Args),
	})}
	return request, approvalNotificationText(approval, "")
}

// M2: an IM approver never saw the containment summary, so in-place credential
// writes were invisible there; and the bounded reason could drop a trailing
// "runs on the host". Both facts lead the reason, so the IM text keeps them
// through its truncation.
func TestIMApprovalShowsTheBoundaryFacts(t *testing.T) {
	if tools.SandboxBackendName(tools.SandboxIsolated) != "seatbelt" || !tools.ExecSandboxAvailable() {
		t.Skip("in-place credential writes exist only on a backend that cannot mount")
	}
	// The review facts this call gathers outrun the IM line's 120 characters,
	// so a boundary fact appended after them would be cut off.
	const long = "NODE_OPTIONS=--require=./hook.js ./deploy.sh --stage production --region eu-west-1 --notify release-channel --wait 600 --verbose"
	const fact = "writes credential caches in place: ~/.aws/sso/cache, ~/.aws/cli/cache"
	for _, tc := range []struct{ name, config string }{
		{"sso", "[profile sso-dev]\nsso_session=dev\n[sso-session dev]\nsso_start_url=https://example.awsapps.com/start\n"},
		// Another authentication kind under another profile name.
		{"role", "[profile ops-admin]\nrole_arn=arn:aws:iam::123456789012:role/ops\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, text := imApprovalFromFunnel(t, tc.name, tc.config, long)
			if rest := strings.TrimPrefix(strings.TrimPrefix(request.Reason, fact), "; "); utf8.RuneCountInString(rest) <= 120 {
				t.Fatalf("fixture: the gathered review facts must outrun the IM line, got %d characters: %q", utf8.RuneCountInString(rest), rest)
			}
			if !strings.Contains(text, fact) {
				t.Fatalf("the IM approval must name the in-place writes:\n%s", text)
			}
		})
	}
}
