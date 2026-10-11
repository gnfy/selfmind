package executionenv

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gitStandIn puts a git first on PATH that runs action, a line of shell, for
// the named subcommands, and the real git for everything else. An action that
// does not exec falls through to the real git with the same arguments.
func gitStandIn(t *testing.T, action string, subcommands ...string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nfor arg in \"$@\"; do\n\tcase \"$arg\" in\n\t%s)\n\t\t%s\n\t\tbreak\n\t\t;;\n\tesac\ndone\nexec '%s' \"$@\"\n",
		strings.Join(subcommands, "|"), action, real)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stuckGit puts a stand-in on PATH whose subcommand never answers, and returns
// a context that is cancelled once that command has started (or after a
// bound, when it never does), with the file that records the start.
func stuckGit(t *testing.T, subcommand string) (context.Context, string) {
	t.Helper()
	started := filepath.Join(t.TempDir(), "started")
	gitStandIn(t, fmt.Sprintf("touch '%s'; exec sleep 30", started), subcommand)
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(stop)
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(started); err == nil {
				stop()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return ctx, started
}

// plantAlternatesAction is stand-in shell that adds an alternates entry to the
// source while the command runs, as a concurrent writer of the source could.
func plantAlternatesAction(t *testing.T, gitDir string) string {
	t.Helper()
	other := cleanGitFixture(t)
	return fmt.Sprintf("printf '%%s\\n' '%s' > '%s'", filepath.Join(other, ".git", "objects"),
		filepath.Join(gitDir, "objects", "info", "alternates"))
}

// The pack goes from the view into the source through an OS pipe between the
// two Git processes, so the daemon never holds it: a pack four times the
// allowance passes through.
func TestImportStreamsThePackWithoutHoldingIt(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "large", baseline)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 32<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, "large.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, view.Path, "add", "large.bin")
	baselineGit(t, view.Path, "commit", "-qm", "large work")
	info, err := InspectGitViewDelivery(ctx, views, view.ID, baseline)
	if err != nil {
		t.Fatal(err)
	}
	data = nil
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err = importGitViewCommit(ctx, baseline, info)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("importing a %d MiB pack allocated %d bytes in the daemon", 32, allocated)
	}
	if got := baselineGit(t, root, "cat-file", "-s", info.HeadCommit+":large.bin"); got != strconv.Itoa(32<<20) {
		t.Fatalf("imported blob size = %s", got)
	}
}

// Stopping an import stops both Git processes, whichever side is stuck, and
// says the import did not finish, whether it was cancelled or ran into its
// deadline.
func TestImportCancellationStopsBothSides(t *testing.T) {
	for _, stuck := range []string{"pack-objects", "index-pack"} {
		for _, deadline := range []bool{false, true} {
			name := stuck + "/cancelled"
			if deadline {
				name = stuck + "/timed out"
			}
			t.Run(name, func(t *testing.T) {
				f := newSourceAttackFixture(t)
				info, err := InspectGitViewDelivery(context.Background(), f.views, f.pending.ID, f.baseline)
				if err != nil {
					t.Fatal(err)
				}
				ctx, startedFile := stuckGit(t, stuck)
				want := error(context.Canceled)
				if deadline {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(context.Background(), 3*time.Second)
					defer stop()
					want = context.DeadlineExceeded
				}
				started := time.Now()
				err = importGitViewCommit(ctx, f.baseline, info)
				if !errors.Is(err, want) || !strings.Contains(err.Error(), "git pack-objects into git index-pack did not finish") {
					t.Fatalf("interrupted import = %v", err)
				}
				if _, statErr := os.Stat(startedFile); statErr != nil {
					t.Fatalf("the stuck %s never started: %v", stuck, statErr)
				}
				if elapsed := time.Since(started); elapsed > 10*time.Second {
					t.Fatalf("the import waited %v for a stopped Git", elapsed)
				}
			})
		}
	}
}

// A single host Git command that is stopped reports that it did not finish,
// with the cause, rather than a bare signal.
func TestHostGitCommandReportsInterruption(t *testing.T) {
	f := newSourceAttackFixture(t)
	ctx, startedFile := stuckGit(t, "config")
	_, err := gitSourceCommand(ctx, f.baseline, "config", "--get", "user.name")
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "git config did not finish") {
		t.Fatalf("interrupted host Git = %v", err)
	}
	if _, statErr := os.Stat(startedFile); statErr != nil {
		t.Fatalf("the stuck config never started: %v", statErr)
	}
}

// When the producing side fails, the consuming side fails too, and the error
// carries each one's own message.
func TestImportReportsEachSideWithItsOwnMessage(t *testing.T) {
	f := newSourceAttackFixture(t)
	info, err := InspectGitViewDelivery(context.Background(), f.views, f.pending.ID, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	gitStandIn(t, "echo 'stand-in pack-objects gave up' >&2; exit 3", "pack-objects")
	err = importGitViewCommit(context.Background(), f.baseline, info)
	if err == nil || !strings.Contains(err.Error(), "git pack-objects: exit status 3: stand-in pack-objects gave up") ||
		!strings.Contains(err.Error(), "git index-pack: exit status") {
		t.Fatalf("failed import = %v", err)
	}
}

// An alternates entry the source gains while objects are imported could let
// the import's own checks pass on borrowed objects; the import refuses it.
func TestImportRefusesAlternatesAddedWhileItRuns(t *testing.T) {
	f := newSourceAttackFixture(t)
	info, err := InspectGitViewDelivery(context.Background(), f.views, f.pending.ID, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	gitStandIn(t, plantAlternatesAction(t, f.gitDir), "index-pack")
	if err := importGitViewCommit(context.Background(), f.baseline, info); err == nil || !strings.Contains(err.Error(), "alternates") {
		t.Fatalf("an import that ran while the source gained alternates = %v", err)
	}
}
