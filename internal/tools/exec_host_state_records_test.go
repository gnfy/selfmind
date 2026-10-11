//go:build darwin

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools/envprofiles"
	"selfmind/internal/tools/sandbox"
)

// replaceStopLeftovers swaps the leftover stop for the test's own.
func replaceStopLeftovers(t *testing.T, stop func(string) ([]int, error)) {
	t.Helper()
	previous := stopLeftovers
	stopLeftovers = stop
	t.Cleanup(func() { stopLeftovers = previous })
}

// Item 1: a stop that fails leaves its canary recorded, so the next startup
// sweep tries again; the call says the stop was not confirmed. A stop that
// succeeds forgets the canary.
func TestAMarkerOutlivesAStopThatFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"failed", errors.New("injected: a process is still running")},
		{"succeeded", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardFixture(t, "marker-"+tc.name)
			var canary string
			replaceStopLeftovers(t, func(name string) ([]int, error) {
				canary = name
				return nil, tc.err
			})
			result, _ := f.execute(context.Background(), "true")
			if !sandbox.ValidCanary(canary) {
				t.Fatalf("fixture: the guard must stop its command's canary, got %q", canary)
			}
			_, statErr := os.Stat(filepath.Join(f.run.runtime.sandboxCanaryDir(), canary))
			if kept := statErr == nil; kept != (tc.err != nil) {
				t.Fatalf("canary record kept=%v after a stop that returned %v", kept, tc.err)
			}
			if said := strings.Contains(result.Output, "could not confirm"); said != (tc.err != nil) {
				t.Fatalf("the call must say exactly when the stop was not confirmed: %s", result.Output)
			}
		})
	}
}

// Item 2: the startup sweep acts only on canaries this runtime generated; any
// other name in the records is ignored and left alone.
func TestStartupSweepActsOnlyOnGeneratedCanaries(t *testing.T) {
	f := newGuardFixture(t, "markers")
	executionenv.DefaultRegistry().Install([]string{"PATH=/usr/bin:/bin", "HOME=" + f.home}, "inherited", "principal-markers", nil)
	dir := f.run.runtime.sandboxCanaryDir()
	mustDo(t, os.MkdirAll(dir, 0o700))
	valid := sandbox.NewCanary()
	names := []string{valid, "evil-marker", "com.selfmind.exec.0123abcd", "com.apple.Safari"}
	for _, name := range names {
		mustWrite(t, filepath.Join(dir, name), "")
	}
	var stopped []string
	replaceStopLeftovers(t, func(name string) ([]int, error) {
		stopped = append(stopped, name)
		return nil, nil
	})
	f.run.runtime.SweepHostStateAtStartup()
	if !slices.Equal(stopped, []string{valid}) {
		t.Fatalf("the sweep must act on the generated canary alone, acted on %v", stopped)
	}
	for _, name := range names[1:] {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("a record that names no canary of ours was touched: %s %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, valid)); !os.IsNotExist(err) {
		t.Fatalf("the stopped canary's record was kept: %v", err)
	}
}

// Item 2: the runtime's records — canary markers and the quarantine — stay
// read-only to a sandboxed command even when its writable root contains the
// runtime root; the rest of that root stays writable.
func TestRuntimeRecordsAreReadOnlyEvenInsideAWritableRoot(t *testing.T) {
	f := newGuardFixture(t, "records")
	parent := filepath.Dir(executionenv.RuntimeRoot())
	marker := filepath.Join(f.run.runtime.sandboxCanaryDir(), sandbox.NewCanary())
	quarantined := filepath.Join(f.run.runtime.hostStateQuarantineRoot(), "planted")
	other := filepath.Join(parent, "records-other-"+filepath.Base(executionenv.RuntimeRoot()))
	t.Cleanup(func() { _ = os.Remove(other) })
	command := "aws sts get-caller-identity --profile sso-dev && " +
		"{ mkdir -p '" + filepath.Dir(marker) + "' '" + filepath.Dir(quarantined) + "' 2>/dev/null; " +
		"printf x > '" + marker + "' && echo 'marker written'; printf x > '" + quarantined + "' && echo 'quarantine written'; " +
		"printf x > '" + other + "' && echo 'other written'; }"
	result, _ := Execute(context.Background(), ExecutionRequest{
		ToolName: "terminal", Payload: command, Shell: true, CWD: f.ws,
		WorkspaceRoots: []string{f.ws, parent}, Sandbox: SandboxAuto, Timeout: 30 * time.Second,
		ToolProfile: ToolProfile{Class: ToolExecutionStandard, Timeout: 30 * time.Second},
	}, map[string]interface{}{"_tenant_id": f.run.tenant, "_tool_name": "terminal", "command": command, "cwd": f.ws})
	if !strings.Contains(result.Output, "cache write ok: sso") {
		t.Fatalf("fixture: the command must run in place: %s", result.Output)
	}
	for _, path := range []string{marker, quarantined} {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("a sandboxed command wrote the runtime's records: %s (%s)", path, result.Output)
		}
	}
	if !strings.Contains(result.Output, "other written") {
		t.Fatalf("the rest of the writable root must stay writable: %s", result.Output)
	}
}

// Item 4: where stopping leftovers is not verified, the guard records no
// canary it could never act on, and the call says the stop was not confirmed.
func TestUnverifiedLeftoverStopRecordsNothing(t *testing.T) {
	f := newGuardFixture(t, "unverified")
	previous := leftoverStopAvailable
	leftoverStopAvailable = func() bool { return false }
	t.Cleanup(func() { leftoverStopAvailable = previous })
	replaceStopLeftovers(t, func(string) ([]int, error) { return nil, sandbox.ErrLeftoverStopUnverified })
	result, _ := f.execute(context.Background(), "true")
	if !strings.Contains(result.Output, "not verified on this architecture") {
		t.Fatalf("the call must say the stop is not verified here: %s", result.Output)
	}
	if entries, _ := os.ReadDir(f.run.runtime.sandboxCanaryDir()); len(entries) != 0 {
		t.Fatalf("a canary nothing can act on was recorded: %v", entries)
	}
}

// Item 5: daemons whose data directories sit side by side share a runtime
// root. One daemon's startup sweep never stops another's running commands.
func TestStartupSweepLeavesAnotherInstancesCommandsAlone(t *testing.T) {
	f := newGuardFixture(t, "instances")
	executionenv.DefaultRegistry().Install([]string{"PATH=/usr/bin:/bin", "HOME=" + f.home}, "inherited", "principal-instances", nil)
	one := NewExecutionRuntime("/data/one", executionenv.RuntimeRoot(), sandbox.PrivateState{}, nil)
	two := NewExecutionRuntime("/data/two", executionenv.RuntimeRoot(), sandbox.PrivateState{}, nil)
	f.run.runtime = one
	canary := sandbox.NewCanary()
	argv, ok := sandbox.WrapSeatbelt(sandbox.Policy{Canary: canary}, []string{"/bin/sleep", "30"})
	if !ok {
		t.Fatal("fixture: cannot wrap a running command")
	}
	running := exec.Command(argv[0], argv[1:]...)
	mustDo(t, running.Start())
	t.Cleanup(func() { _ = running.Process.Kill(); _ = running.Wait() })
	if _, err := f.run.runtime.recordSandboxCanary(canary, envprofiles.HostStatePlan{}); err != nil {
		t.Fatal("fixture: the canary was not recorded")
	}
	waitUnderCanary(t, canary, running.Process.Pid)

	if sweep := two.SweepHostStateAtStartup(); len(sweep.Stopped) != 0 {
		t.Fatalf("another daemon's startup sweep stopped a running command: %v", sweep.Stopped)
	}
	if err := running.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the other daemon's command was stopped: %v", err)
	}
	// The constraint that must change the result: its own daemon's sweep.
	if sweep := one.SweepHostStateAtStartup(); !slices.Contains(sweep.Stopped, running.Process.Pid) {
		t.Fatalf("the daemon's own sweep must stop its leftover: %+v", sweep)
	}
}

func TestGuardedCommandDoesNotStartWithoutRecoveryRecord(t *testing.T) {
	f := newGuardFixture(t, "record-failure")
	mustDo(t, os.MkdirAll(filepath.Dir(f.run.runtime.sandboxCanaryDir()), 0o700))
	mustWrite(t, f.run.runtime.sandboxCanaryDir(), "not a directory")
	output := filepath.Join(f.ws, "effect")
	result, err := f.execute(context.Background(), "printf started > '"+output+"'")
	if err == nil {
		t.Fatalf("command accepted without recovery record: %+v", result)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("command ran without recovery record: %v", err)
	}
}

func TestLegacyCanaryStopsProcessesWithoutInferringCacheScope(t *testing.T) {
	f := newGuardFixture(t, "legacy-record")
	mustDo(t, os.MkdirAll(f.cache, 0o700))
	planted := filepath.Join(f.cache, "legacy-link.json")
	mustDo(t, os.Symlink(f.victim, planted))
	canary := sandbox.NewCanary()
	marker := filepath.Join(f.run.runtime.sandboxCanaryDir(), canary)
	mustDo(t, os.MkdirAll(f.run.runtime.sandboxCanaryDir(), 0o700))
	mustWrite(t, marker, "")
	replaceStopLeftovers(t, func(string) ([]int, error) { return []int{123}, nil })
	sweep := f.run.runtime.SweepHostStateAtStartup()
	if len(sweep.Repairs) != 0 || len(sweep.Stopped) != 1 {
		t.Fatalf("legacy scope widened: %+v", sweep)
	}
	if _, err := os.Lstat(planted); err != nil {
		t.Fatalf("legacy cache changed: %v", err)
	}
}

func TestUnknownRecoveryRecordCannotMutateCache(t *testing.T) {
	f := newGuardFixture(t, "unknown-record")
	canary := sandbox.NewCanary()
	marker := filepath.Join(f.run.runtime.sandboxCanaryDir(), canary)
	mustDo(t, os.MkdirAll(f.run.runtime.sandboxCanaryDir(), 0o700))
	mustWrite(t, marker, `{"version":2,"writes":[]}`)
	replaceStopLeftovers(t, func(string) ([]int, error) { return nil, nil })
	sweep := f.run.runtime.SweepHostStateAtStartup()
	if sweep.Err == nil || len(sweep.Repairs) != 0 {
		t.Fatalf("unknown record accepted: %+v", sweep)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("unsupported record lost: %v", err)
	}
}

func TestRecoveryRecordRejectsReplacedCacheAncestor(t *testing.T) {
	f := newGuardFixture(t, "changed-ancestor")
	mustDo(t, os.MkdirAll(f.cache, 0o700))
	marker, err := f.run.runtime.recordSandboxCanary(sandbox.NewCanary(), envprofiles.HostStatePlan{Writes: []envprofiles.HostStateWrite{{Profile: "aws", Path: f.cache, Root: filepath.Dir(filepath.Dir(f.cache)), Flat: true}}})
	mustDo(t, err)
	external := filepath.Join(f.ws, "foreign-cache")
	mustDo(t, os.MkdirAll(filepath.Join(external, "cache"), 0o700))
	planted := filepath.Join(external, "cache", "link.json")
	mustDo(t, os.Symlink(f.victim, planted))
	mustDo(t, os.RemoveAll(filepath.Dir(f.cache)))
	mustDo(t, os.Symlink(external, filepath.Dir(f.cache)))
	replaceStopLeftovers(t, func(string) ([]int, error) { return nil, nil })
	sweep := f.run.runtime.SweepHostStateAtStartup()
	if sweep.Err == nil || len(sweep.Repairs) != 0 {
		t.Fatalf("changed ancestor accepted: %+v", sweep)
	}
	if _, err := os.Lstat(planted); err != nil {
		t.Fatalf("foreign cache changed: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("unresolved scope lost: %v", err)
	}
}
