package executionenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// reftableGitFixture is cleanGitFixture with reftable ref storage. Git 2.45
// and later can create one; older Git skips these cases.
func reftableGitFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "--ref-format=reftable", root)
	cmd.Env = cleanGitProbeEnv(os.Environ())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this Git cannot create reftable repositories: %v: %s", err, output)
	}
	baselineGit(t, root, "config", "user.name", "Test")
	baselineGit(t, root, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, root, "add", "file.txt")
	baselineGit(t, root, "commit", "-qm", "base")
	return root
}

func commitInGitView(t *testing.T, view GitView, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, view.Path, "add", "file.txt")
	baselineGit(t, view.Path, "commit", "-qm", "work: "+content)
	return baselineGit(t, view.Path, "rev-parse", "HEAD")
}

// A reftable source keeps its refs in reftable/, not in loose files. Ref
// creation must write where the source's own Git reads, and both delivery
// and prune must prove that before reporting success or deleting anything.
func TestReftableSourceDeliversAndProtectsPrunedWork(t *testing.T) {
	ctx := context.Background()
	root := reftableGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "reftable", baseline)
	if err != nil {
		t.Fatal(err)
	}
	head := commitInGitView(t, view, "reftable work\n")
	delivered, err := DeliverGitViewBranch(ctx, views, view.ID, baseline)
	if err != nil || delivered.DeliveredBranch != "selfmind/"+view.ID {
		t.Fatalf("reftable delivery = %+v %v", delivered, err)
	}
	if got := baselineGit(t, root, "rev-parse", "--verify", "refs/heads/selfmind/"+view.ID+"^{commit}"); got != head {
		t.Fatalf("the source repository resolves the delivered branch to %s, want %s", got, head)
	}
	if again, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil || again.DeliveredBranch != delivered.DeliveredBranch {
		t.Fatalf("repeat reftable delivery was not idempotent: %+v %v", again, err)
	}
	retired, err := RetireGitView(ctx, views, view.ID, baseline, nil)
	if err != nil {
		t.Fatal(err)
	}
	pruned, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil)
	if err != nil {
		t.Fatalf("reftable prune: %v", err)
	}
	if got := baselineGit(t, root, "rev-parse", "--verify", pruned.RetainedRef+"^{commit}"); got != head {
		t.Fatalf("the source repository resolves the protected ref to %s, want %s", got, head)
	}
	if _, err := InspectPrunedGitView(ctx, views, view.ID, baseline); err != nil {
		t.Fatalf("pruned reftable view does not inspect: %v", err)
	}
	if _, err := os.Lstat(retired.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pruned checkout was not reclaimed: %v", err)
	}
}

// Prune deletes a checkout on the strength of the protected source ref, so the
// ref is checked against the source's own view of its refs immediately before
// the checkout leaves restore's path. The guard runs just before that move.
func TestPruneKeepsTheArchiveWhenTheProtectedRefCannotBeVerified(t *testing.T) {
	cases := map[string]func(t *testing.T, root, ref string){
		"protected ref removed": func(t *testing.T, root, ref string) {
			baselineGit(t, root, "update-ref", "-d", ref)
		},
		"protected ref moved": func(t *testing.T, root, ref string) {
			baselineGit(t, root, "update-ref", ref, baselineGit(t, root, "rev-parse", "HEAD"))
		},
	}
	for name, interfere := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := cleanGitFixture(t)
			baseline, err := InspectCleanGitBaseline(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			views := filepath.Join(t.TempDir(), "views")
			view, err := EnsureGitView(ctx, views, "verified", baseline)
			if err != nil {
				t.Fatal(err)
			}
			commitInGitView(t, view, "delivered\n")
			if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
				t.Fatal(err)
			}
			retired, err := RetireGitView(ctx, views, view.ID, baseline, nil)
			if err != nil {
				t.Fatal(err)
			}
			interfering := func(move func() error) error {
				interfere(t, root, retainedGitViewRef(view.ID))
				return move()
			}
			if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, interfering); err == nil || !strings.Contains(err.Error(), "protected") {
				t.Fatalf("prune deleted a checkout whose protected ref the source does not hold: %v", err)
			}
			if _, err := os.Stat(filepath.Join(retired.Path, "file.txt")); err != nil {
				t.Fatalf("archive was not kept after a failed verification: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(views, ".retired", ".pruned", view.ID+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a prune record was written although verification failed: %v", err)
			}
		})
	}
}

// A commondir or gitdir indirection present before freeze would make the
// baseline's common directory another repository, which delivery then wrote to.
func TestBaselineFreezeRefusesARedirectedGitDirectory(t *testing.T) {
	ctx := context.Background()
	t.Run("commondir to another repository", func(t *testing.T) {
		root := cleanGitFixture(t)
		other := filepath.Join(t.TempDir(), "other")
		baselineGit(t, root, "clone", "-q", root, other)
		if err := os.WriteFile(filepath.Join(root, ".git", "commondir"), []byte(filepath.Join(other, ".git")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if baseline, err := InspectCleanGitBaseline(ctx, root); !errors.Is(err, ErrGitViewUnavailable) {
			t.Fatalf("a redirected checkout was frozen as a baseline: %+v %v", baseline, err)
		}
	})
	t.Run("gitdir indirection file", func(t *testing.T) {
		root := cleanGitFixture(t)
		if err := os.WriteFile(filepath.Join(root, ".git", "gitdir"), []byte(filepath.Join(t.TempDir(), "elsewhere", ".git")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if baseline, err := InspectCleanGitBaseline(ctx, root); !errors.Is(err, ErrGitViewUnavailable) {
			t.Fatalf("a checkout with a gitdir indirection was frozen as a baseline: %+v %v", baseline, err)
		}
	})
	t.Run("a baseline frozen before this check cannot create a view", func(t *testing.T) {
		root := cleanGitFixture(t)
		other := filepath.Join(t.TempDir(), "other.git")
		baselineGit(t, root, "clone", "-q", "--bare", root, other)
		if err := os.WriteFile(filepath.Join(root, ".git", "commondir"), []byte(other+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		resolved, err := filepath.EvalSymlinks(other)
		if err != nil {
			t.Fatal(err)
		}
		physicalRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		poisoned := GitBaseline{Root: physicalRoot, CommonDir: resolved, Commit: baselineGit(t, root, "rev-parse", "HEAD")}
		if view, err := EnsureGitView(ctx, filepath.Join(t.TempDir(), "views"), "poisoned", poisoned); !errors.Is(err, ErrGitViewUnavailable) {
			t.Fatalf("a baseline whose common directory is another repository created a view: %+v %v", view, err)
		}
	})
}

// Freeze reads HEAD through the daemon-written ref directory, which repeats
// the source HEAD. Every shape Git writes must still freeze the commit the
// person's own Git reports.
func TestBaselineFreezeResolvesEveryHeadShape(t *testing.T) {
	ctx := context.Background()
	shapes := map[string]func(t *testing.T, root string){
		"branch":   func(t *testing.T, root string) {},
		"detached": func(t *testing.T, root string) { baselineGit(t, root, "checkout", "-q", "--detach") },
		"symlinked": func(t *testing.T, root string) {
			baselineGit(t, root, "-c", "core.preferSymlinkRefs=true", "checkout", "-q", "-b", "linked")
			if info, err := os.Lstat(filepath.Join(root, ".git", "HEAD")); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Skipf("this Git does not write a symlinked HEAD: %v", err)
			}
		},
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			root := cleanGitFixture(t)
			shape(t, root)
			baseline, err := InspectCleanGitBaseline(ctx, root)
			if err != nil {
				t.Fatalf("a %s HEAD did not freeze: %v", name, err)
			}
			if want := baselineGit(t, root, "rev-parse", "HEAD"); baseline.Commit != want {
				t.Fatalf("froze %s, want %s", baseline.Commit, want)
			}
		})
	}
}

// With GIT_DIR and GIT_COMMON_DIR pinned, Git still follows a planted
// commondir for ref reads (config and objects stay pinned). Ref reads go
// through the daemon-written directory, which has no commondir of its own.
func TestSourceRefReadsIgnoreAPlantedCommonDir(t *testing.T) {
	ctx := context.Background()
	f := newSourceAttackFixture(t)
	other := cleanGitFixture(t)
	if err := os.WriteFile(filepath.Join(other, "only-other.txt"), []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, other, "add", "only-other.txt")
	baselineGit(t, other, "commit", "-qm", "only in other")
	baselineGit(t, other, "update-ref", "refs/heads/selfmind/"+f.archived.ID, "HEAD")
	want := baselineGit(t, f.root, "rev-parse", "refs/heads/selfmind/"+f.archived.ID)
	refs, err := newGitSourceRefDir(ctx, f.views, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	defer refs.close()
	// Planted after the directory passed its metadata check, as a Run writing
	// the checkout could between a screen and a read.
	if err := os.WriteFile(filepath.Join(f.gitDir, "commondir"), []byte(filepath.Join(other, ".git")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := refs.resolve(ctx, "refs/heads/selfmind/"+f.archived.ID)
	if err != nil || got != want {
		t.Fatalf("ref read followed the planted commondir: got %q %v, want %s", got, err, want)
	}
}

// Git reads the repository format from the repository's own config file and
// does not follow includes there. The ref directory repeats the format as Git
// reads it, so an included extensions.refStorage cannot make the daemon write
// refs to a store the source's Git never reads.
func TestSourceRefFormatIsReadTheWayGitReadsIt(t *testing.T) {
	ctx := context.Background()
	f := newSourceAttackFixture(t)
	included := filepath.Join(t.TempDir(), "included")
	f.write(t, included, "[extensions]\n\trefStorage = reftable\n", 0o600)
	f.config(t, "core.repositoryformatversion", "1")
	f.config(t, "include.path", included)
	if err := os.Mkdir(filepath.Join(f.gitDir, "reftable"), 0o700); err != nil {
		t.Fatal(err)
	}
	delivered, err := DeliverGitViewBranch(ctx, f.views, f.pending.ID, f.baseline)
	if err != nil {
		t.Fatalf("delivery followed an included repository format: %v", err)
	}
	if got := baselineGit(t, f.root, "rev-parse", "--verify", "refs/heads/"+delivered.DeliveredBranch+"^{commit}"); got != delivered.HeadCommit {
		t.Fatalf("the source resolves the delivered branch to %s, want %s", got, delivered.HeadCommit)
	}
	if entries, err := os.ReadDir(filepath.Join(f.gitDir, "reftable")); err != nil || len(entries) != 0 {
		t.Fatalf("refs were written to a store the source does not read: %v %v", entries, err)
	}
}

// Ref storage other than files and reftable is refused before any write
// rather than guessed at. (Git 2.54 itself rejects an unknown value; one a
// later Git accepts is refused by the format check.)
func TestUnknownSourceRefStorageIsRefusedBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	f := newSourceAttackFixture(t)
	f.config(t, "core.repositoryformatversion", "1")
	f.config(t, "extensions.refStorage", "future")
	ref := "refs/heads/selfmind/future"
	if err := createGitSourceRef(ctx, f.views, f.baseline, ref, f.baseline.Commit); err == nil || !strings.Contains(err.Error(), "source repository format") {
		t.Fatalf("an unknown ref storage was not refused up front: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.gitDir, filepath.FromSlash(ref))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a loose ref was written for an unknown ref storage: %v", err)
	}
}

// Managed views do not support SHA-256 repositories yet, and freeze says so
// instead of reporting the checkout as dirty.
func TestBaselineFreezeRefusesSHA256Repositories(t *testing.T) {
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", "--object-format=sha256", root)
	cmd.Env = cleanGitProbeEnv(os.Environ())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this Git cannot create SHA-256 repositories: %v: %s", err, output)
	}
	baselineGit(t, root, "config", "user.name", "Test")
	baselineGit(t, root, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, root, "add", "file.txt")
	baselineGit(t, root, "commit", "-qm", "base")
	if baseline, err := InspectCleanGitBaseline(context.Background(), root); !errors.Is(err, ErrGitViewUnavailable) ||
		!strings.Contains(err.Error(), "SHA-256 repositories are not supported for managed views yet") {
		t.Fatalf("SHA-256 freeze = %+v, %v", baseline, err)
	}
}

// copyLooseGitObject copies one loose object between object stores, as a Run
// that can write the source and read the view could.
func copyLooseGitObject(t *testing.T, fromGitDir, toGitDir, id string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fromGitDir, "objects", id[:2], id[2:]))
	if err != nil {
		t.Fatal(err)
	}
	writeLooseGitObject(t, toGitDir, id, data)
}

func writeLooseGitObject(t *testing.T, gitDir, id string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(gitDir, "objects", id[:2]), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "objects", id[:2], id[2:]), data, 0o444); err != nil {
		t.Fatal(err)
	}
}

// removeGitPacks deletes every pack of a fixture source. Its own objects are
// loose, so exactly the objects delivery imported go missing.
func removeGitPacks(t *testing.T, gitDir string) {
	t.Helper()
	dir := filepath.Join(gitDir, "objects", "pack")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pack-") {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				t.Fatal(err)
			}
			removed++
		}
	}
	if removed == 0 {
		t.Fatal("the source holds no imported pack to remove")
	}
}

var sourceRefFormats = map[string]func(*testing.T) string{"files": cleanGitFixture, "reftable": reftableGitFixture}

// A Run that can write the source may copy just the view's commit object there
// and point the delivery branch at it. The branch then names the work while
// the source cannot rebuild it. Work counts as delivered only while the source
// holds every object it adds over the baseline; /apply supplies them from the
// view.
func TestDeliveryCountsOnlyWorkTheSourceHolds(t *testing.T) {
	for name, fixture := range sourceRefFormats {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := fixture(t)
			baseline, err := InspectCleanGitBaseline(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			views := filepath.Join(t.TempDir(), "views")
			view, err := EnsureGitView(ctx, views, "faked", baseline)
			if err != nil {
				t.Fatal(err)
			}
			head := commitInGitView(t, view, "only in the view\n")
			copyLooseGitObject(t, filepath.Join(view.Path, ".git"), filepath.Join(root, ".git"), head)
			baselineGit(t, root, "update-ref", "refs/heads/selfmind/"+view.ID, head)
			if info, err := InspectGitViewDelivery(ctx, views, view.ID, baseline); err != nil || info.DeliveredBranch != "" {
				t.Fatalf("a branch naming work the source cannot rebuild counted as delivered: %+v %v", info, err)
			}
			delivered, err := DeliverGitViewBranch(ctx, views, view.ID, baseline)
			if err != nil || delivered.DeliveredBranch != "selfmind/"+view.ID {
				t.Fatalf("delivery = %+v %v", delivered, err)
			}
			if got := baselineGit(t, root, "cat-file", "-p", head+":file.txt"); got != "only in the view" {
				t.Fatalf("delivered content in the source = %q", got)
			}
		})
	}
}

// Prune deletes the only other copy of the work, so the source must hold every
// object the work adds over the baseline, not just its commit, before the
// protected ref is created or accepted.
func TestPruneKeepsTheArchiveUntilTheSourceHoldsTheWork(t *testing.T) {
	for name, fixture := range sourceRefFormats {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := fixture(t)
			baseline, err := InspectCleanGitBaseline(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			views := filepath.Join(t.TempDir(), "views")
			view, err := EnsureGitView(ctx, views, "stripped", baseline)
			if err != nil {
				t.Fatal(err)
			}
			head := commitInGitView(t, view, "archived work\n")
			if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
				t.Fatal(err)
			}
			retired, err := RetireGitView(ctx, views, view.ID, baseline, nil)
			if err != nil {
				t.Fatal(err)
			}
			// The imported objects leave the source; the commit and the branch
			// naming it stay.
			removeGitPacks(t, filepath.Join(root, ".git"))
			copyLooseGitObject(t, filepath.Join(retired.Path, ".git"), filepath.Join(root, ".git"), head)
			if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil); err == nil || !strings.Contains(err.Error(), "does not hold") {
				t.Fatalf("prune went ahead while the source lacked the work's objects: %v", err)
			}
			if _, err := os.Stat(filepath.Join(retired.Path, "file.txt")); err != nil {
				t.Fatalf("archive was not kept: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(views, ".retired", ".pruned", view.ID+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a prune record was written for incomplete work: %v", err)
			}
			// Delivering again from the restored view completes the source,
			// after which prune may reclaim the archive.
			if _, err := RestoreGitView(ctx, views, view.ID, baseline); err != nil {
				t.Fatal(err)
			}
			if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
				t.Fatal(err)
			}
			if _, err := RetireGitView(ctx, views, view.ID, baseline, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil); err != nil {
				t.Fatalf("prune of completely delivered work: %v", err)
			}
			if got := baselineGit(t, root, "cat-file", "-p", head+":file.txt"); got != "archived work" {
				t.Fatalf("protected content in the source = %q", got)
			}
		})
	}
}

// The protected ref is created only once the source holds the work. Objects
// the source lost after the archive was inspected come from the archive.
func TestPruneProtectionImportsMissingWorkFromTheArchive(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "refilled", baseline)
	if err != nil {
		t.Fatal(err)
	}
	head := commitInGitView(t, view, "refilled work\n")
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	retired, err := RetireGitView(ctx, views, view.ID, baseline, nil)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := retiredGitViewDir(views)
	if err != nil {
		t.Fatal(err)
	}
	info, err := InspectGitViewDelivery(ctx, archive, view.ID, baseline)
	if err != nil || info.DeliveredBranch == "" {
		t.Fatalf("archived delivery = %+v %v", info, err)
	}
	removeGitPacks(t, filepath.Join(root, ".git"))
	copyLooseGitObject(t, filepath.Join(retired.Path, ".git"), filepath.Join(root, ".git"), head)
	record := PrunedGitView{Version: 1, SourceRoot: baseline.Root, BaselineCommit: baseline.Commit,
		HeadCommit: head, RetainedRef: retainedGitViewRef(view.ID)}
	if err := retainGitViewCommit(ctx, archive, baseline, record, info); err != nil {
		t.Fatal(err)
	}
	if got := baselineGit(t, root, "cat-file", "-p", head+":file.txt"); got != "refilled work" {
		t.Fatalf("protected content in the source = %q", got)
	}
}

// After prune the source is the only copy, so the pruned view is reported as
// protected only while the source still holds the work.
func TestPrunedWorkIsReportedOnlyWhileTheSourceHoldsIt(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "reported", baseline)
	if err != nil {
		t.Fatal(err)
	}
	head := commitInGitView(t, view, "pruned work\n")
	commit, err := os.ReadFile(filepath.Join(view.Path, ".git", "objects", head[:2], head[2:]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireGitView(ctx, views, view.ID, baseline, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectPrunedGitView(ctx, views, view.ID, baseline); err != nil {
		t.Fatalf("complete pruned work does not inspect: %v", err)
	}
	removeGitPacks(t, filepath.Join(root, ".git"))
	writeLooseGitObject(t, filepath.Join(root, ".git"), head, commit)
	if record, err := InspectPrunedGitView(ctx, views, view.ID, baseline); err == nil || !strings.Contains(err.Error(), "no longer complete") {
		t.Fatalf("pruned work the source cannot rebuild was reported as protected: %+v %v", record, err)
	}
}
