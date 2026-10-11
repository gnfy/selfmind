package executionenv

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// historyGitFixture is cleanGitFixture with two more commits, so the baseline
// (HEAD) has a parent and a grandparent.
func historyGitFixture(t *testing.T) string {
	t.Helper()
	root := cleanGitFixture(t)
	for _, content := range []string{"second\n", "third\n"} {
		if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		baselineGit(t, root, "commit", "-qam", strings.TrimSpace(content))
	}
	return root
}

// forgedCommit writes a commit with tree and parents into a scratch repository
// and returns that repository and the commit's real id.
func forgedCommit(t *testing.T, tree string, parents ...string) (string, string) {
	t.Helper()
	return forgedCommitAt(t, 1, tree, parents...)
}

// forgedCommitAt is forgedCommit dated at the given Unix time.
func forgedCommitAt(t *testing.T, date int64, tree string, parents ...string) (string, string) {
	t.Helper()
	repo := t.TempDir()
	baselineGit(t, repo, "init", "-q")
	var commit strings.Builder
	fmt.Fprintf(&commit, "tree %s\n", tree)
	for _, parent := range parents {
		fmt.Fprintf(&commit, "parent %s\n", parent)
	}
	fmt.Fprintf(&commit, "author Forger <forger@example.invalid> %d +0000\ncommitter Forger <forger@example.invalid> %d +0000\n\nforged\n", date, date)
	return repo, gitWithStdin(t, repo, commit.String(), "hash-object", "-t", "commit", "-w", "--stdin")
}

// forgedWork is work two commits deep on a baseline with history, delivered
// for real.
type forgedWork struct {
	root, views string
	baseline    GitBaseline
	view        GitView
	first, head string
}

// commitOldWork commits content at path dated long before the baseline, so a
// walk ordered by date reads the baseline's history before the work's.
func commitOldWork(t *testing.T, view GitView, path, content string) string {
	t.Helper()
	writeTestFile(t, filepath.Join(view.Path, filepath.FromSlash(path)), content)
	baselineGit(t, view.Path, "add", path)
	cmd := exec.Command("git", "-C", view.Path, "commit", "-qm", "work: "+path)
	cmd.Env = append(cleanGitProbeEnv(os.Environ()), "GIT_AUTHOR_DATE=2001-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2001-01-01T00:00:00Z")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	return baselineGit(t, view.Path, "rev-parse", "HEAD")
}

func deliverForgedWork(t *testing.T, viewID string) forgedWork {
	t.Helper()
	ctx := context.Background()
	root := historyGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, viewID, baseline)
	if err != nil {
		t.Fatal(err)
	}
	first := commitOldWork(t, view, "a/b.txt", "first draft\n")
	head := commitOldWork(t, view, "a/b.txt", "final work\n")
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	return forgedWork{root: root, views: views, baseline: baseline, view: view, first: first, head: head}
}

// forgedPackShapes plant, in the object store at gitDir, a newer pack that
// makes the first work commit look like part of the baseline's history: one
// whose index names the baseline itself, or one that names only the
// baseline's parent while the baseline stays genuine. Git does not hash what
// it reads from a pack, so either one steers a walk that excludes the
// baseline's history.
var forgedPackShapes = map[string]func(t *testing.T, gitDir string, w forgedWork){
	"a forged baseline": func(t *testing.T, gitDir string, w forgedWork) {
		parent := baselineGit(t, w.root, "rev-parse", w.baseline.Commit+"^")
		repo, forged := forgedCommit(t, baselineGit(t, w.root, "rev-parse", w.baseline.Commit+"^{tree}"), parent, w.first)
		plantPackedObject(t, filepath.Join(gitDir, "objects", "pack"), repo, forged, []string{w.baseline.Commit})
	},
	// Dated after the work, the forged baseline is expanded before the work
	// is, so a walk that took it would mark the first work commit as the
	// baseline's; only the hash check keeps it out.
	"a forged baseline dated after the work": func(t *testing.T, gitDir string, w forgedWork) {
		parent := baselineGit(t, w.root, "rev-parse", w.baseline.Commit+"^")
		repo, forged := forgedCommitAt(t, time.Now().Add(time.Hour).Unix(),
			baselineGit(t, w.root, "rev-parse", w.baseline.Commit+"^{tree}"), parent, w.first)
		plantPackedObject(t, filepath.Join(gitDir, "objects", "pack"), repo, forged, []string{w.baseline.Commit})
	},
	// The pack also holds a genuine copy of the baseline, so reading the
	// baseline makes it the pack Git searches first for the ancestor.
	"a genuine baseline with a forged ancestor": func(t *testing.T, gitDir string, w forgedWork) {
		ancestor := baselineGit(t, w.root, "rev-parse", w.baseline.Commit+"^")
		grandparent := baselineGit(t, w.root, "rev-parse", w.baseline.Commit+"^^")
		repo, forged := forgedCommit(t, baselineGit(t, w.root, "rev-parse", ancestor+"^{tree}"), grandparent, w.first)
		writeTestFile(t, filepath.Join(repo, ".git", "objects", "info", "alternates"), filepath.Join(gitDir, "objects")+"\n")
		plantPackedObjects(t, filepath.Join(gitDir, "objects", "pack"), repo,
			[]packedAs{{object: w.baseline.Commit, as: w.baseline.Commit}, {object: forged, as: ancestor}})
	},
}

// workContentHeld reports whether the source holds the first work commit's
// own content, which a listing steered by a forged pack leaves out.
func workContentHeld(t *testing.T, w forgedWork) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", w.root, "cat-file", "-p", w.first+":a/b.txt")
	cmd.Env = cleanGitProbeEnv(os.Environ())
	output, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(output)) == "first draft"
}

// The Run can write any object into its own view. A pack it adds there must
// not change which objects count as the work: prune keeps the archive while
// the source lacks part of the work, whatever the forged pack claims.
func TestForgedViewPacksDoNotShrinkTheWork(t *testing.T) {
	ctx := context.Background()
	for name, plant := range forgedPackShapes {
		t.Run("prune/"+name, func(t *testing.T) {
			t.Parallel()
			w := deliverForgedWork(t, "forged")
			kept := append(readWorkObjects(t, w.view.Path, w.head, w.first), readWorkObjects(t, w.view.Path, w.first, w.baseline.Commit)[0])
			plant(t, filepath.Join(w.view.Path, ".git"), w)
			retired, err := RetireGitView(ctx, w.views, w.view.ID, w.baseline, nil)
			if err != nil {
				// Refusing to archive a view whose objects do not hash keeps
				// the work in place, which is also safe.
				if _, statErr := os.Stat(filepath.Join(w.view.Path, "a", "b.txt")); statErr != nil {
					t.Fatalf("a refused archive lost the view: %v (%v)", statErr, err)
				}
				t.Logf("archive refused: %v", err)
				return
			}
			removeGitPacks(t, filepath.Join(w.root, ".git"))
			for _, object := range kept {
				writeLooseGitObject(t, filepath.Join(w.root, ".git"), object.id, object.data)
			}
			if _, err := PruneRetiredGitView(ctx, w.views, w.view.ID, w.baseline, nil); err == nil {
				t.Fatal("prune deleted the archive while the source lacked the first commit's tree")
			} else {
				t.Logf("prune refused: %v", err)
			}
			if _, err := os.Stat(filepath.Join(retired.Path, "a", "b.txt")); err != nil {
				t.Fatalf("archive was not kept: %v", err)
			}
		})
		t.Run("delivery/"+name, func(t *testing.T) {
			t.Parallel()
			root := historyGitFixture(t)
			baseline, err := InspectCleanGitBaseline(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			views := filepath.Join(t.TempDir(), "views")
			view, err := EnsureGitView(ctx, views, "forged", baseline)
			if err != nil {
				t.Fatal(err)
			}
			w := forgedWork{root: root, views: views, baseline: baseline, view: view}
			w.first = commitOldWork(t, view, "a/b.txt", "first draft\n")
			w.head = commitOldWork(t, view, "a/b.txt", "final work\n")
			plant(t, filepath.Join(view.Path, ".git"), w)
			// Either the view's own objects cannot prove the work, or the
			// whole work, the first commit's content included, is delivered.
			delivered, err := DeliverGitViewBranch(ctx, views, view.ID, baseline)
			if err == nil && (delivered.DeliveredBranch == "" || !workContentHeld(t, w)) {
				t.Fatalf("a delivery left out part of the work: %+v", delivered)
			}
			t.Logf("delivery: %q %v", delivered.DeliveredBranch, err)
		})
	}
}

// A writer of the source can plant the same packs there after prune; the
// report must not call work protected that the source cannot rebuild.
func TestForgedSourcePacksDoNotShrinkThePrunedWork(t *testing.T) {
	ctx := context.Background()
	for name, plant := range forgedPackShapes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := deliverForgedWork(t, "pruned")
			kept := append(readWorkObjects(t, w.view.Path, w.head, w.first), readWorkObjects(t, w.view.Path, w.first, w.baseline.Commit)[0])
			if _, err := RetireGitView(ctx, w.views, w.view.ID, w.baseline, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := PruneRetiredGitView(ctx, w.views, w.view.ID, w.baseline, nil); err != nil {
				t.Fatal(err)
			}
			removeGitPacks(t, filepath.Join(w.root, ".git"))
			for _, object := range kept {
				writeLooseGitObject(t, filepath.Join(w.root, ".git"), object.id, object.data)
			}
			plant(t, filepath.Join(w.root, ".git"), w)
			if record, err := InspectPrunedGitView(ctx, w.views, w.view.ID, w.baseline); err == nil {
				t.Fatalf("work the source cannot rebuild was reported as protected: %+v", record)
			} else {
				t.Logf("report refused: %v", err)
			}
		})
	}
}

// A Run may legitimately repack and collect garbage in its own view; that
// must not lose work or stop delivery and prune. A plain gc leaves out of the
// view's own store what the source holds, the baseline's history included; a
// full repack copies it back.
func TestGarbageCollectionInTheViewKeepsDeliveryAndPrune(t *testing.T) {
	ctx := context.Background()
	for name, collect := range map[string][][]string{
		"gc":                   {{"gc", "-q"}},
		"gc and a full repack": {{"gc", "-q", "--prune=now"}, {"repack", "-a", "-d", "-q"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := historyGitFixture(t)
			baseline, err := InspectCleanGitBaseline(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			views := filepath.Join(t.TempDir(), "views")
			view, err := EnsureGitView(ctx, views, "collected", baseline)
			if err != nil {
				t.Fatal(err)
			}
			first := commitPathInGitView(t, view, "a/b.txt", "first draft\n")
			head := commitPathInGitView(t, view, "a/b.txt", "final work\n")
			for _, args := range collect {
				baselineGit(t, view.Path, args...)
			}
			own, err := localGitViewObjects(ctx, view.Path)
			if err != nil {
				t.Fatal(err)
			}
			if _, kept := own[baseline.Commit]; kept == (name == "gc") {
				t.Fatalf("after %s the view's own store holds the baseline: %t", name, kept)
			}
			if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
				t.Fatalf("delivery after gc: %v", err)
			}
			if _, err := RetireGitView(ctx, views, view.ID, baseline, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil); err != nil {
				t.Fatalf("prune after gc: %v", err)
			}
			for commit, want := range map[string]string{first: "first draft", head: "final work"} {
				if got := baselineGit(t, root, "cat-file", "-p", commit+":a/b.txt"); got != want {
					t.Fatalf("protected content of %s = %q", commit, got)
				}
			}
		})
	}
}

// Each guard on what the Run writes into its view has a test that fails
// without it.
func TestViewStateGuardsAreEachNeeded(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (string, GitBaseline, string, GitView, string, string) {
		root := historyGitFixture(t)
		baseline, err := InspectCleanGitBaseline(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		views := filepath.Join(t.TempDir(), "views")
		view, err := EnsureGitView(ctx, views, "guarded", baseline)
		if err != nil {
			t.Fatal(err)
		}
		first := commitPathInGitView(t, view, "a/b.txt", "first draft\n")
		head := commitPathInGitView(t, view, "a/b.txt", "final work\n")
		return root, baseline, views, view, first, head
	}
	t.Run("a graft that adds a parent to the baseline", func(t *testing.T) {
		_, baseline, views, view, first, _ := setup(t)
		parent := baselineGit(t, view.Path, "rev-parse", baseline.Commit+"^")
		writeTestFile(t, filepath.Join(view.Path, ".git", "info", "grafts"), baseline.Commit+" "+parent+" "+first+"\n")
		if info, err := InspectGitViewDelivery(ctx, views, view.ID, baseline); err == nil {
			t.Fatalf("a view with a graft on its baseline was inspected: %+v", info)
		}
	})
	t.Run("a shallow file on the baseline's history", func(t *testing.T) {
		_, baseline, views, view, _, _ := setup(t)
		writeTestFile(t, filepath.Join(view.Path, ".git", "shallow"), baselineGit(t, view.Path, "rev-parse", baseline.Commit+"^")+"\n")
		if info, err := InspectGitViewDelivery(ctx, views, view.ID, baseline); err == nil {
			t.Fatalf("a view with a shallow file was inspected: %+v", info)
		}
	})
	t.Run("replace refs to objects outside the work", func(t *testing.T) {
		root, baseline, views, view, _, head := setup(t)
		decoyBlob := gitWithStdin(t, view.Path, "decoy\n", "hash-object", "-w", "--stdin")
		decoyTree := gitWithStdin(t, view.Path, "100644 blob "+decoyBlob+"\tb.txt\n", "mktree")
		baselineGit(t, view.Path, "replace", "-f", head+":a/b.txt", decoyBlob)
		baselineGit(t, view.Path, "replace", "-f", head+":a", decoyTree)
		if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
			t.Fatalf("delivery with replace refs in the view: %v", err)
		}
		if got := baselineGit(t, root, "cat-file", "-p", head+":a/b.txt"); got != "final work" {
			t.Fatalf("delivered content = %q", got)
		}
	})
	t.Run("a commit-graph that hides unreachable work", func(t *testing.T) {
		_, baseline, views, view, _, head := setup(t)
		// Work the Run committed and then left unreachable from HEAD.
		baselineGit(t, view.Path, "checkout", "-q", "-b", "side")
		hidden := commitPathInGitView(t, view, "side.txt", "side work\n")
		baselineGit(t, view.Path, "checkout", "-q", "--detach", head)
		baselineGit(t, view.Path, "branch", "-q", "-D", "side")
		if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
			t.Fatal(err)
		}
		gitWithStdin(t, view.Path, head+"\n"+hidden+"\n", "commit-graph", "write", "--stdin-commits")
		commitGraphEntry(t, filepath.Join(view.Path, ".git", "objects", "info", "commit-graph"), head,
			func(data []byte, entry int, position func(string) uint32) {
				binary.BigEndian.PutUint32(data[entry+24:], position(hidden))
			})
		retired, err := RetireGitView(ctx, views, view.ID, baseline, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil); err == nil || !strings.Contains(err.Error(), "unreachable") {
			t.Fatalf("prune went ahead over unreachable work a commit-graph claimed was reachable: %v", err)
		}
		if _, err := os.Stat(filepath.Join(retired.Path, ".git")); err != nil {
			t.Fatalf("archive was not kept: %v", err)
		}
	})
}

// Prune deletes an archive only when its delivered HEAD reaches every object
// the archive stores itself. A writer of the source, which the archive reads
// through its alternates, must not be able to change that answer: once a gc in
// the view has left out of its own store what the source holds, Git reads the
// baseline's history from the source, and a pack there that gives an old
// baseline commit the hidden work as another parent would make the hidden
// work count as reachable, so prune would delete its only copy.
func TestSourcePacksDoNotMakeHiddenWorkReachable(t *testing.T) {
	ctx := context.Background()
	root := historyGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "hidden", baseline)
	if err != nil {
		t.Fatal(err)
	}
	head := commitPathInGitView(t, view, "a/b.txt", "delivered work\n")
	// Work the Run committed and then left unreachable from HEAD.
	baselineGit(t, view.Path, "checkout", "-q", "-b", "side")
	hidden := commitPathInGitView(t, view, "side.txt", "side work\n")
	baselineGit(t, view.Path, "checkout", "-q", "--detach", head)
	baselineGit(t, view.Path, "branch", "-q", "-D", "side")
	baselineGit(t, view.Path, "gc", "-q")
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	retired, err := RetireGitView(ctx, views, view.ID, baseline, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := baselineGit(t, root, "rev-parse", baseline.Commit+"^")
	ancestor := baselineGit(t, root, "rev-parse", baseline.Commit+"^^")
	repo, forged := forgedCommit(t, baselineGit(t, root, "rev-parse", ancestor+"^{tree}"), hidden)
	writeTestFile(t, filepath.Join(repo, ".git", "objects", "info", "alternates"), filepath.Join(retired.Path, ".git", "objects")+"\n")
	// Genuine copies of the commits read before the ancestor keep this the
	// pack Git searches first when it reads the ancestor.
	plantPackedObjects(t, filepath.Join(root, ".git", "objects", "pack"), repo, []packedAs{
		{object: head, as: head}, {object: baseline.Commit, as: baseline.Commit},
		{object: parent, as: parent}, {object: forged, as: ancestor}})
	if walked := baselineGit(t, retired.Path, "rev-list", head); !strings.Contains(walked, hidden) {
		t.Fatalf("Git's own walk in the archive does not read the planted ancestor; the test needs it to:\n%s", walked)
	}
	if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("prune went ahead over work its delivered HEAD leaves out: %v", err)
	}
	if got := baselineGit(t, retired.Path, "cat-file", "-t", hidden); got != "commit" {
		t.Fatalf("the archive lost the hidden work: %q", got)
	}
}

// libViewAfterGC is a delivered view whose baseline has a lib directory the
// work never changed, after a gc that left the baseline's history, lib's tree
// included, out of the view's own store.
func libViewAfterGC(t *testing.T) (string, GitBaseline, string, GitView) {
	t.Helper()
	ctx := context.Background()
	root := historyGitFixture(t)
	writeTestFile(t, filepath.Join(root, "lib", "x.txt"), "baseline lib\n")
	baselineGit(t, root, "add", "lib/x.txt")
	baselineGit(t, root, "commit", "-qm", "lib")
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "staged", baseline)
	if err != nil {
		t.Fatal(err)
	}
	commitPathInGitView(t, view, "a/b.txt", "delivered work\n")
	baselineGit(t, view.Path, "gc", "-q")
	own, err := localGitViewObjects(ctx, view.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := own[baselineGit(t, root, "rev-parse", baseline.Commit+":lib")]; kept {
		t.Fatal("gc kept lib's tree in the view's own store; the test needs Git to read it from the source")
	}
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	return root, baseline, views, view
}

// The index check refuses no clean view: after a change is staged and
// unstaged, the index's cached trees no longer vouch for lib, and status reads
// lib's tree from the source, yet the index still records HEAD's tree.
func TestIndexCheckPassesACleanViewWithStaleCachedTrees(t *testing.T) {
	ctx := context.Background()
	_, baseline, views, view := libViewAfterGC(t)
	writeTestFile(t, filepath.Join(view.Path, "lib", "x.txt"), "staged, then dropped\n")
	baselineGit(t, view.Path, "add", "lib/x.txt")
	baselineGit(t, view.Path, "reset", "-q", "--", "lib/x.txt")
	baselineGit(t, view.Path, "checkout", "--", "lib/x.txt")
	// The dropped content stays behind as an unreachable blob, which prune
	// would rightly keep the archive for.
	baselineGit(t, view.Path, "prune", "--expire=now")
	if _, err := RetireGitView(ctx, views, view.ID, baseline, nil); err != nil {
		t.Fatalf("archive of a clean view: %v", err)
	}
	if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil); err != nil {
		t.Fatalf("prune of a clean view: %v", err)
	}
}

// Archive and prune take a view only while its index holds nothing HEAD does
// not. Git status compares the index with HEAD's trees, and once a gc in the
// view has left a baseline tree out of its own store, it reads that tree from
// the source: a writer of the source must not be able to hide a staged change
// with a tree there that lists the staged content.
func TestSourcePacksDoNotHideStagedWork(t *testing.T) {
	ctx := context.Background()
	root, baseline, views, view := libViewAfterGC(t)
	writeTestFile(t, filepath.Join(view.Path, "lib", "x.txt"), "staged only\n")
	baselineGit(t, view.Path, "add", "lib/x.txt")
	repo := t.TempDir()
	baselineGit(t, repo, "init", "-q")
	writeTestFile(t, filepath.Join(repo, ".git", "objects", "info", "alternates"), filepath.Join(view.Path, ".git", "objects")+"\n")
	listing := gitWithStdin(t, repo, "100644 blob "+baselineGit(t, view.Path, "rev-parse", ":lib/x.txt")+"\tx.txt\n", "mktree")
	plantPackedObject(t, filepath.Join(root, ".git", "objects", "pack"), repo, listing,
		[]string{baselineGit(t, root, "rev-parse", baseline.Commit+":lib")})
	if status := baselineGit(t, view.Path, "status", "--porcelain"); status != "" {
		t.Fatalf("Git's own status in the view still shows the staged change; the test needs the planted tree to hide it:\n%s", status)
	}
	if info, err := InspectGitViewDelivery(ctx, views, view.ID, baseline); err == nil && info.Uncommitted == 0 {
		t.Fatalf("the staged change was not reported: %+v", info)
	} else {
		t.Logf("inspection: %+v %v", info, err)
	}
	if _, err := RetireGitView(ctx, views, view.ID, baseline, nil); err == nil {
		t.Fatal("archive took a view whose index holds a staged change")
	}
	if got := baselineGit(t, view.Path, "show", ":lib/x.txt"); got != "staged only" {
		t.Fatalf("the view lost the staged change: %q", got)
	}
}

// checkoutViewAfterGC is a delivered view whose baseline holds a symlink and
// a text file the work never changed, with attributes when given, after a gc
// that left their blobs out of the view's own store, so Git reads them from
// the source.
func checkoutViewAfterGC(t *testing.T, attributes string) (string, GitBaseline, string, GitView) {
	t.Helper()
	ctx := context.Background()
	root := historyGitFixture(t)
	if err := os.Symlink("target-a", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "text.txt"), "text one\r\nline two\r\n")
	add := []string{"add", "link", "text.txt"}
	if attributes != "" {
		writeTestFile(t, filepath.Join(root, ".gitattributes"), attributes)
		add = append(add, ".gitattributes")
	}
	baselineGit(t, root, add...)
	baselineGit(t, root, "commit", "-qm", "link and text")
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "checkout", baseline)
	if err != nil {
		t.Fatal(err)
	}
	commitPathInGitView(t, view, "a/b.txt", "delivered work\n")
	baselineGit(t, view.Path, "gc", "-q")
	own, err := localGitViewObjects(ctx, view.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"link", "text.txt"} {
		if _, kept := own[baselineGit(t, root, "rev-parse", baseline.Commit+":"+path)]; kept {
			t.Fatalf("gc kept the blob of %s in the view's own store; the test needs Git to read it from the source", path)
		}
	}
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	return root, baseline, views, view
}

// checkoutEdit changes one baseline entry of a checkout right after its index
// was written. planted is the content that makes Git read the changed entry as
// unchanged, which a writer of the source plants under the entry's blob.
type checkoutEdit struct {
	path, mode, planted string
	attributes          string
	// statless clears the entry's stat data in the index first, as read-tree
	// does, and as Git does to an entry it writes in the same second the
	// entry last changed: such an entry is settled by its content.
	statless bool
	// exposed says whether Git's own status reads the planted blob for it.
	exposed bool
	edit    func(t *testing.T, path string)
}

var checkoutEdits = map[string]checkoutEdit{
	"a symlink re-pointed at a target of the same length": {path: "link", mode: "120000",
		planted: "target-b", exposed: true,
		edit: func(t *testing.T, path string) { repointLink(t, path, "target-b") }},
	"a symlink whose index entry has no stat data, re-pointed": {path: "link", mode: "120000",
		planted: "a/longer/target", statless: true, exposed: true,
		edit: func(t *testing.T, path string) { repointLink(t, path, "a/longer/target") }},
	"a text file rewritten at the same size": {path: "text.txt", mode: "100644",
		planted: "text ONE\r\nline two\r\n",
		edit:    func(t *testing.T, path string) { writeTestFile(t, path, "text ONE\r\nline two\r\n") }},
	"a text file whose index entry has no stat data, rewritten": {path: "text.txt", mode: "100644",
		planted: "other text\n", statless: true,
		edit: func(t *testing.T, path string) { writeTestFile(t, path, "other text\n") }},
	// Git reads the blob of a file under text=auto to see whether it was
	// committed with CRLF; a planted blob without CRLF turns normalization
	// on, which cannot make other content match.
	"a text file under text=auto, rewritten at the same size": {path: "text.txt", mode: "100644",
		planted: "text ONE\nline two\n", attributes: "*.txt text=auto\n",
		edit: func(t *testing.T, path string) { writeTestFile(t, path, "text ONE\r\nline two\r\n") }},
}

func repointLink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// apply makes the edit in the checkout at dir, whose source is root, and
// plants in the source a pack whose index names the entry's blob with the
// content Git would then read as unchanged.
func (e checkoutEdit) apply(t *testing.T, root, dir string) {
	t.Helper()
	baselineGit(t, dir, "update-index", "-q", "--refresh")
	blob := baselineGit(t, dir, "rev-parse", ":"+e.path)
	if e.statless {
		baselineGit(t, dir, "update-index", "--cacheinfo", e.mode+","+blob+","+e.path)
	}
	e.edit(t, filepath.Join(dir, e.path))
	repo := t.TempDir()
	baselineGit(t, repo, "init", "-q")
	plantPackedObject(t, filepath.Join(root, ".git", "objects", "pack"), repo,
		gitWithStdin(t, repo, e.planted, "hash-object", "-w", "--stdin"), []string{blob})
}

// A changed entry of the checkout stays uncommitted work however Git settles
// it. Git compares a symlink it cannot settle by its size and timestamps with
// its blob, read without hashing and, once a gc in the view has left that
// blob out of its own store, from the source, where a writer could plant the
// new target under the old blob's id. A regular file is compared by hashing
// the file, so a planted blob cannot hide its change; those cases hold that.
func TestPlantedBlobsDoNotHideCheckoutChanges(t *testing.T) {
	ctx := context.Background()
	for name, edit := range checkoutEdits {
		t.Run("archive/"+name, func(t *testing.T) {
			t.Parallel()
			root, baseline, views, view := checkoutViewAfterGC(t, edit.attributes)
			edit.apply(t, root, view.Path)
			if status := baselineGit(t, view.Path, "status", "--porcelain"); (status == "") != edit.exposed {
				t.Fatalf("Git's own status with the planted blob: %q; the test expects exposed=%t", status, edit.exposed)
			}
			if info, err := InspectGitViewDelivery(ctx, views, view.ID, baseline); err == nil && info.Uncommitted == 0 {
				t.Fatalf("the change to %s was not reported: %+v", edit.path, info)
			} else {
				t.Logf("inspection: uncommitted=%d %v", info.Uncommitted, err)
			}
			if _, err := RetireGitView(ctx, views, view.ID, baseline, nil); err == nil {
				t.Fatalf("archive took a view whose %s changed", edit.path)
			}
		})
	}
	t.Run("an unchanged checkout is archived and pruned", func(t *testing.T) {
		_, baseline, views, view := checkoutViewAfterGC(t, "")
		baselineGit(t, view.Path, "update-index", "--cacheinfo", "120000,"+baselineGit(t, view.Path, "rev-parse", ":link")+",link")
		if _, err := RetireGitView(ctx, views, view.ID, baseline, nil); err != nil {
			t.Fatalf("archive: %v", err)
		}
		if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil); err != nil {
			t.Fatalf("prune: %v", err)
		}
	})
	t.Run("prune/a symlink whose index entry has no stat data, re-pointed", func(t *testing.T) {
		edit := checkoutEdits["a symlink whose index entry has no stat data, re-pointed"]
		root, baseline, views, view := checkoutViewAfterGC(t, "")
		retired, err := RetireGitView(ctx, views, view.ID, baseline, nil)
		if err != nil {
			t.Fatal(err)
		}
		edit.apply(t, root, retired.Path)
		if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline, nil); err == nil {
			t.Fatal("prune deleted an archive whose symlink changed")
		} else {
			t.Logf("prune refused: %v", err)
		}
		if target, err := os.Readlink(filepath.Join(retired.Path, "link")); err != nil || target != "a/longer/target" {
			t.Fatalf("the archive lost the changed symlink: %q %v", target, err)
		}
	})
}

// looseGitObject is the file Git stores for an object of kind with content.
func looseGitObject(t *testing.T, kind string, content []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := zlib.NewWriter(&out)
	fmt.Fprintf(writer, "%s %d\x00", kind, len(content))
	writer.Write(content)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// The walk's reader neither reads a blob nor waits on an object it will not
// read. Git announces a blob's size from one copy and may stream another, so
// a pack the Run plants beside a loose copy under one id could otherwise
// leave the walk waiting until its deadline, and a planted object larger than
// a walk reads could stall the reader's close; both read as an interruption
// instead of what they are.
func TestWalkDoesNotWaitOnPlantedObjects(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, gitDir, tree string){
		"a packed blob under a tree id, beside a shorter loose blob": func(t *testing.T, gitDir, tree string) {
			repo := t.TempDir()
			baselineGit(t, repo, "init", "-q")
			long := gitWithStdin(t, repo, "a longer planted blob\n", "hash-object", "-w", "--stdin")
			plantPackedObject(t, filepath.Join(gitDir, "objects", "pack"), repo, long, []string{tree})
			if err := os.Remove(filepath.Join(gitDir, "objects", tree[:2], tree[2:])); err != nil {
				t.Fatal(err)
			}
			writeLooseGitObject(t, gitDir, tree, looseGitObject(t, "blob", []byte("short\n")))
		},
		"an object larger than a walk reads under a tree id": func(t *testing.T, gitDir, tree string) {
			repo := t.TempDir()
			baselineGit(t, repo, "init", "-q")
			huge := gitWithStdin(t, repo, strings.Repeat("\x00", maxGitWalkObject+1), "hash-object", "-t", "tree", "--literally", "-w", "--stdin")
			plantPackedObject(t, filepath.Join(gitDir, "objects", "pack"), repo, huge, []string{tree})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := cleanGitFixture(t)
			baseline, err := InspectCleanGitBaseline(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			view, err := EnsureGitView(context.Background(), filepath.Join(t.TempDir(), "views"), "stalled", baseline)
			if err != nil {
				t.Fatal(err)
			}
			head := commitPathInGitView(t, view, "a/b.txt", "work\n")
			plant(t, filepath.Join(view.Path, ".git"), baselineGit(t, view.Path, "rev-parse", head+":a"))
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			start := time.Now()
			_, err = listGitViewWork(ctx, view.Path, head, baseline.Commit)
			if took := time.Since(start); err == nil || gitInterrupted(err) || took > 20*time.Second {
				t.Fatalf("the walk over a planted object = %v after %s", err, took)
			}
			t.Logf("walk refused: %v", err)
		})
	}
}

// A substitute the source holds next to the genuine copy, older so that Git
// resolves the genuine one today, still corrupts the work for a later repack.
// Every copy of the work's objects is checked, not only the one Git resolves.
func TestEveryCopyOfTheWorkIsChecked(t *testing.T) {
	ctx := context.Background()
	f := newSourceAttackFixture(t)
	info, err := InspectGitViewDelivery(ctx, f.views, f.pending.ID, f.baseline)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DeliverGitViewBranch(ctx, f.views, f.pending.ID, f.baseline); err != nil {
		t.Fatal(err)
	}
	blob := baselineGit(t, f.pending.Path, "rev-parse", info.HeadCommit+":file.txt")
	other := baselineGit(t, f.root, "rev-parse", f.baseline.Commit+":file.txt")
	substitute := plantPackedObject(t, filepath.Join(f.gitDir, "objects", "pack"), f.root, other, []string{blob})
	past := time.Now().Add(-48 * time.Hour)
	for _, ext := range []string{".pack", ".idx"} {
		if err := os.Chtimes(substitute+ext, past, past); err != nil {
			t.Fatal(err)
		}
	}
	if got := baselineGit(t, f.root, "cat-file", "-p", blob); got != "pending work" {
		t.Fatalf("Git resolves the substitute already (%q); the test needs the genuine copy first", got)
	}
	if again, err := InspectGitViewDelivery(ctx, f.views, f.pending.ID, f.baseline); err == nil && again.DeliveredBranch != "" {
		t.Fatal("work counted as delivered while the source also held a substitute for one of its objects")
	}
	if delivered, err := DeliverGitViewBranch(ctx, f.views, f.pending.ID, f.baseline); err == nil {
		t.Fatalf("/apply passed over a substitute the source holds: %+v", delivered)
	}
	if _, err := PruneRetiredGitView(ctx, f.views, f.archived.ID, f.baseline, nil); err != nil {
		t.Fatalf("an unrelated archive could not be pruned: %v", err)
	}
}

// forgeMultiPackIndexOffset rewrites the multi-pack index in packDir so that id
// resolves to the pack offset of as, as a writer of the source could, keeping
// every pack and pack index genuine and the index's checksum valid.
func forgeMultiPackIndexOffset(t *testing.T, packDir, id, as string) {
	t.Helper()
	path := filepath.Join(packDir, "multi-pack-index")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:4]) != "MIDX" || data[5] != 1 {
		t.Fatalf("unexpected multi-pack index header %x", data[:8])
	}
	chunks := map[string]int{}
	for i := 0; i < int(data[6]); i++ {
		entry := data[12+i*12:]
		chunks[string(entry[:4])] = int(binary.BigEndian.Uint64(entry[4:12]))
	}
	const hashLen = 20
	count := int(binary.BigEndian.Uint32(data[chunks["OIDF"]+255*4:]))
	position := func(object string) int {
		for i := 0; i < count; i++ {
			if hex.EncodeToString(data[chunks["OIDL"]+i*hashLen:chunks["OIDL"]+(i+1)*hashLen]) == object {
				return i
			}
		}
		t.Fatalf("object %s is not in the multi-pack index", object)
		return 0
	}
	from, to := chunks["OOFF"]+position(as)*8, chunks["OOFF"]+position(id)*8
	copy(data[to:to+8], data[from:from+8])
	sum := sha1.Sum(data[:len(data)-hashLen])
	copy(data[len(data)-hashLen:], sum[:])
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o444); err != nil {
		t.Fatal(err)
	}
}

// Git reads a source with a multi-pack index through that index by default,
// so its choice of copy is checked too: with every pack and pack index
// genuine, a forged index offset that makes the person's own Git read a tree
// under a blob of the work is not delivered work.
func TestEveryCopyCheckReadsTheMultiPackIndex(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "midx", baseline)
	if err != nil {
		t.Fatal(err)
	}
	head := commitPathInGitView(t, view, "a/b.txt", "work\n")
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	blob := baselineGit(t, root, "rev-parse", head+":a/b.txt")
	baselineGit(t, root, "multi-pack-index", "write")
	forgeMultiPackIndexOffset(t, filepath.Join(root, ".git", "objects", "pack"), blob, baselineGit(t, root, "rev-parse", head+":a"))
	if kind := baselineGit(t, root, "cat-file", "-t", blob); kind != "tree" {
		t.Fatalf("the person's Git reads a %s under the blob; the test needs the forged index to give it a tree", kind)
	}
	if info, err := InspectGitViewDelivery(ctx, views, view.ID, baseline); err == nil && info.DeliveredBranch != "" {
		t.Fatalf("work counted as delivered while the source's multi-pack index gives a blob of it other content: %+v", info)
	}
	if delivered, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err == nil {
		t.Fatalf("/apply passed over a forged multi-pack index: %+v", delivered)
	}
}

// A ref read by the source's own Git that fails for another reason than an
// absent ref is reported, not taken for "not delivered".
func TestSourceRefReadWithTheSourcesOwnGitKeepsItsFailure(t *testing.T) {
	ctx := context.Background()
	w := deliverForgedWork(t, "own")
	gitStandIn(t, fmt.Sprintf(`[ "$GIT_DIR" = '%s' ] && { echo 'fatal: simulated source failure' >&2; exit 128; }`, w.baseline.CommonDir), "rev-parse")
	if info, err := InspectGitViewDelivery(ctx, w.views, w.view.ID, w.baseline); err == nil || errors.Is(err, errGitSourceRefOther) ||
		!strings.Contains(err.Error(), "simulated source failure") {
		t.Fatalf("a failed read with the source's own Git = %+v %v", info, err)
	}
}

// An interrupted check of a source ref is no evidence that the ref changed:
// the interruption is reported as such, never as a different ref.
func TestSourceRefChecksKeepFailureMeaning(t *testing.T) {
	ctx := context.Background()
	w := deliverForgedWork(t, "checked")
	if _, err := RetireGitView(ctx, w.views, w.view.ID, w.baseline, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneRetiredGitView(ctx, w.views, w.view.ID, w.baseline, nil); err != nil {
		t.Fatal(err)
	}
	refs, err := newGitSourceRefDir(ctx, w.views, w.baseline)
	if err != nil {
		t.Fatal(err)
	}
	defer refs.close()
	stuck, startedFile := stuckGit(t, "rev-parse")
	if err := refs.verify(stuck, retainedGitViewRef(w.view.ID), w.head); !errors.Is(err, context.Canceled) {
		t.Fatalf("an interrupted ref check = %v", err)
	}
	if _, statErr := os.Stat(startedFile); statErr != nil {
		t.Fatalf("the stuck rev-parse never started: %v", statErr)
	}
	stuck, _ = stuckGit(t, "rev-parse")
	if record, err := InspectPrunedGitView(stuck, w.views, w.view.ID, w.baseline); !errors.Is(err, context.Canceled) ||
		strings.Contains(err.Error(), "changed") {
		t.Fatalf("an interrupted pruned-view report = %+v %v", record, err)
	}
}
