package tools

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/executionenv"
)

// fakeGcloudHome builds a host state directory shaped like ~/.config/gcloud: a
// credential store the tool opens READ-WRITE even for a pure read (the exact
// reason a read-only mapping is not enough), plus a large logs/ directory that
// must never be copied.
func fakeGcloudHome(t *testing.T, base string) (home string, stateDir string, checksum string) {
	t.Helper()
	home = filepath.Join(base, "home")
	stateDir = filepath.Join(home, ".config", "gcloud")
	_ = os.MkdirAll(home, 0o755)
	if err := os.MkdirAll(filepath.Join(stateDir, "configurations"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "logs", "2026.07.29"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "credentials.db"), []byte("account=zerogu\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "active_config"), []byte("default\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "configurations", "config_default"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The 231 MB in miniature: excluded by the profile, so it must not appear in
	// the overlay and must not count against the copy bounds.
	if err := os.WriteFile(filepath.Join(stateDir, "logs", "2026.07.29", "big.log"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, stateDir, stateChecksum(t, stateDir)
}

func stateChecksum(t *testing.T, dir string) string {
	t.Helper()
	sum := sha256.New()
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, _ := filepath.Rel(dir, path)
		fmt.Fprintf(sum, "%s:%d:%x\n", rel, info.Mode().Perm(), sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sum.Sum(nil))
}

// fakeGcloudOnPath installs a script named `gcloud` that behaves like the real
// one in the way that matters: it OPENS ITS CREDENTIAL STORE FOR WRITING before
// answering a read-only question.
func fakeGcloudOnPath(t *testing.T, base string) string {
	t.Helper()
	// NOT t.TempDir(): isolated execution binds the run scratch at /tmp, which
	// shadows every real path beneath it — a fixture there would vanish inside
	// the sandbox exactly as $SELFMIND_RUN_TMP would.
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -e
config="${CLOUDSDK_CONFIG:-$HOME/.config/gcloud}"
# Real gcloud fails here when the directory is read-only:
#   ERROR: Unable to create private file [.../credentials.db]: Read-only file system
printf 'touched\n' >> "$config/credentials.db"
mkdir -p "$config/logs"
printf 'log\n' >> "$config/logs/run.log"
printf '%s\n' "$(head -n 1 "$config/credentials.db")"
`
	path := filepath.Join(bin, "gcloud")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// fixtureBase returns a per-test directory outside /tmp, so nothing a test
// depends on can be shadowed by the scratch bind at /tmp.
func fixtureBase(t *testing.T) string {
	t.Helper()
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory outside /tmp")
	}
	base := filepath.Join(realHome, ".selfmind-test-runtime", "fixture-"+t.Name())
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	return base
}

func profileExecScope(t *testing.T, home, extraPath, trust string, capabilities ...string) (tenant, workspace string) {
	t.Helper()
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory for a runtime root outside /tmp")
	}
	root := filepath.Join(realHome, ".selfmind-test-runtime", "profile-"+t.Name())
	if err := executionenv.SetRuntimeRoot(root); err != nil {
		t.Skipf("runtime root unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })

	snapshot := registry.Install([]string{
		"PATH=" + extraPath + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"HOME=" + home,
	}, "inherited", "principal-test", nil)
	registry.BindLease("lease-"+t.Name(), snapshot.ID)

	workspace = filepath.Join(filepath.Dir(home), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	tenant = "tenant-profile-" + t.Name()
	cleanup := SetExecutionScope(tenant, ExecutionScope{
		TenantID:      tenant,
		PersonID:      "person-profile",
		WorkspaceID:   "ws-profile",
		WorkspaceRoot: workspace,
		AllowedRoots:  []string{workspace},
		LeaseID:       "lease-" + t.Name(),
		TrustLevel:    trust,
		Capabilities:  capabilities,
		ApprovalMode:  ApprovalFullAuto,
	})
	t.Cleanup(cleanup)
	return tenant, workspace
}

// The headline failure: 26 sandboxed commands died writing their own state
// directory. With the state overlay the same command succeeds on FIRST use and
// the operator's real directory stays byte-identical.
func TestOperatorStateOverlayLetsToolWriteItsOwnStateIsolated(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("bubblewrap or unprivileged user namespaces unavailable")
	}
	enabled, required, network := execSandboxPolicy()
	SetExecSandbox(true, false, true)
	t.Cleanup(func() { SetExecSandbox(enabled, required, network) })

	base := fixtureBase(t)
	home, stateDir, before := fakeGcloudHome(t, base)
	tenant, workspace := profileExecScope(t, home, fakeGcloudOnPath(t, base), executionenv.TrustTrusted)

	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant,
		"command":    "gcloud auth list",
		"cwd":        workspace,
		"sandbox":    string(SandboxIsolated),
		"timeout":    30,
	})
	if err != nil {
		t.Fatalf("gcloud must succeed on first use inside the sandbox: %v\n%s", err, out)
	}
	if !strings.Contains(out, "account=zerogu") {
		t.Fatalf("the overlay must carry the real credential state: %q", out)
	}
	if after := stateChecksum(t, stateDir); after != before {
		t.Fatal("the operator's own credential directory must stay byte-identical")
	}
	// The excluded logs directory must not have been copied.
	overlay := filepath.Join(executionenv.RuntimeRoot(), "leases", "lease-"+t.Name(), "state", "gcloud")
	if _, err := os.Stat(filepath.Join(overlay, "logs", "2026.07.29", "big.log")); err == nil {
		t.Fatal("excluded logs must not be copied into the overlay")
	}
	// The tool's own writes landed in the overlay, not on the host.
	data, err := os.ReadFile(filepath.Join(overlay, "credentials.db"))
	if err != nil || !strings.Contains(string(data), "touched") {
		t.Fatalf("tool writes must land in the overlay: %v %q", err, data)
	}
}

// Second command of the same run must reuse the overlay, including a token the
// first command refreshed into it — copying again would discard it.
func TestOperatorStateOverlayIsCopiedOncePerRun(t *testing.T) {
	base := fixtureBase(t)
	home, _, _ := fakeGcloudHome(t, base)
	tenant, workspace := profileExecScope(t, home, fakeGcloudOnPath(t, base), executionenv.TrustTrusted)

	for i := 0; i < 2; i++ {
		if out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
			"_tenant_id": tenant,
			"command":    "gcloud auth list",
			"cwd":        workspace,
			"timeout":    30,
		}); err != nil {
			t.Fatalf("call %d failed: %v (%s)", i+1, err, out)
		}
	}
	overlay := filepath.Join(executionenv.RuntimeRoot(), "leases", "lease-"+t.Name(), "state", "gcloud")
	data, err := os.ReadFile(filepath.Join(overlay, "credentials.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Two appends survive: the overlay was not re-copied over the tool's writes.
	if strings.Count(string(data), "touched") != 2 {
		t.Fatalf("overlay must persist across commands of one run, got %q", data)
	}
}

// An untrusted workspace without credential:read gets the redirects but no
// credential copy, so the tool reports its own state instead of a read-only
// filesystem error — and the reason is recorded, not silent.
func TestUntrustedWorkspaceWithholdsOperatorCredentials(t *testing.T) {
	base := fixtureBase(t)
	home, _, _ := fakeGcloudHome(t, base)
	tenant, workspace := profileExecScope(t, home, fakeGcloudOnPath(t, base), executionenv.TrustUntrusted)

	args := map[string]interface{}{
		"_tenant_id": tenant,
		"_tool_name": "terminal",
		"command":    "gcloud auth list",
		"cwd":        workspace,
		"timeout":    30,
	}
	material := execMaterialForArgs(args, workspace)
	if material.ProfileError != nil {
		t.Fatal(material.ProfileError)
	}
	if len(material.ProfileNotes) == 0 {
		t.Fatal("withholding operator credentials must be reported")
	}
	if !strings.Contains(strings.Join(material.ProfileNotes, " "), "credential:read") {
		t.Fatalf("the note must name the capability that would grant access: %v", material.ProfileNotes)
	}
	overlay := filepath.Join(executionenv.RuntimeRoot(), "leases", "lease-"+t.Name(), "state", "gcloud")
	if _, err := os.Stat(filepath.Join(overlay, "credentials.db")); err == nil {
		t.Fatal("an untrusted workspace must not receive the operator credential store")
	}
	// The redirect still applies, so the tool writes into the empty overlay.
	if !strings.Contains(strings.Join(material.Env, " "), "CLOUDSDK_CONFIG="+overlay) {
		t.Fatalf("the redirect must still point into the run overlay: %v", material.Env)
	}
}

// With credential:read granted, an untrusted workspace gets the same overlay as
// a trusted one — the capability is the documented way in.
func TestUntrustedWorkspaceWithCredentialReadGetsOverlay(t *testing.T) {
	base := fixtureBase(t)
	home, _, _ := fakeGcloudHome(t, base)
	tenant, workspace := profileExecScope(t, home, fakeGcloudOnPath(t, base),
		executionenv.TrustUntrusted, executionenv.CapabilityCredentialRead)

	material := execMaterialForArgs(map[string]interface{}{
		"_tenant_id": tenant,
		"_tool_name": "terminal",
		"command":    "gcloud auth list",
		"cwd":        workspace,
	}, workspace)
	if material.ProfileError != nil {
		t.Fatal(material.ProfileError)
	}
	overlay := filepath.Join(executionenv.RuntimeRoot(), "leases", "lease-"+t.Name(), "state", "gcloud")
	if _, err := os.Stat(filepath.Join(overlay, "credentials.db")); err != nil {
		t.Fatalf("credential:read must materialize the overlay: %v", err)
	}
}

// kubectl authenticates through whatever its kubeconfig names. A GKE kubeconfig
// goes through gcloud — that indirection is why GKE kubectl calls failed while
// aws calls succeeded — so the gcloud overlay must be prepared for it. A kubectl
// pointed at any other cluster must NOT pay for Google credentials.
func TestKubectlPullsInGcloudOnlyForGKE(t *testing.T) {
	base := fixtureBase(t)
	home, _, _ := fakeGcloudHome(t, base)
	kubeDir := filepath.Join(home, ".kube")
	if err := os.MkdirAll(kubeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tenant, workspace := profileExecScope(t, home, fakeGcloudOnPath(t, base), executionenv.TrustTrusted)

	args := func() map[string]interface{} {
		return map[string]interface{}{
			"_tenant_id": tenant,
			"_tool_name": "terminal",
			"command":    "set -euo pipefail\nkubectl get ns",
			"cwd":        workspace,
		}
	}

	// A plain kubeconfig: only the generic Kubernetes profile applies.
	if err := os.WriteFile(filepath.Join(kubeDir, "config"),
		[]byte("users:\n- user:\n    client-certificate-data: abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	material := execMaterialForArgs(args(), workspace)
	if material.ProfileError != nil {
		t.Fatal(material.ProfileError)
	}
	if joined := strings.Join(material.Profiles, ","); joined != "kubernetes" {
		t.Fatalf("a non-GKE kubectl must not drag in gcloud, got %q", joined)
	}
	if strings.Contains(strings.Join(material.Env, " "), "CLOUDSDK_CONFIG=") {
		t.Fatal("a non-GKE kubectl must not receive a gcloud redirect")
	}

	// A GKE kubeconfig: the exec plugin is named, so gcloud is prepared.
	if err := os.WriteFile(filepath.Join(kubeDir, "config"),
		[]byte("users:\n- user:\n    exec:\n      command: gke-gcloud-auth-plugin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	material = execMaterialForArgs(args(), workspace)
	if material.ProfileError != nil {
		t.Fatal(material.ProfileError)
	}
	joined := strings.Join(material.Profiles, ",")
	if !strings.Contains(joined, "gcloud") || !strings.Contains(joined, "kubernetes") {
		t.Fatalf("a GKE kubectl must apply both profiles, got %q", joined)
	}
	env := strings.Join(material.Env, " ")
	for _, want := range []string{"CLOUDSDK_CONFIG=", "KUBECACHEDIR="} {
		if !strings.Contains(env, want) {
			t.Fatalf("missing redirect %q in %q", want, env)
		}
	}
}

// Program discovery must see every program in a compound command, not just the
// leading word — the defect class that produced `command:set` grant keys.
func TestExecCommandProgramsSkipsBuiltinsAndControlFlow(t *testing.T) {
	cases := []struct {
		command string
		want    []string
	}{
		{"set -euo pipefail\ngcloud builds list", []string{"gcloud"}},
		{"for t in a b; do kubectl get ns; done", []string{"kubectl"}},
		{"cd /tmp && gcloud auth list | jq .", []string{"gcloud", "jq"}},
		{"echo hi", nil},
	}
	for _, tc := range cases {
		got := execCommandPrograms("terminal", map[string]interface{}{
			"_tool_name": "terminal",
			"command":    tc.command,
		})
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("execCommandPrograms(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

// `credential:read` used to be a constant that was only ever READ: nothing could
// grant it, so an untrusted workspace's only way to run a credential CLI was to
// trust the whole workspace — the all-or-nothing escalation the capability exists
// to avoid. The middleware must resolve it BEFORE execution, because deciding
// after the failure would make the tool report a login problem that is not real.
func TestCredentialReadCapabilityIsApprovableBeforeExecution(t *testing.T) {
	base := fixtureBase(t)
	home, _, _ := fakeGcloudHome(t, base)
	tenant, workspace := profileExecScope(t, home, fakeGcloudOnPath(t, base), executionenv.TrustUntrusted)

	asked := 0
	store := &recordingCapabilityStore{}
	cleanup := SetExecutionScope(tenant, ExecutionScope{
		TenantID:        tenant,
		PersonID:        "person-profile",
		WorkspaceID:     "ws-profile",
		WorkspaceRoot:   workspace,
		AllowedRoots:    []string{workspace},
		LeaseID:         "lease-" + t.Name(),
		TrustLevel:      executionenv.TrustUntrusted,
		ApprovalMode:    ApprovalFullAuto,
		CapabilityStore: store,
		Approval: func(_ context.Context, req ToolApprovalRequest) (ToolApprovalDecision, error) {
			asked++
			if req.ToolName != executionenv.CapabilityCredentialRead {
				t.Fatalf("unexpected approval for %q", req.ToolName)
			}
			if req.GrantClass == "" {
				t.Fatal("the ask must describe what it authorizes")
			}
			return ToolApprovalDecision{Approved: true, Scope: "person",
				ExpiresAt: time.Now().Add(time.Hour)}, nil
		},
	})
	t.Cleanup(cleanup)

	args := map[string]interface{}{
		"_tenant_id": tenant,
		"_tool_name": "terminal",
		"command":    "gcloud auth list",
		"cwd":        workspace,
	}
	resolveCredentialCapability(args, mustScope(t, args), "terminal")
	if asked != 1 {
		t.Fatalf("expected exactly one ask, got %d", asked)
	}
	if allowed, _ := args[credentialReadArgKey].(bool); !allowed {
		t.Fatal("an approved capability must allow credential access")
	}
	if store.granted != executionenv.CapabilityCredentialRead {
		t.Fatalf("the grant must be persisted, got %q", store.granted)
	}
	// With the decision in place the overlay materializes, which is the whole
	// point: the command now runs with credentials instead of failing.
	material := execMaterialForArgs(args, workspace)
	if material.ProfileError != nil {
		t.Fatal(material.ProfileError)
	}
	overlay := filepath.Join(executionenv.RuntimeRoot(), "leases", "lease-"+t.Name(), "state", "gcloud")
	if _, err := os.Stat(filepath.Join(overlay, "credentials.db")); err != nil {
		t.Fatalf("an approved credential:read must materialize the overlay: %v", err)
	}
}

// A command that touches no credential CLI must never trigger the ask.
func TestCredentialReadIsNotAskedForUnrelatedCommands(t *testing.T) {
	base := fixtureBase(t)
	home, _, _ := fakeGcloudHome(t, base)
	tenant, workspace := profileExecScope(t, home, fakeGcloudOnPath(t, base), executionenv.TrustUntrusted)
	asked := 0
	cleanup := SetExecutionScope(tenant, ExecutionScope{
		TenantID: tenant, PersonID: "person-profile", WorkspaceID: "ws-profile",
		WorkspaceRoot: workspace, AllowedRoots: []string{workspace},
		LeaseID: "lease-" + t.Name(), TrustLevel: executionenv.TrustUntrusted,
		Approval: func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
			asked++
			return ToolApprovalDecision{Approved: true}, nil
		},
	})
	t.Cleanup(cleanup)
	args := map[string]interface{}{
		"_tenant_id": tenant, "_tool_name": "terminal",
		"command": "printf hello", "cwd": workspace,
	}
	resolveCredentialCapability(args, mustScope(t, args), "terminal")
	if asked != 0 {
		t.Fatalf("a command with no credential CLI must not ask, got %d asks", asked)
	}
}

// A trusted workspace already has access and must not be interrupted.
func TestCredentialReadIsNotAskedInTrustedWorkspace(t *testing.T) {
	base := fixtureBase(t)
	home, _, _ := fakeGcloudHome(t, base)
	tenant, workspace := profileExecScope(t, home, fakeGcloudOnPath(t, base), executionenv.TrustTrusted)
	args := map[string]interface{}{
		"_tenant_id": tenant, "_tool_name": "terminal",
		"command": "gcloud auth list", "cwd": workspace,
	}
	scope := mustScope(t, args)
	scope.Approval = func(context.Context, ToolApprovalRequest) (ToolApprovalDecision, error) {
		t.Fatal("a trusted workspace must not be asked")
		return ToolApprovalDecision{}, nil
	}
	resolveCredentialCapability(args, scope, "terminal")
	if allowed, _ := args[credentialReadArgKey].(bool); !allowed {
		t.Fatal("a trusted workspace must be allowed without asking")
	}
}

func mustScope(t *testing.T, args map[string]interface{}) ExecutionScope {
	t.Helper()
	scope, ok := currentExecutionScopeAny(args)
	if !ok {
		t.Fatal("execution scope should be installed")
	}
	return scope
}

type recordingCapabilityStore struct{ granted string }

func (s *recordingCapabilityStore) HasExecutionCapability(context.Context, string, string, string, string, string) (bool, error) {
	return false, nil
}

func (s *recordingCapabilityStore) GrantExecutionCapability(_ context.Context, _, _, _, capability, _, _ string, _ time.Time) error {
	s.granted = capability
	return nil
}
