package tools

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools/sandbox"
)

// The stub stands in for the AWS CLI in the two ways that matter here: it reads
// the selected profile from its configuration, and it WRITES the credential
// cache that profile's authentication churns — the SSO token cache for an SSO
// profile, the STS cache for an assume-role or credential_process one. It then
// tries two writes no credential refresh ever needs: appending to the
// configuration and dropping a file in the state root itself.
const awsCredentialStateStub = `#!/bin/sh
profile=default
while [ $# -gt 0 ]; do
  case "$1" in
    --profile) profile="$2"; shift 2 ;;
    --profile=*) profile="${1#--profile=}"; shift ;;
    *) shift ;;
  esac
done
config="${AWS_CONFIG_FILE:-$HOME/.aws/config}"
section="profile $profile"
[ "$profile" = default ] && section=default
if ! grep -q "^\[$section\]" "$config"; then echo "profile $profile not found in $config"; exit 3; fi
kind=sso
if sed -n "/^\[$section\]/,/^\[/p" "$config" | grep -q -e '^role_arn' -e '^credential_process'; then kind=cli; fi
cache="$HOME/.aws/sso/cache"
[ "$kind" = cli ] && cache="$HOME/.aws/cli/cache"
mkdir -p "$cache" 2>/dev/null
if printf '{"token":"stub-%s"}\n' "$profile" > "$cache/stub-$profile.json" 2>/dev/null; then
  echo "cache write ok: $kind"
else
  echo "cache write denied: $kind"
  exit 4
fi
if printf '# tampered\n' >> "$config" 2>/dev/null; then echo "config write ALLOWED"; else echo "config write denied"; fi
if printf 'x' > "$HOME/.aws/stray" 2>/dev/null; then echo "state root write attempted"; else echo "state root write denied"; fi
echo '{"Account":"123456789012","Arn":"arn:aws:sts::123456789012:assumed-role/stub"}'
`

// writeStub installs an executable script named name in base/bin.
func writeStub(t *testing.T, base, name, script string) string {
	t.Helper()
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// awsStateHome builds a host home whose ~/.aws/config holds the given profiles.
// No sso/ or cli/ directory exists yet: the live host shape that first broke
// the sandbox.
func awsStateHome(t *testing.T, base, config string) string {
	t.Helper()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(filepath.Join(home, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aws", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func requireIsolationBackend(t *testing.T) {
	t.Helper()
	if !ExecSandboxAvailable() {
		t.Skip("no isolation backend available on this host")
	}
}

// writesHostStateInPlace reports whether this platform's isolation backend
// realizes mount-backed tool state as in-place writes on the real host path.
// Bubblewrap mounts a private overlay instead, so the host is never modified.
func writesHostStateInPlace() bool {
	return SandboxBackendName(SandboxIsolated) == seatbeltBackendName
}

const (
	ssoProfileConfig = "[default]\nregion=us-east-1\n" +
		"[profile sso-dev]\nsso_session=dev\nregion=us-east-1\n" +
		"[sso-session dev]\nsso_start_url=https://example.awsapps.com/start\nsso_region=us-east-1\n"
	roleProfileConfig = "[default]\nregion=eu-west-1\n" +
		"[profile ops-admin]\nrole_arn=arn:aws:iam::123456789012:role/ops\nsource_profile=default\n"
)

// D3: an AWS command whose selected authentication must WRITE its cache runs
// inside the platform sandbox. On macOS seatbelt cannot mount, so the cache is
// written in place — and only the declared cache subpath is writable: the
// configuration stays read-only and the state root itself takes no write. On
// Linux the same write lands in the run's private overlay and the host is
// untouched. Both used to be impossible on macOS: the plan needed a mount and
// seatbelt refused it.
func TestCredentialCacheProfileRunsSandboxedWritingOnlyItsDeclaredSubpath(t *testing.T) {
	for _, tc := range []struct {
		name, config, profile, cacheRel string
	}{
		{"sso-session", ssoProfileConfig, "sso-dev", filepath.Join(".aws", "sso", "cache")},
		// The invariant with another profile name, another authentication
		// kind, and the other declared cache.
		{"assume-role", roleProfileConfig, "ops-admin", filepath.Join(".aws", "cli", "cache")},
		{"credential-process", "[default]\nregion=eu-central-1\n[profile build-bot]\ncredential_process=/usr/bin/false\n",
			"build-bot", filepath.Join(".aws", "cli", "cache")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireIsolationBackend(t)
			withExecSandboxPolicy(t, true, false, false)
			base := fixtureBase(t)
			home := awsStateHome(t, base, tc.config)
			configPath := filepath.Join(home, ".aws", "config")
			before := fileDigest(t, configPath)
			tenant, workspace := profileExecScope(t, home, writeStub(t, base, "aws", awsCredentialStateStub), executionenv.TrustTrusted)

			args := map[string]interface{}{
				"_tenant_id": tenant,
				"command":    "aws --profile " + tc.profile + " sts get-caller-identity",
				"cwd":        workspace,
				"sandbox":    string(SandboxIsolated),
				"timeout":    30,
			}
			out, err := NewExecuteCommandTool().Execute(args)
			if err != nil {
				t.Fatalf("the credential CLI must run inside the sandbox: %v\n%s", err, out)
			}
			if mode := stringArg(args, "_sandbox_mode"); mode != string(SandboxIsolated) {
				t.Fatalf("ran with sandbox mode %q, want isolated", mode)
			}
			if !strings.Contains(out, "cache write ok") || !strings.Contains(out, "123456789012") {
				t.Fatalf("the stub did not complete its credential refresh: %s", out)
			}
			if !strings.Contains(out, "config write denied") {
				t.Fatalf("the configuration must stay read-only inside the sandbox: %s", out)
			}
			if after := fileDigest(t, configPath); after != before {
				t.Fatal("the host configuration changed")
			}
			if _, err := os.Stat(filepath.Join(home, ".aws", "stray")); err == nil {
				t.Fatal("a write to the state root itself reached the host")
			}
			hostCache := filepath.Join(home, tc.cacheRel, "stub-"+tc.profile+".json")
			_, statErr := os.Stat(hostCache)
			if writesHostStateInPlace() {
				if statErr != nil {
					t.Fatalf("seatbelt must write the declared cache in place: %v", statErr)
				}
			} else if statErr == nil {
				t.Fatal("a mounting backend must keep the token in the run overlay, never on the host")
			}
		})
	}
}

// An opaque script that calls a credential CLI is prepared with the host
// inventory, so it receives the same credential state as the CLI it calls and
// runs in the same sandbox. Withholding the credentials (an untrusted workspace
// without credential:read) is the input that must change the outcome: then no
// host cache becomes writable.
func TestOpaqueScriptsReceiveTheCredentialStateOfTheCLIsTheyCall(t *testing.T) {
	const deploy = "#!/bin/sh\nset -e\naws --profile sso-dev sts get-caller-identity\n"
	const makeStub = "#!/bin/sh\n[ \"$1\" = release ] || { echo \"unknown target $1\"; exit 2; }\nexec sh ./deploy.sh\n"
	for _, command := range []string{"./deploy.sh", "make release"} {
		t.Run(strings.Fields(command)[0], func(t *testing.T) {
			requireIsolationBackend(t)
			withExecSandboxPolicy(t, true, false, false)
			base := fixtureBase(t)
			home := awsStateHome(t, base, ssoProfileConfig)
			writeStub(t, base, "aws", awsCredentialStateStub)
			bin := writeStub(t, base, "make", makeStub)
			tenant, workspace := profileExecScope(t, home, bin, executionenv.TrustTrusted)
			if err := os.WriteFile(filepath.Join(workspace, "deploy.sh"), []byte(deploy), 0o755); err != nil {
				t.Fatal(err)
			}
			args := map[string]interface{}{
				"_tenant_id": tenant, "_tool_name": "terminal",
				"command": command, "cwd": workspace, "sandbox": string(SandboxIsolated), "timeout": 30,
			}
			// The containment report and preparation make one decision: the
			// script carries the operator's selected credentials.
			assessed := copyArgs(args)
			assessed["_effective_sandbox_mode"] = string(SandboxIsolated)
			if got := assessExecContainment("terminal", assessed); got.Credentials != containmentCredentialsSelected {
				t.Fatalf("%q must report credentials=selected, got %s", command, got.Summary())
			}
			out, err := NewExecuteCommandTool().Execute(args)
			if err != nil {
				t.Fatalf("%q must run sandboxed with the CLI's credential state: %v\n%s", command, err, out)
			}
			if mode := stringArg(args, "_sandbox_mode"); mode != string(SandboxIsolated) {
				t.Fatalf("%q ran with sandbox mode %q, want isolated", command, mode)
			}
			if !strings.Contains(out, "cache write ok") || !strings.Contains(out, "config write denied") {
				t.Fatalf("%q did not get the CLI's writable cache with read-only config: %s", command, out)
			}
			_, statErr := os.Stat(filepath.Join(home, ".aws", "sso", "cache", "stub-sso-dev.json"))
			if writesHostStateInPlace() != (statErr == nil) {
				t.Fatalf("%q cache placement: in place=%v, host file present=%v", command, writesHostStateInPlace(), statErr == nil)
			}
		})
	}

	t.Run("withheld", func(t *testing.T) {
		requireIsolationBackend(t)
		withExecSandboxPolicy(t, true, false, false)
		base := fixtureBase(t)
		home := awsStateHome(t, base, ssoProfileConfig)
		bin := writeStub(t, base, "aws", awsCredentialStateStub)
		tenant, workspace := profileExecScope(t, home, bin, executionenv.TrustUntrusted)
		if err := os.WriteFile(filepath.Join(workspace, "deploy.sh"), []byte(deploy), 0o755); err != nil {
			t.Fatal(err)
		}
		args := map[string]interface{}{
			"_tenant_id": tenant, "_tool_name": "terminal",
			"command": "./deploy.sh", "cwd": workspace, "sandbox": string(SandboxIsolated), "timeout": 30,
		}
		out, err := NewExecuteCommandTool().Execute(args)
		if mode := stringArg(args, "_sandbox_mode"); mode != string(SandboxIsolated) {
			t.Fatalf("withheld credentials must still run sandboxed, ran %q: %v\n%s", mode, err, out)
		}
		if _, statErr := os.Stat(filepath.Join(home, ".aws", "sso", "cache")); statErr == nil {
			t.Fatal("withheld credentials must not make or write the host cache")
		}
		if writesHostStateInPlace() && (err == nil || !strings.Contains(out, "cache write denied")) {
			t.Fatalf("on an in-place backend the withheld cache must stay read-only: %v\n%s", err, out)
		}
	})
}

func copyArgs(args map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(args))
	for key, value := range args {
		out[key] = value
	}
	return out
}

// boundaryRun is one smart-mode Run with a real lease, a stub-bearing PATH and
// the production middleware order around the real terminal tool.
type boundaryRun struct {
	runtime   *ExecutionRuntime
	tenant    string
	workspace string
	asks      []ToolApprovalRequest
	judge     *fakeJudge
	approve   bool
	// decide, when set, answers each ask instead of a plain approve-once.
	decide func(ToolApprovalRequest) ToolApprovalDecision
}

func newBoundaryRun(t *testing.T, name, home, extraPath, trust string, mode ApprovalMode, adjust func(*ExecutionScope)) *boundaryRun {
	t.Helper()
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory for a runtime root outside /tmp")
	}
	root := filepath.Join(realHome, ".selfmind-test-runtime", "boundary-"+name)
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
	}, "inherited", "principal-boundary", nil)
	lease := "lease-boundary-" + name
	registry.BindLease(lease, snapshot.ID)

	run := &boundaryRun{
		tenant:    "tenant-boundary-" + name,
		workspace: filepath.Join(filepath.Dir(home), "workspace"),
		judge:     &fakeJudge{reply: `{"outcome":"escalate","risk_level":"low","user_authorization":"unknown","rationale":"Needs a person."}`},
		approve:   true,
	}
	if err := os.MkdirAll(run.workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	scope := ExecutionScope{
		Runtime:  NewExecutionRuntime("", executionenv.RuntimeRoot(), sandbox.PrivateState{}, nil),
		TenantID: run.tenant, PersonID: "person-boundary", TaskID: "task-boundary", RunID: "run-boundary-" + name,
		WorkspaceID: "ws-boundary", WorkspaceRoot: run.workspace, AllowedRoots: []string{run.workspace},
		LeaseID: lease, TrustLevel: trust, ApprovalMode: mode, Judge: run.judge,
		Grants: newFakeGrantStore(), StandingGrants: InteractiveStandingGrants(),
		IntentSnapshot: func() RunIntentSnapshot {
			return RunIntentSnapshot{ModelAuthorization: true, RawUserText: "Run the project tooling."}
		},
		Approval: func(_ context.Context, request ToolApprovalRequest) (ToolApprovalDecision, error) {
			run.asks = append(run.asks, request)
			if run.decide != nil {
				return run.decide(request), nil
			}
			return ToolApprovalDecision{Approved: run.approve, ApprovalID: "apr-boundary"}, nil
		},
	}
	if adjust != nil {
		adjust(&scope)
	}
	run.runtime = scope.Runtime
	t.Cleanup(SetExecutionScope(run.tenant, scope))
	return run
}

// call runs the production order — capability resolution, approval — around
// the real terminal tool, with an optional hook between approval and dispatch.
func (r *boundaryRun) call(command string, beforeDispatch func()) (map[string]interface{}, string, error) {
	args := map[string]interface{}{
		"_tenant_id": r.tenant, "_tool_name": "terminal", "command": command, "cwd": r.workspace, "timeout": 30,
	}
	body := func(args map[string]interface{}) (string, error) {
		if beforeDispatch != nil {
			beforeDispatch()
		}
		return NewExecuteCommandTool().Execute(args)
	}
	out, err := ExecutionCapabilityMiddleware()(SmartApprovalMiddleware("")(body))(args)
	return args, out, err
}

// The security-review reproduction (WP-F2, HIGH): macOS, the default
// exec_sandbox (enabled, not required), smart mode, an untrusted workspace and
// AWS SSO configuration on the host. An opaque script was released as contained
// with no judge or human ask, seatbelt then refused the mount-backed AWS state,
// and the engine fell back to running it on the host. Whatever the platform,
// the call must now run inside the sandbox that containment promised, and the
// host credential cache must stay out of reach of a workspace that holds no
// credential:read.
func TestContainedOpaqueScriptIsNeverSilentlyRunOnTheHost(t *testing.T) {
	const interpreterStub = "#!/bin/sh\n" +
		"if mkdir -p \"$HOME/.aws/sso/cache\" 2>/dev/null && printf x > \"$HOME/.aws/sso/cache/opaque.json\" 2>/dev/null; then echo \"host cache write attempted\"; else echo \"host cache write denied\"; fi\n" +
		"echo \"tool ran\"\n"
	for _, tc := range []struct {
		name, config, command, interpreter string
	}{
		{"python-sso", ssoProfileConfig, "python3 tool.py", "python3"},
		// The same invariant through another opaque payload and an
		// assume-role configuration.
		{"script-role", roleProfileConfig, "./tool.sh", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireIsolationBackend(t)
			withExecSandboxPolicy(t, true, false, false)
			base := fixtureBase(t)
			home := awsStateHome(t, base, tc.config)
			bin := writeStub(t, base, "aws", awsCredentialStateStub)
			if tc.interpreter != "" {
				writeStub(t, base, tc.interpreter, interpreterStub)
			}
			run := newBoundaryRun(t, tc.name, home, bin, executionenv.TrustUntrusted, ApprovalSmart, nil)
			if err := os.WriteFile(filepath.Join(run.workspace, "tool.py"), []byte("print('tool')\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(run.workspace, "tool.sh"), []byte(interpreterStub), 0o755); err != nil {
				t.Fatal(err)
			}

			args, out, err := run.call(tc.command, nil)
			if err != nil {
				t.Fatalf("%q: %v\n%s", tc.command, err, out)
			}
			if mode := stringArg(args, "_sandbox_mode"); mode != string(SandboxIsolated) {
				t.Fatalf("%q was released as contained but ran with sandbox mode %q", tc.command, mode)
			}
			if len(run.asks) != 0 || run.judge.calls != 0 {
				t.Fatalf("a contained call must not ask: asks=%d judge=%d", len(run.asks), run.judge.calls)
			}
			if !strings.Contains(out, "tool ran") {
				t.Fatalf("%q did not run: %s", tc.command, out)
			}
			if _, statErr := os.Stat(filepath.Join(home, ".aws", "sso", "cache", "opaque.json")); statErr == nil {
				t.Fatal("an untrusted script wrote the host credential cache")
			}
		})
	}
}
