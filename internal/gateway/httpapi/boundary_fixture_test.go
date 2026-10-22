package httpapi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools"
)

// A host whose AWS configuration makes a sandbox backend that cannot mount tool
// state write the SSO and CLI credential caches in place on the real host. The
// boundary the person approved then includes those writes, which is what the
// tests below hold a poll or a continuation to.

const awsSSOConfig = "[default]\nregion=us-east-1\n" +
	"[profile sso-dev]\nsso_session=dev\nregion=us-east-1\n" +
	"[sso-session dev]\nsso_start_url=https://example.awsapps.com/start\nsso_region=us-east-1\n"

const awsStaticConfig = "[default]\nregion=us-east-1\n"

// awsCacheStub stands in for the AWS CLI refreshing the SSO token cache.
const awsCacheStub = "#!/bin/sh\n" +
	"cache=\"$HOME/.aws/sso/cache\"\n" +
	"mkdir -p \"$cache\" 2>/dev/null\n" +
	"if printf '{\"token\":\"stub\"}\\n' > \"$cache/stub.json\" 2>/dev/null; then echo 'cache write ok'; else echo 'cache write denied'; exit 4; fi\n"

const awsCommand = "aws sts get-caller-identity --profile sso-dev"

type awsHost struct {
	home, workspace string
	sso, cli        string
	snapshot        *executionenv.Snapshot
	lease           executionenv.Lease
}

func newAWSHost(t *testing.T, name, config string) *awsHost {
	t.Helper()
	if tools.SandboxBackendName(tools.SandboxIsolated) != "seatbelt" || !tools.ExecSandboxAvailable() {
		t.Skip("in-place credential writes exist only on a backend that cannot mount")
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory for a runtime root outside the temp directory")
	}
	base := filepath.Join(realHome, ".selfmind-test-runtime", "httpapi-boundary-"+name)
	home, bin, workspace := filepath.Join(base, "home"), filepath.Join(base, "bin"), filepath.Join(base, "workspace")
	for _, dir := range []string{filepath.Join(home, ".aws"), bin, workspace} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(awsCacheStub), 0o755); err != nil {
		t.Fatal(err)
	}
	host := &awsHost{home: home, workspace: workspace}
	host.writeConfig(t, config)

	previousRoot := executionenv.RuntimeRoot()
	if err := executionenv.SetRuntimeRoot(filepath.Join(base, "runtime")); err != nil {
		t.Skipf("runtime root unavailable: %v", err)
	}
	t.Cleanup(func() {
		if previousRoot != "" {
			_ = executionenv.SetRuntimeRoot(previousRoot)
		}
	})
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	host.snapshot = registry.Install([]string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home}, "inherited", "principal-"+name, nil)
	host.lease = executionenv.Lease{
		ID: "lease-" + name, EnvironmentSnapshotID: host.snapshot.ID, EnvironmentGeneration: host.snapshot.Generation,
		PrincipalFingerprint: host.snapshot.PrincipalFingerprint, EnvironmentFingerprint: host.snapshot.EnvironmentFingerprint,
		CredentialSourceHash: host.snapshot.CredentialSourceHash,
	}
	registry.BindLease(host.lease.ID, host.snapshot.ID)

	root, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	host.sso = filepath.Join(root, ".aws", "sso", "cache")
	host.cli = filepath.Join(root, ".aws", "cli", "cache")
	return host
}

func (h *awsHost) writeConfig(t *testing.T, config string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.home, ".aws", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireNoCache fails when anything created credential state on the host.
func (h *awsHost) requireNoCache(t *testing.T, why string) {
	t.Helper()
	for _, path := range []string{h.sso, h.cli} {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("%s was created: %s", path, why)
		}
	}
}

// withSandboxPolicy installs the daemon's execution configuration for one test.
func withSandboxPolicy(t *testing.T, enabled, required, network bool) {
	t.Helper()
	previous := tools.CurrentExecSandboxPolicy()
	tools.SetExecSandbox(enabled, required, network)
	t.Cleanup(func() { tools.SetExecSandbox(previous.Enabled, previous.Required, previous.AllowNetwork) })
}

// notDispatchedCode returns the typed code of a refusal that started nothing.
func notDispatchedCode(t *testing.T, err error) string {
	t.Helper()
	var typed interface {
		ToolErrorCode() string
		ToolEffectState() string
	}
	if !errors.As(err, &typed) || typed.ToolEffectState() != "not_dispatched" {
		t.Fatalf("want a typed not_dispatched refusal, got %v", err)
	}
	return typed.ToolErrorCode()
}
