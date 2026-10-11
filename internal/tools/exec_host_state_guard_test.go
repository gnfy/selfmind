//go:build darwin

package tools

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"selfmind/internal/executionenv"
	"selfmind/internal/tools/envprofiles"
	"selfmind/internal/tools/sandbox"
)

// guardFixture is a trusted run on a host whose AWS SSO cache is written in
// place, with a workspace holding a link and a pipe a planter can swap in.
type guardFixture struct {
	run    *boundaryRun
	home   string
	ws     string
	cache  string
	victim string
	helper string
}

func newGuardFixture(t *testing.T, name string) guardFixture {
	t.Helper()
	requireInPlaceBackend(t)
	withExecSandboxPolicy(t, true, false, false)
	helper, err := os.Executable()
	if err != nil {
		t.Skip("no path to this test binary")
	}
	base := fixtureBase(t)
	home := awsStateHome(t, base, ssoProfileConfig)
	run := newBoundaryRun(t, "guard-"+name, home, writeStub(t, base, "aws", awsCredentialStateStub),
		executionenv.TrustTrusted, ApprovalOnRequest, nil)
	f := guardFixture{run: run, home: home, ws: canonicalPath(t, run.workspace), victim: filepath.Join(base, "victim.json"), helper: helper,
		cache: filepath.Join(canonicalPath(t, home), ".aws", "sso", "cache")}
	mustWrite(t, f.victim, "victim")
	mustDo(t, os.Symlink(f.victim, filepath.Join(f.ws, "link")))
	return f
}

// waitUnderCanary waits until sandbox-exec has applied a leftover's policy, so
// the check can find the process under its canary however loaded the host is.
func waitUnderCanary(t *testing.T, canary string, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if found, err := sandbox.ProcessesUnderCanary(canary); err == nil && slices.Contains(found, pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fixture: process %d never ran under its canary", pid)
}

func (f guardFixture) plant(operation, from, to string) string {
	return "'" + f.helper + "' " + hostStatePlantVerb + " " + operation + " '" + from + "' '" + to + "'"
}

func (f guardFixture) execute(ctx context.Context, command string) (ExecutionResult, error) {
	command = "aws sts get-caller-identity --profile sso-dev && " + command
	return Execute(ctx, ExecutionRequest{
		ToolName: "terminal", Payload: command, Shell: true, CWD: f.ws,
		WorkspaceRoots: []string{f.ws}, Sandbox: SandboxAuto, Timeout: 30 * time.Second,
		ToolProfile: ToolProfile{Class: ToolExecutionStandard, Timeout: 30 * time.Second},
	}, map[string]interface{}{"_tenant_id": f.run.tenant, "_tool_name": "terminal", "command": command, "cwd": f.ws})
}

func (f guardFixture) quarantined(t *testing.T) int {
	t.Helper()
	count := 0
	_ = filepath.WalkDir(f.run.runtime.hostStateQuarantineRoot(), func(_ string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			count++
		}
		return nil
	})
	return count
}

// The review probe (item 2): a planter that called setsid left the command's
// process group, planted 0.7 s after the call returned, and lived on. The
// command's sandbox carries a per-command canary, so every process still
// under it is found and stopped before the cache is scanned, whatever group
// or session it is in; the call says so. The variant without setsid is the
// background planter of the round-4 probe.
func TestLeftoverProcessesUnderTheCommandsSandboxAreStopped(t *testing.T) {
	for _, tc := range []struct{ name, operation string }{
		{"setsid", "detach-swap"},
		{"same-group", "swap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardFixture(t, "leftover-"+tc.name)
			planter := f.plant(tc.operation, f.ws+"/link", f.cache+"/stub-sso-dev.json")
			if tc.operation == "swap" {
				planter = "(sleep 1.5; " + planter + ")"
			}
			// The shell lingers so the planter has left the group before the
			// command ends: only the canary can find it then.
			result, _ := f.execute(context.Background(), "{ "+planter+" >/dev/null 2>&1 & } ; sleep 0.3")
			if !strings.Contains(result.Output, "cache write ok: sso") {
				t.Fatalf("fixture: the sandboxed aws must have written its cache: %s", result.Output)
			}
			time.Sleep(2500 * time.Millisecond)
			requireNoPlantedEntry(t, f.cache)
			if data, _ := os.ReadFile(f.victim); string(data) != "victim" {
				t.Fatal("the file a planted link pointed at changed")
			}
			if tc.operation == "detach-swap" && !strings.Contains(result.Output, "stopped 1 process this command left running under its sandbox") {
				t.Fatalf("the call must say it stopped the process its sandbox left: %s", result.Output)
			}
		})
	}
}

// The review probe (item 2): after a leftover planted an entry, the next aws
// call was refused with "seatbelt cannot enforce this plan". What a cache
// holds before a command is moved into the quarantine and the command runs.
func TestAPlantedEntryIsRepairedBeforeTheNextCommandRuns(t *testing.T) {
	for _, tc := range []struct{ name string }{{"symlink"}, {"fifo"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardFixture(t, "before-"+tc.name)
			mustDo(t, os.MkdirAll(f.cache, 0o700))
			planted := filepath.Join(f.cache, "planted.json")
			if tc.name == "symlink" {
				mustDo(t, os.Symlink(f.victim, planted))
			} else {
				mustDo(t, exec.Command("mkfifo", planted).Run())
			}
			result, err := f.execute(context.Background(), "true")
			if err != nil || !strings.Contains(result.Output, "cache write ok: sso") {
				t.Fatalf("a planted entry must not refuse the command: %v (%s)", err, result.Output)
			}
			if !strings.Contains(result.Output, "repaired the credential cache before this command") {
				t.Fatalf("the call must say what it repaired first: %s", result.Output)
			}
			requireNoPlantedEntry(t, f.cache)
			if f.quarantined(t) == 0 {
				t.Fatal("the planted entry is not in the quarantine")
			}
		})
	}
}

// Item 1: the repair runs from a deferred step, so a panic after the command
// starts cannot skip it: the cache is still scanned and the guarded command
// is no longer counted as in flight.
func TestAPanicAfterTheStartStillRepairsTheCache(t *testing.T) {
	f := newGuardFixture(t, "panic")
	previous := afterGuardedStart
	afterGuardedStart = func() {
		if _, err := os.Stat(filepath.Join(f.cache, "stub-sso-dev.json")); err != nil {
			t.Fatalf("fixture: the command must write its cache before the panic: %v", err)
		}
		// Inject the unsafe entry at the failure boundary independently of
		// whether this kernel permits a sandboxed RENAME_SWAP attack.
		mustDo(t, os.MkdirAll(f.cache, 0o700))
		mustDo(t, os.Symlink(f.victim, filepath.Join(f.cache, "panic-planted.json")))
		panic("injected after the command started")
	}
	t.Cleanup(func() { afterGuardedStart = previous })
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("fixture: the injected panic did not happen")
			}
		}()
		_, _ = f.execute(context.Background(), "true")
	}()
	requireNoPlantedEntry(t, f.cache)
	if f.quarantined(t) == 0 {
		t.Fatal("the planted entry is not in the quarantine")
	}
	if !f.run.runtime.WaitForHostStateRepairs(0) {
		t.Fatal("a guarded command interrupted by a panic is still counted as in flight")
	}
}

// Item 1: shutdown waits, bounded, for a guarded command that is still
// running — cancelled but not yet repaired — instead of exiting under it.
func TestShutdownWaitsForInFlightRepairs(t *testing.T) {
	f := newGuardFixture(t, "shutdown")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan ExecutionResult, 1)
	go func() {
		result, _ := f.execute(ctx, "sleep 2 && "+f.plant("swap", f.ws+"/link", f.cache+"/stub-sso-dev.json"))
		done <- result
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(f.cache, "stub-sso-dev.json")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The constraint that must change the result: a wait shorter than the
	// command reports that a repair is still pending.
	if f.run.runtime.WaitForHostStateRepairs(100 * time.Millisecond) {
		t.Fatal("no in-flight repair was seen while the guarded command ran")
	}
	cancel()
	if !f.run.runtime.WaitForHostStateRepairs(10 * time.Second) {
		t.Fatal("the in-flight repair did not finish within the shutdown wait")
	}
	select {
	case <-done:
	default:
		t.Fatal("the wait returned before the guarded command finished its repair")
	}
	requireNoPlantedEntry(t, f.cache)
}

// Item 1: a daemon that crashed or was killed never reached a command's own
// repair. Startup stops every process still under a recorded canary — the
// command's sandbox outlives the daemon — then repairs its recorded cache paths.
func TestStartupSweepStopsLeftoversAndRepairsRecordedCaches(t *testing.T) {
	f := newGuardFixture(t, "startup")
	registry := executionenv.DefaultRegistry()
	registry.Install([]string{"PATH=/usr/bin:/bin", "HOME=" + f.home}, "inherited", "principal-startup", nil)
	mustDo(t, os.MkdirAll(f.cache, 0o700))
	mustDo(t, os.Symlink(f.victim, filepath.Join(f.cache, "planted.json")))
	canary := newSandboxCanary()
	argv, ok := sandbox.WrapSeatbelt(sandbox.Policy{Canary: canary}, []string{"/bin/sleep", "30"})
	if !ok {
		t.Fatal("fixture: cannot wrap a leftover")
	}
	leftover := exec.Command(argv[0], argv[1:]...)
	mustDo(t, leftover.Start())
	t.Cleanup(func() { _ = leftover.Process.Kill(); _ = leftover.Wait() })
	marker, recordErr := f.run.runtime.recordSandboxCanary(canary, envprofiles.HostStatePlan{Writes: []envprofiles.HostStateWrite{{Profile: "aws", Path: f.cache, Root: filepath.Dir(filepath.Dir(f.cache)), Flat: true}}})
	mustDo(t, recordErr)
	if marker == "" {
		t.Fatal("fixture: the canary was not recorded")
	}
	waitUnderCanary(t, canary, leftover.Process.Pid)

	sweep := f.run.runtime.SweepHostStateAtStartup()
	if len(sweep.Repairs) != 1 || sweep.Repairs[0].Kind != "symbolic link" || sweep.Err != nil {
		t.Fatalf("the startup sweep must repair the planted link: %+v", sweep)
	}
	if len(sweep.Stopped) != 1 || sweep.Stopped[0] != leftover.Process.Pid {
		t.Fatalf("the startup sweep must stop the process left under the recorded canary: %+v", sweep.Stopped)
	}
	if err := leftover.Wait(); err == nil {
		t.Fatal("the leftover is still running")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the canary record was kept after its processes were stopped: %v", err)
	}
	requireNoPlantedEntry(t, f.cache)
	if status := f.run.runtime.HostStateQuarantineStatus(); !strings.Contains(status, "startup sweep") || !strings.Contains(status, "1 entry quarantined") {
		t.Fatalf("/diag must show the sweep: %q", status)
	}
	// The constraint that must change the result: a clean host.
	if again := f.run.runtime.SweepHostStateAtStartup(); len(again.Repairs) != 0 || len(again.Stopped) != 0 {
		t.Fatalf("a clean host was repaired: %+v", again)
	}
}

func TestStartupSweepDoesNotRepairCachesWithoutAnInterruptedCommand(t *testing.T) {
	f := newGuardFixture(t, "untouched-startup")
	executionenv.DefaultRegistry().Install([]string{"PATH=/usr/bin:/bin", "HOME=" + f.home}, "inherited", "principal-untouched", nil)
	mustDo(t, os.MkdirAll(f.cache, 0o700))
	planted := filepath.Join(f.cache, "foreign-link.json")
	mustDo(t, os.Symlink(f.victim, planted))
	sweep := f.run.runtime.SweepHostStateAtStartup()
	if len(sweep.Repairs) != 0 {
		t.Fatalf("startup repaired an unrecorded cache: %+v", sweep)
	}
	if _, err := os.Lstat(planted); err != nil {
		t.Fatalf("unrecorded cache changed: %v", err)
	}
}

func TestStartupSweepRepairsOnlyTheRecordedWriteSet(t *testing.T) {
	f := newGuardFixture(t, "scoped-startup")
	executionenv.DefaultRegistry().Install([]string{"PATH=/usr/bin:/bin", "HOME=" + f.home}, "inherited", "principal-scoped", nil)
	other := filepath.Join(f.home, ".aws", "cli", "cache")
	for _, cache := range []string{f.cache, other} {
		mustDo(t, os.MkdirAll(cache, 0o700))
		mustDo(t, os.Symlink(f.victim, filepath.Join(cache, "planted.json")))
	}
	canary := sandbox.NewCanary()
	_, recordErr := f.run.runtime.recordSandboxCanary(canary, envprofiles.HostStatePlan{Writes: []envprofiles.HostStateWrite{{Profile: "aws", Path: f.cache, Root: filepath.Dir(filepath.Dir(f.cache)), Flat: true}}})
	mustDo(t, recordErr)
	replaceStopLeftovers(t, func(string) ([]int, error) { return nil, nil })
	sweep := f.run.runtime.SweepHostStateAtStartup()
	if len(sweep.Repairs) != 1 || sweep.Repairs[0].Path != filepath.Join(f.cache, "planted.json") {
		t.Fatalf("wrong repair scope: %+v", sweep)
	}
	if _, err := os.Lstat(filepath.Join(other, "planted.json")); err != nil {
		t.Fatalf("unrelated cache changed: %v", err)
	}
}
