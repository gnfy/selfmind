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
