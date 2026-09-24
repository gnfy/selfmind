package tools

import (
	"context"
	"net"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func resetExecSandbox(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { SetExecSandbox(false, false, false) })
}

func TestExecSandboxDisabledAutoUsesObservableHostFallback(t *testing.T) {
	SetExecSandbox(false, false, false)
	resetExecSandbox(t)
	cmd, decision, err := sandboxedShellCommand(context.Background(), "echo hi", "/tmp", SandboxAuto)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != SandboxHost || !strings.Contains(decision.Reason, "disabled") {
		t.Fatalf("decision = %+v", decision)
	}
	if cmd.Args[0] != shellArgv("echo hi")[0] {
		t.Fatalf("disabled sandbox must not wrap: %v", cmd.Args)
	}
}

func TestExecSandboxDisabledIsolatedRefuses(t *testing.T) {
	SetExecSandbox(false, false, false)
	resetExecSandbox(t)
	if _, _, err := sandboxedShellCommand(context.Background(), "echo hi", "/tmp", SandboxIsolated); err == nil {
		t.Fatal("isolated mode must fail when the operator disabled sandboxing")
	}
}

func TestExecSandboxExplicitHostAndRequiredPolicy(t *testing.T) {
	SetExecSandbox(true, false, false)
	resetExecSandbox(t)
	cmd, decision, err := sandboxedShellCommand(context.Background(), "echo hi", "/tmp", SandboxHost)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Mode != SandboxHost || cmd.Args[0] != shellArgv("echo hi")[0] {
		t.Fatalf("host decision=%+v argv=%v", decision, cmd.Args)
	}

	SetExecSandbox(true, true, false)
	if _, _, err := sandboxedShellCommand(context.Background(), "echo hi", "/tmp", SandboxHost); err == nil {
		t.Fatal("required policy must reject the host escape hatch")
	}
}

func TestExecSandboxAutoReflectsHostCapability(t *testing.T) {
	SetExecSandbox(true, false, false)
	resetExecSandbox(t)
	cmd, decision, err := sandboxedShellCommand(context.Background(), "echo hi", "/tmp", SandboxAuto)
	if err != nil {
		t.Fatal(err)
	}
	if cmd == nil {
		t.Fatal("auto mode must produce a runnable command")
	}
	if ExecSandboxAvailable() {
		// Wrapping is proved by the PROGRAM, not by a backend name appearing
		// somewhere in argv: seatbelt carries its whole policy text in argv, so
		// a substring check passes on any word the policy happens to mention.
		if decision.Mode != SandboxIsolated || cmd.Args[0] == shellArgv("echo hi")[0] {
			t.Fatalf("available sandbox decision=%+v argv=%v", decision, cmd.Args)
		}
	} else if decision.Mode != SandboxHost || strings.TrimSpace(decision.Reason) == "" {
		t.Fatalf("unavailable sandbox must expose host fallback: %+v", decision)
	}
}

// A host that CAN isolate but currently cannot must say which mechanism is
// missing, and a platform with no isolation at all must say that instead. The
// two were the same message while macOS had no backend; conflating them now
// would send someone installing bubblewrap on a Mac.
func TestExecSandboxAutoFallbackNamesTheMissingMechanism(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want string
	}{
		{"darwin", "sandbox-exec unavailable"},
		{"linux", "bubblewrap or unprivileged user namespaces unavailable"},
		{"windows", "isolated sandbox unsupported on windows"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			SetExecSandbox(true, false, false)
			resetExecSandbox(t)

			cmd, decision, err := sandboxedCommandForPlatform(
				context.Background(),
				[]string{"/bin/bash", "-c", "echo hi"},
				"/tmp",
				SandboxAuto,
				tc.goos,
				false,
			)
			if err != nil {
				t.Fatal(err)
			}
			if cmd == nil || decision.Mode != SandboxHost {
				t.Fatalf("auto fallback decision=%+v cmd=%v", decision, cmd)
			}
			if !strings.Contains(decision.Reason, tc.want) {
				t.Fatalf("fallback reason = %q, want it to name %q", decision.Reason, tc.want)
			}
		})
	}
}

func TestExecSandboxDarwinStrictModesFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested SandboxMode
		required  bool
	}{
		{name: "isolated", requested: SandboxIsolated},
		{name: "required", requested: SandboxAuto, required: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetExecSandbox(true, tc.required, false)
			resetExecSandbox(t)
			_, _, err := sandboxedCommandForPlatform(
				context.Background(),
				[]string{"/bin/bash", "-c", "echo hi"},
				"/tmp",
				tc.requested,
				"darwin",
				false,
			)
			if err == nil {
				t.Fatal("strict sandbox mode must fail closed on darwin")
			}
		})
	}
}

func TestExecSandboxRequiredButUnavailableRefuses(t *testing.T) {
	if ExecSandboxAvailable() {
		t.Skip("bubblewrap is available on this host")
	}
	SetExecSandbox(true, true, false)
	resetExecSandbox(t)
	if _, _, err := sandboxedShellCommand(context.Background(), "echo hi", "/tmp", SandboxAuto); err == nil {
		t.Fatal("required sandbox must fail closed when unavailable")
	}
}

// A backend that cannot enforce THIS plan must say so rather than run it with
// a policy that quietly drops part of the contract. Seatbelt has no mount
// namespace, so a plan whose tool state is redirected by a bind (the AWS SSO
// token cache) is unenforceable there: the command must degrade to an
// approval-gated host run, and a run that REQUIRED isolation must fail instead.
func TestMountBackedPlanIsRefusedRatherThanSilentlyWidened(t *testing.T) {
	if SandboxBackendName(SandboxIsolated) != seatbeltBackendName {
		t.Skip("mount-backed plans are enforceable on this platform")
	}
	withExecSandboxPolicy(t, true, false, false)
	workspace := t.TempDir()
	material := execMaterial{
		WritableRoots: []string{workspace},
		Env:           currentToolProcessEnv(),
		OverlayMounts: []SandboxOverlayMount{{
			Source: workspace, Target: filepath.Join(workspace, "state", "sso"),
		}},
	}

	_, decision, _, err := sandboxedCommandWithMaterial(context.Background(),
		[]string{"/bin/sh", "-c", "echo hi"}, material, SandboxAuto, runtime.GOOS, true)
	if err != nil {
		t.Fatalf("auto must degrade rather than fail: %v", err)
	}
	if decision.Mode != SandboxHost {
		t.Fatalf("an unenforceable plan must not claim isolation: %+v", decision)
	}
	if !strings.Contains(decision.Reason, "cannot enforce this plan") {
		t.Fatalf("the degraded reason must name the cause, got %q", decision.Reason)
	}

	// The constraint that must change the result: isolation was demanded, so
	// there is nothing to degrade to.
	if _, _, _, err := sandboxedCommandWithMaterial(context.Background(),
		[]string{"/bin/sh", "-c", "echo hi"}, material, SandboxIsolated, runtime.GOOS, true); err == nil {
		t.Fatal("an explicit isolated request must fail when the plan is unenforceable")
	}

	// Generality: the same plan WITHOUT the mount is enforceable, so the
	// refusal is about the mount and not about this material in general.
	material.OverlayMounts = nil
	_, decision, _, err = sandboxedCommandWithMaterial(context.Background(),
		[]string{"/bin/sh", "-c", "echo hi"}, material, SandboxAuto, runtime.GOOS, true)
	if err != nil || decision.Mode != SandboxIsolated {
		t.Fatalf("the same plan without a mount must isolate: %+v (%v)", decision, err)
	}
}

// The three surfaces that told the person macOS had no isolation must now agree
// with the backend, or a real guarantee stays invisible.
func TestMacOSIsolationIsReportedEverywhere(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("no isolation backend available on this host")
	}
	withExecSandboxPolicy(t, true, false, false)

	if diag := ExecSandboxDiagnostics(); diag.Backend != SandboxBackendName(SandboxIsolated) {
		t.Fatalf("diagnostics must name the enforcing backend, got %q", diag.Backend)
	}
	// Enforced is what releases an isolated call from the approval queue; while
	// it was false on macOS the whole observation catalog was unreachable.
	assessment := assessExecContainment("terminal", map[string]interface{}{
		"_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated),
	})
	if !assessment.Enforced || !assessment.AutoApprove() {
		t.Fatalf("an isolated call on an enforcing host must be contained: %+v", assessment)
	}
	// The constraint that must change the result: enforcement is a HOST
	// capability, so disabling the policy must withdraw the claim.
	withExecSandboxPolicy(t, false, false, false)
	if assessExecContainment("terminal", map[string]interface{}{
		"_tool_name": "terminal", "_effective_sandbox_mode": string(SandboxIsolated),
	}).Enforced {
		t.Fatal("a disabled sandbox must not report enforcement")
	}
}

// The proxy projection must follow the decision that actually runs, not the
// request. `auto` degrades to host execution carrying the daemon's own network,
// and projecting from the request stripped a proxy that command could reach —
// recording a plan that read network_mode "shared" beside proxy_mode
// "omitted_for_isolated_network".
//
// The listener is real rather than injected: "reachable loopback proxy" is the
// condition under test, and a stub would assert the plumbing instead of it.
func TestProxyProjectionFollowsTheResolvedDecision(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind a loopback listener")
	}
	defer listener.Close()
	proxy := "HTTPS_PROXY=http://" + listener.Addr().String()

	material := func() execMaterial {
		return execMaterial{
			WritableRoots: []string{t.TempDir()},
			Env:           []string{"PATH=/usr/bin", proxy},
		}
	}

	t.Run("auto degrading to host keeps a proxy that host execution can reach", func(t *testing.T) {
		withExecSandboxPolicy(t, false, false, false) // disabled -> host fallback
		cmd, decision, plan, err := sandboxedCommandWithMaterial(context.Background(),
			[]string{"/bin/sh", "-c", "true"}, material(), SandboxAuto, runtime.GOOS, false, false)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Mode != SandboxHost || !decision.NetworkShared {
			t.Fatalf("expected a host fallback with the daemon's network: %+v", decision)
		}
		if plan.NetworkMode != "shared" || plan.ProxyMode != proxyModeInherited {
			t.Fatalf("a shared-network plan must not report an isolated-network proxy omission: %+v", plan)
		}
		if !slices.Contains(cmd.Env, proxy) {
			t.Fatal("host execution lost a proxy it can reach")
		}
	})

	t.Run("explicit host execution is a shared network", func(t *testing.T) {
		withExecSandboxPolicy(t, true, false, false)
		cmd, decision, plan, err := sandboxedCommandWithMaterial(context.Background(),
			[]string{"/bin/sh", "-c", "true"}, material(), SandboxHost, runtime.GOOS, true, false)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Mode != SandboxHost || plan.ProxyMode != proxyModeInherited || !slices.Contains(cmd.Env, proxy) {
			t.Fatalf("explicit host execution must keep its proxy: %+v %+v", decision, plan)
		}
	})

	// The constraint that must change the result: an isolated call cannot reach
	// any proxy, so the same material must lose it — and say so consistently.
	t.Run("isolated execution omits it and the plan agrees", func(t *testing.T) {
		if !ExecSandboxAvailable() {
			t.Skip("no isolation backend on this host")
		}
		withExecSandboxPolicy(t, true, false, false)
		cmd, decision, plan, err := sandboxedCommandWithMaterial(context.Background(),
			[]string{"/bin/sh", "-c", "true"}, material(), SandboxAuto, runtime.GOOS, true, false)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Mode != SandboxIsolated {
			t.Fatalf("expected isolation: %+v", decision)
		}
		if plan.NetworkMode != "isolated" || plan.ProxyMode != proxyModeOmittedForIsolatedNetwork {
			t.Fatalf("an isolated plan must record why the proxy went: %+v", plan)
		}
		if slices.Contains(cmd.Env, proxy) {
			t.Fatal("an isolated command kept a proxy it cannot reach")
		}
	})
}
