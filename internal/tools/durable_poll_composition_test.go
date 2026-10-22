package tools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools/sandbox"
)

// A durable poll is described by its binding alone: the sandbox policy the
// registering Run froze and the in-place credential writes its approval
// covered. These tests run polls from a binding under a configuration that has
// since changed, because the two guarantees only mean something together — a
// poll the configuration can move onto the host never reaches the check that
// holds it to its approved writes.

// durablePollHost is the host a registered watch polls against: AWS SSO
// configuration, an aws stub that refreshes the caches that configuration
// needs, and the daemon environment snapshot pointing at them.
type durablePollHost struct {
	runtime   *ExecutionRuntime
	workspace string
	sso, cli  string
}

func newDurablePollHost(t *testing.T, name string) *durablePollHost {
	t.Helper()
	base := fixtureBase(t)
	home := awsStateHome(t, base, ssoProfileConfig)
	bin := writeStub(t, base, "aws", awsCredentialStateStub)
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory for a runtime root outside the temp directory")
	}
	runtimeRoot := filepath.Join(realHome, ".selfmind-test-runtime", "durable-composition-"+name)
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
	registry.Install([]string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home}, "inherited", "principal-durable", nil)
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	root := canonicalPath(t, home)
	return &durablePollHost{
		runtime:   NewExecutionRuntime("", runtimeRoot, sandbox.PrivateState{}, nil),
		workspace: workspace,
		sso:       filepath.Join(root, ".aws", "sso", "cache"),
		cli:       filepath.Join(root, ".aws", "cli", "cache"),
	}
}

// binding is what a registration recorded: the policy its Run ran under (nil:
// the row predates it) and the writes its approval covered.
func (h *durablePollHost) binding(frozen *executionenv.SandboxPolicy, writes ...string) executionenv.Binding {
	return executionenv.Binding{
		Version: executionenv.BindingVersion, ID: "binding:watch-composition",
		TenantID: "tenant-durable", PersonID: "person-durable", WorkspaceID: "ws-durable",
		TrustLevel: executionenv.TrustTrusted, SandboxPolicy: frozen, HostStateWrites: writes,
	}
}

// poll runs the registered check once, from its binding, under the
// configuration in force now.
func (h *durablePollHost) poll(t *testing.T, name string, binding executionenv.Binding) (ExecutionResult, error) {
	t.Helper()
	return RunDurableCheck(WithExecutionRuntime(t.Context(), h.runtime), "aws sts get-caller-identity --profile sso-dev", h.workspace, true, DurableExecutionScope{
		ScratchKey: "watch-" + name, TenantID: "tenant-durable", PersonID: "person-durable",
		WorkspaceID: "ws-durable", TrustLevel: executionenv.TrustTrusted, Binding: binding,
	})
}

// requireNoCache fails when a poll created credential state on the host.
func (h *durablePollHost) requireNoCache(t *testing.T, why string) {
	t.Helper()
	for _, path := range []string{h.sso, h.cli} {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("%s was created: %s", path, why)
		}
	}
}

func TestDurablePollRequiresItsOwnedRecoveryRuntime(t *testing.T) {
	requireInPlaceBackend(t)
	withExecSandboxPolicy(t, true, false, true)
	h := newDurablePollHost(t, t.Name())
	result, err := RunDurableCheck(t.Context(), "aws sts get-caller-identity --profile sso-dev", h.workspace, true, DurableExecutionScope{
		ScratchKey: "watch-missing-runtime", TenantID: "tenant-durable", PersonID: "person-durable",
		WorkspaceID: "ws-durable", TrustLevel: executionenv.TrustTrusted,
		Binding: h.binding(nil, h.sso, h.cli),
	})
	if err == nil || result.Started || result.FailureClass != "environment" || result.RecoveryAttempted {
		t.Fatalf("a missing recovery owner must refuse before dispatch: %+v %v", result, err)
	}
	if requireNotDispatched(t, err).ToolEffectState() != "not_dispatched" {
		t.Fatal("the refusal must retain effect certainty")
	}
	// Profile preparation may make empty state directories; the command itself
	// must not write either cache or touch its configuration.
	for _, cache := range []string{h.sso, h.cli} {
		if _, statErr := os.Stat(filepath.Join(cache, "stub-sso-dev.json")); !os.IsNotExist(statErr) {
			t.Fatalf("the refused command wrote credential state: %s %v", cache, statErr)
		}
	}
}

func requireWriteSetRefusal(t *testing.T, result ExecutionResult, err error) {
	t.Helper()
	if err == nil || result.Started {
		t.Fatalf("a poll that writes beyond its registration must not start: started=%v err=%v (%s)", result.Started, err, result.Output)
	}
	if refusal := requireNotDispatched(t, err); refusal.ToolErrorCode() != "approved_host_writes_changed" {
		t.Fatalf("want the write-set refusal, got %s: %v", refusal.ToolErrorCode(), err)
	}
	if result.FailureClass != "environment" || result.RecoveryAttempted {
		t.Fatalf("the refusal must park as an environment failure without a retry: %+v", result)
	}
}

// The frozen policy and the registered write set compose. After registration
// the operator disables the sandbox: a poll that followed the configuration
// would run on the host, where no in-place plan exists to hold it to its
// approved writes. It keeps the isolation its Run froze, so it is still held to
// them — a larger plan is refused before anything is created, the registered
// one runs.
func TestDurablePollKeepsItsFrozenBoundaryAndRegisteredWritesWhenConfigurationLoosens(t *testing.T) {
	requireInPlaceBackend(t)
	frozen := &executionenv.SandboxPolicy{Enabled: true}
	for _, tc := range []struct {
		name   string
		writes []string
	}{
		{"registered-without-writes", nil},
		// The same invariant when the plan grows by one path.
		{"registered-with-fewer-writes", []string{"sso"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newDurablePollHost(t, tc.name)
			withExecSandboxPolicy(t, false, false, true)
			writes := make([]string, 0, len(tc.writes))
			for _, which := range tc.writes {
				writes = append(writes, map[string]string{"sso": host.sso, "cli": host.cli}[which])
			}
			result, err := host.poll(t, tc.name, host.binding(frozen, writes...))
			requireWriteSetRefusal(t, result, err)
			if result.Plan.Mode == SandboxHost {
				t.Fatalf("a poll registered under isolation ran on the host after the configuration loosened: %+v", result.Plan)
			}
			host.requireNoCache(t, "a poll the registration never covered ran on the host")
		})
	}

	// The constraint that must change the result: the registration covered
	// exactly these writes, so the poll runs, still isolated, writing only them.
	host := newDurablePollHost(t, "registered-with-these-writes")
	withExecSandboxPolicy(t, false, false, true)
	result, err := host.poll(t, "registered-with-these-writes", host.binding(frozen, host.cli, host.sso))
	if err != nil || !strings.Contains(result.Output, "cache write ok: sso") {
		t.Fatalf("a poll within its registered writes must run: %v (%s)", err, result.Output)
	}
	if result.Plan.Mode != SandboxIsolated {
		t.Fatalf("the poll must keep the isolation its registration froze, ran %q", result.Plan.Mode)
	}
	if got := slices.Sorted(slices.Values(result.Plan.HostStateWrites)); !slices.Equal(got, []string{host.cli, host.sso}) {
		t.Fatalf("the poll wrote %v in place, want exactly its registered %v", got, []string{host.cli, host.sso})
	}
}

// The configuration can only narrow a poll. A watch registered on the host
// approved no in-place writes, so the isolation a stricter configuration now
// imposes does not grant any.
func TestDurablePollNarrowedByTheConfigurationIsStillHeldToItsRegisteredWrites(t *testing.T) {
	requireInPlaceBackend(t)
	hostPolicy := &executionenv.SandboxPolicy{}
	for _, tc := range []struct {
		name              string
		enabled, required bool
	}{
		{"sandbox-enabled-after-registration", true, false},
		// The same invariant through the other way of tightening it.
		{"sandbox-required-after-registration", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newDurablePollHost(t, tc.name)
			withExecSandboxPolicy(t, tc.enabled, tc.required, true)
			result, err := host.poll(t, tc.name, host.binding(hostPolicy))
			requireWriteSetRefusal(t, result, err)
			host.requireNoCache(t, "a poll narrowed into isolation wrote state its registration never covered")
		})
	}

	// The constraint that must change the result: the configuration the watch
	// was registered under, so nothing narrows it and it runs on the host.
	host := newDurablePollHost(t, "unchanged-host-policy")
	withExecSandboxPolicy(t, false, false, true)
	result, err := host.poll(t, "unchanged-host-policy", host.binding(hostPolicy))
	if err != nil || result.Plan.Mode != SandboxHost || !strings.Contains(result.Output, "cache write ok: sso") {
		t.Fatalf("a host watch under its own configuration must poll on the host: %v mode=%q (%s)", err, result.Plan.Mode, result.Output)
	}
}

// Rows that predate the fields behave as they always did, no looser: a row that
// recorded no policy follows the configured one, and a row that recorded no
// writes covers none.
func TestLegacyBindingPollFollowsTheConfiguredPolicyAndCoversNoWrites(t *testing.T) {
	requireInPlaceBackend(t)
	for _, tc := range []struct {
		name    string
		binding func(*durablePollHost) executionenv.Binding
	}{
		{"binding-before-either-field", func(h *durablePollHost) executionenv.Binding { return h.binding(nil) }},
		// A row that predates bindings altogether.
		{"row-before-bindings", func(*durablePollHost) executionenv.Binding { return executionenv.Binding{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newDurablePollHost(t, tc.name)
			// Configured to isolate: the in-place plan is held to the writes the
			// row never recorded.
			withExecSandboxPolicy(t, true, false, true)
			result, err := host.poll(t, tc.name+"-isolated", tc.binding(host))
			requireWriteSetRefusal(t, result, err)
			host.requireNoCache(t, "a legacy poll wrote state no approval ever covered")

			// Configured not to: with no frozen policy the configuration is the
			// policy, as it was before policies were recorded.
			withExecSandboxPolicy(t, false, false, true)
			result, err = host.poll(t, tc.name+"-host", tc.binding(host))
			if err != nil || result.Plan.Mode != SandboxHost {
				t.Fatalf("a row with no frozen policy follows the configured one: %v mode=%q (%s)", err, result.Plan.Mode, result.Output)
			}
		})
	}
}

// The policy composition holds on every platform, not only where state is
// written in place: from its binding alone, a poll is isolated whenever either
// the policy its Run froze or the configuration now says so.
func TestDurableCheckRunsUnderTheBindingsFrozenPolicyNarrowedByConfiguration(t *testing.T) {
	requireIsolationBackend(t)
	registry := executionenv.NewRegistry()
	previous := executionenv.DefaultRegistry()
	executionenv.SetDefaultRegistry(registry)
	t.Cleanup(func() { executionenv.SetDefaultRegistry(previous) })
	registry.Install([]string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}, "inherited", "principal-durable", nil)

	poll := func(frozen *executionenv.SandboxPolicy) ExecutionResult {
		t.Helper()
		result, err := RunDurableCheck(WithExecutionRuntime(t.Context(), NewExecutionRuntime("durable-policy", t.TempDir(), sandbox.PrivateState{}, nil)), "printf PENDING", canonicalPath(t, t.TempDir()), true, DurableExecutionScope{
			TenantID: "tenant-durable", PersonID: "person-durable", WorkspaceID: "ws-durable",
			Binding: executionenv.Binding{Version: executionenv.BindingVersion, ID: "binding:policy",
				TenantID: "tenant-durable", PersonID: "person-durable", WorkspaceID: "ws-durable",
				TrustLevel: executionenv.TrustUntrusted, SandboxPolicy: frozen},
		})
		if err != nil || result.Output != "PENDING" {
			t.Fatalf("poll: %v (%q)", err, result.Output)
		}
		return result
	}
	for _, tc := range []struct {
		name              string
		frozen            *executionenv.SandboxPolicy
		enabled, required bool
		want              SandboxMode
	}{
		{"frozen-isolated-configuration-loosened", &executionenv.SandboxPolicy{Enabled: true}, false, false, SandboxIsolated},
		{"frozen-required-configuration-loosened", &executionenv.SandboxPolicy{Enabled: true, Required: true}, false, false, SandboxIsolated},
		{"frozen-host-configuration-tightened", &executionenv.SandboxPolicy{}, true, false, SandboxIsolated},
		{"frozen-host-configuration-unchanged", &executionenv.SandboxPolicy{}, false, false, SandboxHost},
		{"no-frozen-policy-follows-configuration-host", nil, false, false, SandboxHost},
		{"no-frozen-policy-follows-configuration-isolated", nil, true, false, SandboxIsolated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withExecSandboxPolicy(t, tc.enabled, tc.required, true)
			if got := poll(tc.frozen).Plan.Mode; got != tc.want {
				t.Fatalf("poll ran %q, want %q", got, tc.want)
			}
		})
	}
}
