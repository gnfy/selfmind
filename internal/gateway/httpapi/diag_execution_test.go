package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/tools"
)

func TestExecutionDiagIsRedactedAndShowsWorkspace(t *testing.T) {
	store := controltest.NewStore(t)
	ctx := context.Background()
	identity := &control.IdentityContext{
		TenantID: "tenant-exec-diag",
		PersonID: "person-exec-diag",
		Platform: "cli",
	}
	if _, err := store.EnsureWorkspace(ctx, control.Workspace{
		TenantID:      identity.TenantID,
		OwnerPersonID: identity.PersonID,
		Name:          "project",
		LocalPath:     "/work/project",
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{Control: store}
	handled, reply, _, err := server.tryHandleControlCommand(ctx, identity, api.MessageRequest{
		Channel: "cli:person-exec-diag",
		Content: "/diag execution",
	})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	for _, want := range []string{
		"Execution diagnostics",
		"Sandbox backend:",
		"Process environment:",
		"Workspace: project",
		"Workspace trust: untrusted",
		"Writable root: /work/project",
		"Workspace capabilities: none",
		"Environment lease: none",
		"Credential values: hidden",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply missing %q:\n%s", want, reply)
		}
	}
}

// /diag execution states, per credential profile on this host, whether the
// platform's sandbox can run it and which host paths a sandboxed run writes —
// paths only, never what the configuration holds.
func TestExecutionDiagShowsCredentialProfileBoundaries(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := "[profile sso-dev]\nsso_session=dev\n[sso-session dev]\nsso_start_url=https://example.awsapps.com/start\n"
	if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	registry.Install([]string{"PATH=/usr/bin:/bin", "HOME=" + home}, "inherited", "principal-diag", nil)
	enabled, required, _ := tools.ExecSandboxStatus()
	network := tools.ExecSandboxAllowsNetwork()
	tools.SetExecSandbox(true, false, network)
	t.Cleanup(func() { tools.SetExecSandbox(enabled, required, network) })

	store := controltest.NewStore(t)
	identity := &control.IdentityContext{TenantID: "tenant-profile-diag", PersonID: "person-profile-diag", Platform: "cli"}
	server := &Server{Control: store}
	handled, reply, _, err := server.tryHandleControlCommand(context.Background(), identity, api.MessageRequest{
		Channel: "cli:person-profile-diag", Content: "/diag execution",
	})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(reply, "Credential profiles on this host") {
		t.Fatalf("reply lacks the per-profile boundary section:\n%s", reply)
	}
	var awsLine string
	for _, line := range strings.Split(reply, "\n") {
		if strings.HasPrefix(line, "- aws: ") {
			awsLine = line
		}
	}
	if awsLine == "" {
		t.Fatalf("the host's AWS profile is not reported:\n%s", reply)
	}
	root, _ := filepath.EvalSymlinks(filepath.Join(home, ".aws"))
	backend := tools.SandboxBackendName(tools.SandboxIsolated)
	if !tools.ExecSandboxAvailable() {
		backend = "none"
		if !strings.Contains(awsLine, "not sandbox-runnable") {
			t.Fatalf("without an isolation backend the profile cannot be sandbox-runnable: %q", awsLine)
		}
	}
	switch backend {
	case "seatbelt":
		if !strings.Contains(awsLine, "in place") || !strings.Contains(awsLine, filepath.Join(root, "sso", "cache")) ||
			!strings.Contains(awsLine, filepath.Join(root, "cli", "cache")) {
			t.Fatalf("seatbelt must name the in-place subpaths: %q", awsLine)
		}
	case "bubblewrap":
		if !strings.Contains(awsLine, "overlay") {
			t.Fatalf("bubblewrap must name its private overlays: %q", awsLine)
		}
	}
	if !strings.Contains(reply, "- gh: no state on this host") {
		t.Fatalf("a profile without host state must say so:\n%s", reply)
	}
	if strings.Contains(reply, "awsapps") || strings.Contains(reply, "sso_session") {
		t.Fatalf("configuration content leaked into diagnostics:\n%s", reply)
	}

	// With exec_sandbox disabled, commands run on the host: nothing may be
	// reported as sandbox-runnable.
	tools.SetExecSandbox(false, false, network)
	_, reply, _, err = server.tryHandleControlCommand(context.Background(), identity, api.MessageRequest{
		Channel: "cli:person-profile-diag", Content: "/diag execution",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reply, ": sandbox-runnable") || !strings.Contains(reply, "- aws: not sandbox-runnable: ") {
		t.Fatalf("a disabled sandbox must not be reported as runnable:\n%s", reply)
	}
}

func TestExecutionDiagDoesNotCallMultipleRunsIdle(t *testing.T) {
	store := controltest.NewStore(t)
	identity := &control.IdentityContext{TenantID: "tenant-multi-diag", PersonID: "person-multi-diag", Platform: "cli"}
	server := &Server{Control: store}
	coord := server.coordinator()
	coord.activeLimit = 2
	for _, runID := range []string{"run-one", "run-two"} {
		handle := &activeRun{PersonID: identity.PersonID, RunID: runID, Channel: runID}
		if !coord.beginActive(identity.PersonID, handle) {
			t.Fatal("could not register active run")
		}
		defer coord.endActiveRun(identity.PersonID, handle)
	}
	handled, reply, _, err := server.tryHandleControlCommand(context.Background(), identity, api.MessageRequest{
		Channel: "cli:person-multi-diag", Content: "/diag execution",
	})
	if err != nil || !handled || !strings.Contains(reply, "2 active runs") || strings.Contains(reply, "no active run") {
		t.Fatalf("multi-run execution diagnostics: handled=%v err=%v reply=%q", handled, err, reply)
	}
}

func TestExecutionDiagShowsOnlyOwnersUnresolvedExternalEffects(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	owner := &control.IdentityContext{TenantID: "default", PersonID: "person-owner", Platform: "cli"}
	other := &control.IdentityContext{TenantID: "default", PersonID: "person-other", Platform: "cli"}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: owner.TenantID, PersonID: owner.PersonID, Title: "deploy", Channel: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRunWithOptions(ctx, task, "owner", "deploy", control.StartRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimExternalEffects(ctx, control.ExternalEffectClaimRequest{
		TenantID: owner.TenantID, PersonID: owner.PersonID, RunID: run.ID,
		EffectID: "effect-one", TargetKeys: []string{control.UnknownExternalTarget},
	}); err != nil || !claimed.Granted {
		t.Fatalf("setup claim: %+v, %v", claimed, err)
	}
	server := &Server{Control: store}
	for _, tc := range []struct {
		identity *control.IdentityContext
		want     string
	}{
		{owner, "unknown target (person-wide)"},
		{other, "External effects unresolved: none"},
	} {
		handled, reply, _, err := server.tryHandleControlCommand(ctx, tc.identity, api.MessageRequest{Content: "/diag execution"})
		if err != nil || !handled || !strings.Contains(reply, tc.want) {
			t.Fatalf("diagnostic for %s: handled=%v err=%v reply=%q", tc.identity.PersonID, handled, err, reply)
		}
		if tc.identity == other && strings.Contains(reply, "person-wide") {
			t.Fatalf("another person's claim leaked into diagnostics: %s", reply)
		}
	}
}
