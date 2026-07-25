package tools

import (
	"context"
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
	if strings.Contains(strings.Join(cmd.Args, " "), "bwrap") {
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
		if decision.Mode != SandboxIsolated || !strings.Contains(strings.Join(cmd.Args, " "), "bwrap") {
			t.Fatalf("available sandbox decision=%+v argv=%v", decision, cmd.Args)
		}
	} else if decision.Mode != SandboxHost || strings.TrimSpace(decision.Reason) == "" {
		t.Fatalf("unavailable sandbox must expose host fallback: %+v", decision)
	}
}

func TestExecSandboxAutoFallsBackOnDarwin(t *testing.T) {
	SetExecSandbox(true, false, false)
	resetExecSandbox(t)

	cmd, decision, err := sandboxedCommandForPlatform(
		context.Background(),
		[]string{"/bin/bash", "-c", "echo hi"},
		"/tmp",
		SandboxAuto,
		"darwin",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if cmd == nil || decision.Mode != SandboxHost {
		t.Fatalf("darwin auto fallback decision=%+v cmd=%v", decision, cmd)
	}
	if !strings.Contains(decision.Reason, "approval-controlled host execution") {
		t.Fatalf("fallback reason = %q", decision.Reason)
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
