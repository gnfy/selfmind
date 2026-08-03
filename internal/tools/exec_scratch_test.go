package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/executionenv"
)

// scratchExecScope installs a run scope with a lease so exec calls resolve run
// scratch, and points the runtime root somewhere that is NOT under /tmp (the
// /tmp bind would otherwise shadow the real scratch path).
func scratchExecScope(t *testing.T) (tenant string, workspace string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory for a runtime root outside /tmp")
	}
	root := filepath.Join(home, ".selfmind-test-runtime", "exec-"+t.Name())
	if err := executionenv.SetRuntimeRoot(root); err != nil {
		t.Skipf("runtime root unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	workspace = t.TempDir()
	tenant = "tenant-scratch-" + t.Name()
	cleanup := SetExecutionScope(tenant, ExecutionScope{
		TenantID:      tenant,
		PersonID:      "person-scratch",
		WorkspaceID:   "ws-scratch",
		WorkspaceRoot: workspace,
		AllowedRoots:  []string{workspace},
		LeaseID:       "lease-" + strings.ReplaceAll(t.Name(), "/", "-"),
		ApprovalMode:  ApprovalFullAuto,
	})
	t.Cleanup(cleanup)
	return tenant, workspace
}

// The live discontinuity: an isolated command wrote /tmp/<file> and the next
// command of the same run could not read it, because each invocation received a
// fresh private tmpfs. $SELFMIND_RUN_TMP must survive from one command to the
// next in whichever mode the host provides.
func TestRunScratchSurvivesAcrossCommands(t *testing.T) {
	tenant, workspace := scratchExecScope(t)

	write := map[string]interface{}{
		"_tenant_id": tenant,
		"command":    `printf 'kubeconfig-from-command-A' > "$SELFMIND_RUN_TMP/handoff.txt"`,
		"cwd":        workspace,
		"timeout":    20,
	}
	if out, err := NewExecuteCommandTool().Execute(write); err != nil {
		t.Fatalf("first command failed: %v (%s)", err, out)
	}

	read := map[string]interface{}{
		"_tenant_id": tenant,
		"command":    `cat "$SELFMIND_RUN_TMP/handoff.txt"`,
		"cwd":        workspace,
		"timeout":    20,
	}
	out, err := NewExecuteCommandTool().Execute(read)
	if err != nil {
		t.Fatalf("second command of the same run could not read the handoff: %v (%s)", err, out)
	}
	if !strings.Contains(out, "kubeconfig-from-command-A") {
		t.Fatalf("handoff content lost: %q", out)
	}
}

// TMPDIR follows the run scratch, so a tool that only honours TMPDIR also gets
// run-scoped temp space.
func TestRunScratchRedirectsTMPDIR(t *testing.T) {
	tenant, workspace := scratchExecScope(t)
	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant,
		"command":    `test "$TMPDIR" = "$SELFMIND_RUN_TMP" && echo tmpdir-follows-scratch`,
		"cwd":        workspace,
		"timeout":    20,
	})
	if err != nil {
		t.Fatalf("TMPDIR must follow the run scratch: %v (%s)", err, out)
	}
	if !strings.Contains(out, "tmpdir-follows-scratch") {
		t.Fatalf("unexpected output: %q", out)
	}
}

// A different run must not see this run's scratch.
func TestRunScratchIsolatedBetweenRuns(t *testing.T) {
	tenant, workspace := scratchExecScope(t)
	if _, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant,
		"command":    `printf secret > "$SELFMIND_RUN_TMP/private.txt"`,
		"cwd":        workspace,
		"timeout":    20,
	}); err != nil {
		t.Fatal(err)
	}

	otherTenant := tenant + "-other"
	cleanup := SetExecutionScope(otherTenant, ExecutionScope{
		TenantID:      otherTenant,
		PersonID:      "person-scratch",
		WorkspaceRoot: workspace,
		AllowedRoots:  []string{workspace},
		LeaseID:       "lease-other-run",
		ApprovalMode:  ApprovalFullAuto,
	})
	t.Cleanup(cleanup)

	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": otherTenant,
		"command":    `test -f "$SELFMIND_RUN_TMP/private.txt" && echo LEAKED || echo isolated`,
		"cwd":        workspace,
		"timeout":    20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "LEAKED") {
		t.Fatalf("run scratch leaked across runs: %q", out)
	}
}

// The writable view must come from the workspace's allowed roots, not from the
// command's cwd: a command running in a subdirectory used to be unable to write
// to its own workspace root.
func TestWritableViewComesFromScopeNotCWD(t *testing.T) {
	tenant, workspace := scratchExecScope(t)
	sub := filepath.Join(workspace, "nested", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant,
		"command":    `printf ok > ` + filepath.Join(workspace, "root-level.txt") + ` && echo wrote-workspace-root`,
		"cwd":        sub,
		"timeout":    20,
	})
	if err != nil {
		t.Fatalf("a command in a subdirectory must still write to its workspace root: %v (%s)", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "root-level.txt")); statErr != nil {
		t.Fatalf("workspace-root write did not land: %v", statErr)
	}
}

// The isolated path is the one that regressed: a private tmpfs per invocation.
// This exercises the real bubblewrap double bind — scratch at its own absolute
// path AND at /tmp — so $SELFMIND_RUN_TMP is literally the same directory in
// both modes and survives between commands.
func TestRunScratchSurvivesAcrossCommandsIsolated(t *testing.T) {
	if !ExecSandboxAvailable() {
		t.Skip("bubblewrap or unprivileged user namespaces unavailable")
	}
	enabled, required, network := execSandboxPolicy()
	SetExecSandbox(true, false, true)
	t.Cleanup(func() { SetExecSandbox(enabled, required, network) })

	tenant, workspace := scratchExecScope(t)
	if out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant,
		"command":    `printf isolated-handoff > "$SELFMIND_RUN_TMP/iso.txt"; printf also-via-tmp > /tmp/iso-tmp.txt`,
		"cwd":        workspace,
		"sandbox":    string(SandboxIsolated),
		"timeout":    30,
	}); err != nil {
		t.Fatalf("isolated write failed: %v (%s)", err, out)
	}

	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant,
		"command":    `cat "$SELFMIND_RUN_TMP/iso.txt"; cat "$SELFMIND_RUN_TMP/iso-tmp.txt"; cat /tmp/iso.txt`,
		"cwd":        workspace,
		"sandbox":    string(SandboxIsolated),
		"timeout":    30,
	})
	if err != nil {
		t.Fatalf("isolated second command lost the handoff: %v (%s)", err, out)
	}
	// Written via $SELFMIND_RUN_TMP, read back through both paths, and the file
	// written via literal /tmp is visible through $SELFMIND_RUN_TMP: one
	// directory, two mount points.
	for _, want := range []string{"isolated-handoff", "also-via-tmp"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in isolated handoff output: %q", want, out)
		}
	}
}

// A background command belongs to its run. Skipping the execution engine made
// the same command behave differently depending only on whether it was
// backgrounded: the foreground gcloud got its state overlay and the background
// one did not.
func TestBackgroundCommandUsesTheRunEnvironment(t *testing.T) {
	tenant, workspace := scratchExecScope(t)
	marker := filepath.Join(workspace, "background-env.txt")
	out, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant,
		"_tool_name": "terminal",
		// The run's scratch variable must be visible to a backgrounded command.
		"command":    `printf '%s' "$SELFMIND_RUN_TMP" > ` + marker,
		"cwd":        workspace,
		"background": true,
		"sandbox":    string(SandboxHost),
	})
	if err != nil {
		t.Fatalf("background start failed: %v (%s)", err, out)
	}
	if !strings.Contains(out, "Started background process") {
		t.Fatalf("unexpected output: %q", out)
	}
	// The process is detached; poll briefly for its effect.
	var recorded []byte
	for i := 0; i < 100; i++ {
		if data, readErr := os.ReadFile(marker); readErr == nil && len(data) > 0 {
			recorded = data
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(recorded) == 0 {
		t.Fatal("background command produced no output; it never ran")
	}
	scratch, ok := scratchForArgs(map[string]interface{}{"_tenant_id": tenant})
	if !ok {
		t.Fatal("run scratch should be resolvable")
	}
	if string(recorded) != scratch.TmpDir {
		t.Fatalf("background command saw %q, want the run's scratch %q", recorded, scratch.TmpDir)
	}
}

// A background command still requires host mode plus approval; that contract
// must survive routing it through the engine.
func TestBackgroundCommandStillRequiresHostMode(t *testing.T) {
	tenant, workspace := scratchExecScope(t)
	_, err := NewExecuteCommandTool().Execute(map[string]interface{}{
		"_tenant_id": tenant,
		"_tool_name": "terminal",
		"command":    "sleep 1",
		"cwd":        workspace,
		"background": true,
		"sandbox":    string(SandboxIsolated),
	})
	if err == nil || !strings.Contains(err.Error(), "require sandbox=host") {
		t.Fatalf("background must still require host mode, got %v", err)
	}
}

// A background process outlives its run, so nothing else will ever stop it. The
// ceiling exists to stop a leak — not to bound legitimate work, which belongs in
// watch_external.
func TestBackgroundProcessIsBounded(t *testing.T) {
	if got := backgroundProcessCeiling(map[string]interface{}{}); got != 2*time.Hour {
		t.Fatalf("default ceiling = %v, want 2h", got)
	}
	explicit := backgroundProcessCeiling(map[string]interface{}{"timeout": 45})
	if explicit != 45*time.Second {
		t.Fatalf("an explicit timeout must win, got %v", explicit)
	}
	long := backgroundProcessCeiling(map[string]interface{}{
		"execution_class": string(ToolExecutionLongRunning),
	})
	if long != 2*time.Hour {
		t.Fatalf("long-running class ceiling = %v, want its 2h maximum", long)
	}
}

// The ceiling must actually stop a wedged process.
func TestBackgroundProcessCeilingKillsAWedgedCommand(t *testing.T) {
	registry := &ProcessRegistry{processes: map[string]*ProcessInfo{}}
	id, err := registry.StartProcess("sleep 30", t.TempDir(), nil, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		registry.mu.Lock()
		info := registry.processes[id]
		registry.mu.Unlock()
		if info == nil || info.Cmd == nil {
			t.Fatal("process record disappeared")
		}
		if info.Cmd.ProcessState != nil {
			return // reaped
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("a wedged background process must be stopped by its ceiling")
}
