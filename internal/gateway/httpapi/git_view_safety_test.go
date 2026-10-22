package httpapi

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
)

type gitViewCommandFixture struct {
	daemon *Server
	store  *control.Store
	owner  *control.IdentityContext
	task   *control.Task
	root   string
	view   executionenv.GitView
	roots  []executionenv.RootBinding
	run    *control.Run
}

// newGitViewCommandFixture binds one managed view to an owned Run that has
// committed work in it. The Run is left in the requested status.
func newGitViewCommandFixture(t *testing.T, status string) gitViewCommandFixture {
	t.Helper()
	daemon, store, owner, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	root := gitAdmissionFixture(t)
	baseline, err := executionenv.InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	view, err := executionenv.EnsureGitView(ctx, store.ExecutionViewsDir(), "view-safety", baseline)
	if err != nil {
		t.Fatal(err)
	}
	roots := []executionenv.RootBinding{{Path: view.Path, Role: executionenv.RootRolePrimary,
		AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceExecutionView,
		ContextRoot: true, GitBaseline: &baseline}}
	run, err := store.StartRunWithOptions(ctx, task, "cli", "edit in view", control.StartRunOptions{ExecutionRoots: roots})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), []byte("delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "file.txt"}, {"commit", "-qm", "from isolated run"}} {
		if output, err := exec.Command("git", append([]string{"-C", view.Path}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	if status != "running" {
		if err := store.FinishRun(ctx, owner.TenantID, run.ID, status); err != nil {
			t.Fatal(err)
		}
	}
	return gitViewCommandFixture{daemon: daemon, store: store, owner: owner, task: task, root: root, view: view, roots: roots, run: run}
}

func (f gitViewCommandFixture) command(t *testing.T, input string) (string, error) {
	t.Helper()
	handled, reply, _, err := f.daemon.tryHandleControlCommand(context.Background(), f.owner, api.MessageRequest{Content: input})
	if !handled {
		t.Fatalf("%s escaped the control path", input)
	}
	return reply, err
}

func plantViewCommonDirFilter(t *testing.T, viewPath, commonDir, config string) {
	t.Helper()
	gitDir := filepath.Join(viewPath, ".git")
	target := commonDir
	if !filepath.IsAbs(target) {
		target = filepath.Join(gitDir, commonDir)
	}
	for _, dir := range []string{filepath.Join(target, "objects", "info"), filepath.Join(target, "refs")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(target, "objects", "info", "alternates"): filepath.Join(gitDir, "objects") + "\n",
		filepath.Join(target, "config"):                        config,
		filepath.Join(gitDir, "commondir"):                     commonDir + "\n",
		filepath.Join(viewPath, ".gitattributes"):              "* filter=x\n",
		filepath.Join(viewPath, "file.txt"):                    "delivered\n", // identical bytes, new stat data
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestViewCommandsNeverRunRedirectedViewConfigOnHost(t *testing.T) {
	for _, shape := range []string{"relative-commondir", "absolute-commondir-include"} {
		t.Run(shape, func(t *testing.T) {
			f := newGitViewCommandFixture(t, "done")
			marker := filepath.Join(t.TempDir(), "MARKER")
			switch shape {
			case "relative-commondir":
				plantViewCommonDirFilter(t, f.view.Path, "evil",
					"[core]\n\trepositoryformatversion = 0\n\tbare = false\n[filter \"x\"]\n\tclean = touch '"+marker+"'; cat\n")
			case "absolute-commondir-include":
				include := filepath.Join(t.TempDir(), "included")
				if err := os.WriteFile(include, []byte("[filter \"x\"]\n\tclean = touch '"+marker+"'; cat\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				plantViewCommonDirFilter(t, f.view.Path, filepath.Join(t.TempDir(), "common"),
					"[core]\n\trepositoryformatversion = 0\n\tbare = false\n[include]\n\tpath = "+include+"\n")
			}
			if reply, err := f.command(t, "/views "+f.run.ID); err == nil {
				t.Fatalf("/views inspected a redirected view: %q", reply)
			}
			if reply, err := f.command(t, "/apply "+f.run.ID); err == nil {
				t.Fatalf("/apply delivered a redirected view: %q", reply)
			}
			if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("view-writable Git state executed a host command: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(f.view.Path, ".git", "commondir")); err != nil {
				t.Fatalf("rejected view was not preserved for inspection: %v", err)
			}
		})
	}
}

// recordGatewayGitCalls returns the subcommands and work trees of every git
// process fn starts, observed through a wrapper first on PATH.
func recordGatewayGitCalls(t *testing.T, fn func()) []string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	bin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "git.log")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"$(pwd -P)\" \"$*\" >> '" + logPath + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fn()
	data, err := os.ReadFile(logPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func requireNoCheckoutReads(t *testing.T, viewPath string, calls []string) {
	t.Helper()
	for _, call := range calls {
		cwd, args, _ := strings.Cut(call, "|")
		if cwd != viewPath {
			continue
		}
		for _, filterCapable := range []string{" status ", " ls-files ", " diff-files", " checkout ", " add "} {
			if strings.Contains(" "+args+" ", filterCapable) {
				t.Fatalf("host-side git read the checkout while a Run may still write it: git %s", args)
			}
		}
	}
}

func TestViewDetailDoesNotReadCheckoutWhileARunMayWriteIt(t *testing.T) {
	for _, writer := range []string{"owning-run-running", "continuation-running", "admission-in-flight"} {
		t.Run(writer, func(t *testing.T) {
			ctx := context.Background()
			status := "done"
			switch writer {
			case "owning-run-running":
				status = "running"
			case "continuation-running":
				status = "interrupted"
			}
			f := newGitViewCommandFixture(t, status)
			switch writer {
			case "continuation-running":
				if _, err := f.store.StartRunWithOptions(ctx, f.task, "cli", "continue", control.StartRunOptions{ExecutionRoots: f.roots, ResumesRunID: f.run.ID}); err != nil {
					t.Fatal(err)
				}
			case "admission-in-flight":
				active := &activeRun{TenantID: f.owner.TenantID, PersonID: f.owner.PersonID, Platform: "cli", Channel: "other",
					ExecutionRoots: executionenv.CloneRootBindings(f.roots)}
				if !f.daemon.coordinator().beginActive(f.owner.PersonID, active) {
					t.Fatal("could not register the in-flight admission")
				}
				defer f.daemon.coordinator().endActiveRun(f.owner.PersonID, active)
			}
			var reply string
			calls := recordGatewayGitCalls(t, func() {
				var err error
				if reply, err = f.command(t, "/views "+f.run.ID); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(reply, "Committed files: 1") || !strings.Contains(reply, "not inspected while") {
				t.Fatalf("view detail did not explain the skipped checkout state: %q", reply)
			}
			requireNoCheckoutReads(t, f.view.Path, calls)
			if _, err := f.command(t, "/apply "+f.run.ID); err == nil {
				t.Fatal("/apply delivered a view a Run may still write")
			}
		})
	}
}

func TestViewArchiveRefusesWhileARunIsBeingAdmittedIntoTheView(t *testing.T) {
	f := newGitViewCommandFixture(t, "done")
	if _, err := f.command(t, "/apply "+f.run.ID); err != nil {
		t.Fatal(err)
	}
	coordinator := f.daemon.coordinator()
	active := &activeRun{TenantID: f.owner.TenantID, PersonID: f.owner.PersonID, Platform: "cli", Channel: "resume",
		ExecutionRoots: executionenv.CloneRootBindings(f.roots)}
	if !coordinator.beginActive(f.owner.PersonID, active) {
		t.Fatal("could not register the in-flight admission")
	}
	if _, err := f.command(t, "/views archive "+f.run.ID); err == nil || !strings.Contains(err.Error(), "starting or running") {
		t.Fatalf("archive moved a view while a Run was being admitted into it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.view.Path, "file.txt")); err != nil {
		t.Fatalf("refused archive moved the view: %v", err)
	}
	coordinator.endActiveRun(f.owner.PersonID, active)
	if _, err := f.command(t, "/views archive "+f.run.ID); err != nil {
		t.Fatalf("archive stayed blocked after admission finished: %v", err)
	}
	// The archived checkout is the prune variant of the same race.
	if !coordinator.beginActive(f.owner.PersonID, active) {
		t.Fatal("could not register the second in-flight admission")
	}
	if _, err := f.command(t, "/views prune "+f.run.ID); err == nil || !strings.Contains(err.Error(), "starting or running") {
		t.Fatalf("prune deleted a view while a Run was being admitted into it: %v", err)
	}
	coordinator.endActiveRun(f.owner.PersonID, active)
	if _, err := os.Stat(filepath.Join(f.store.ExecutionViewsDir(), ".retired", f.view.ID, "file.txt")); err != nil {
		t.Fatalf("refused prune lost the archive: %v", err)
	}
}

func TestResumeAfterArchiveExplainsRecoveryAndRestoreStillWorks(t *testing.T) {
	ctx := context.Background()
	f := newGitViewCommandFixture(t, "done")
	if _, err := f.command(t, "/apply "+f.run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.command(t, "/views archive "+f.run.ID); err != nil {
		t.Fatal(err)
	}
	// The loser of the race is a resume admitted after the move: admission
	// parks it as waiting_user through the environment check.
	if _, err := executionRootUnavailable(ctx, f.roots); err == nil ||
		!strings.Contains(err.Error(), "archived") || !strings.Contains(err.Error(), "/views restore") {
		t.Fatalf("resume of an archived view got no recovery instruction: %v", err)
	}
	parked, err := f.store.StartRunWithOptions(ctx, f.task, "cli", "continue", control.StartRunOptions{ExecutionRoots: f.roots})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.FinishRun(ctx, f.owner.TenantID, parked.ID, "waiting_user"); err != nil {
		t.Fatal(err)
	}
	if reply, err := f.command(t, "/views restore "+parked.ID); err != nil || !strings.Contains(reply, "Restored execution view") {
		t.Fatalf("parked work could not restore its own view: %q %v", reply, err)
	}
	if path, err := executionRootUnavailable(ctx, f.roots); err != nil {
		t.Fatalf("restored view is still unavailable at %s: %v", path, err)
	}
	if _, err := f.command(t, "/views archive "+f.run.ID); err == nil || !strings.Contains(err.Error(), "still refers") {
		t.Fatalf("archive ignored the parked Run that refers to the view: %v", err)
	}
}

func TestViewRetentionMoveAndRunAdmissionAreMutuallyExclusive(t *testing.T) {
	ctx := context.Background()
	f := newGitViewCommandFixture(t, "done")
	coordinator := f.daemon.coordinator()
	usage, err := f.store.ExecutionViewUsage(ctx, f.owner.TenantID, f.owner.PersonID, f.view.Path)
	if err != nil {
		t.Fatal(err)
	}
	guard := coordinator.executionViewMoveGuard(ctx, f.owner, f.view.Path, usage)
	entered, release, moved := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		moved <- guard(func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	active := &activeRun{TenantID: f.owner.TenantID, PersonID: f.owner.PersonID, Platform: "cli", Channel: "resume",
		ExecutionRoots: executionenv.CloneRootBindings(f.roots)}
	registered := make(chan bool, 1)
	go func() { registered <- coordinator.beginActive(f.owner.PersonID, active) }()
	select {
	case <-registered:
		t.Fatal("a Run registered the view while its retention move was in progress")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-moved; err != nil {
		t.Fatal(err)
	}
	if !<-registered {
		t.Fatal("admission was refused instead of waiting for the move to finish")
	}
	called := false
	if err := guard(func() error { called = true; return nil }); !errors.Is(err, errExecutionViewAdmitting) || called {
		t.Fatalf("move ran beside a registered admission: called=%t err=%v", called, err)
	}
	coordinator.endActiveRun(f.owner.PersonID, active)
}

func TestViewRetentionMoveRefusesRunsAdmittedDuringItsInspection(t *testing.T) {
	for _, outcome := range []string{"done", "waiting_user"} {
		t.Run(outcome, func(t *testing.T) {
			ctx := context.Background()
			f := newGitViewCommandFixture(t, "done")
			checked, err := f.store.ExecutionViewUsage(ctx, f.owner.TenantID, f.owner.PersonID, f.view.Path)
			if err != nil {
				t.Fatal(err)
			}
			// A resume starts, works in the view, and leaves the registry while
			// the slow Git inspection runs.
			resumed, err := f.store.StartRunWithOptions(ctx, f.task, "cli", "quick follow-up", control.StartRunOptions{ExecutionRoots: f.roots})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.FinishRun(ctx, f.owner.TenantID, resumed.ID, outcome); err != nil {
				t.Fatal(err)
			}
			want := errExecutionViewUsedDuringCheck
			if outcome == "waiting_user" {
				want = errExecutionViewPending
			}
			called := false
			guard := f.daemon.coordinator().executionViewMoveGuard(ctx, f.owner, f.view.Path, checked)
			if err := guard(func() error { called = true; return nil }); !errors.Is(err, want) || called {
				t.Fatalf("stale inspection was allowed to move the view: called=%t err=%v", called, err)
			}
		})
	}
}

func TestResumeAfterPruneNamesTheRetainedCommit(t *testing.T) {
	ctx := context.Background()
	f := newGitViewCommandFixture(t, "done")
	for _, input := range []string{"/apply ", "/views archive ", "/views prune "} {
		if reply, err := f.command(t, input+f.run.ID); err != nil {
			t.Fatalf("%s: %q %v", input, reply, err)
		}
	}
	if _, err := executionRootUnavailable(ctx, f.roots); err == nil ||
		!strings.Contains(err.Error(), "pruned") || !strings.Contains(err.Error(), "refs/selfmind/retained/"+f.view.ID) {
		t.Fatalf("resume of a pruned view did not name the retained commit: %v", err)
	}
}
