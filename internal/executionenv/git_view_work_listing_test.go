package executionenv

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Where the listing of a work's objects comes from. Delivery, archive, and
// prune list the objects a work adds over the baseline with the daemon's own
// walk over the view or its archive (git_view_work_walk.go): each commit and
// tree it reads must hash to its id, and a commit counts as the baseline's only
// when a verified chain of parents leads to it from the baseline. Grafts,
// shallow files, replace refs, commit-graphs, and refs other than HEAD play no
// part; grafts, shallow files, and any configuration key outside the passive
// allowlist also make the view unavailable. Neither the view's own store nor
// the source it reads through its alternates is trusted: a substitute in
// either fails a hash, in the walk or in the proof, instead of changing what
// passes. The tests below hold each part of that.

// deliveredWork is one view whose work at a/b.txt, two commits deep, was
// delivered for real.
type deliveredWork struct {
	root, views string
	baseline    GitBaseline
	view        GitView
	first, head string
}

func deliverWork(t *testing.T, viewID, content string) deliveredWork {
	t.Helper()
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, viewID, baseline)
	if err != nil {
		t.Fatal(err)
	}
	first := commitPathInGitView(t, view, "a/b.txt", "first draft\n")
	head := commitPathInGitView(t, view, "a/b.txt", content)
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	return deliveredWork{root: root, views: views, baseline: baseline, view: view, first: first, head: head}
}

// historyKept is what the source keeps of the work in stripWorkHistory: what
// the last commit adds over the first, and the first commit object itself.
func historyKept(t *testing.T, w deliveredWork) []workObject {
	t.Helper()
	return append(readWorkObjects(t, w.view.Path, w.head, w.first), readWorkObjects(t, w.view.Path, w.first, w.baseline.Commit)[0])
}

// stripWorkHistory leaves the source holding only objects: the branch still
// names the work, and every link Git checks while importing it resolves, but
// the first commit's tree and blob are gone.
func stripWorkHistory(t *testing.T, w deliveredWork, objects []workObject) {
	t.Helper()
	removeGitPacks(t, filepath.Join(w.root, ".git"))
	for _, object := range objects {
		writeLooseGitObject(t, filepath.Join(w.root, ".git"), object.id, object.data)
	}
}

// commitGraphEntry finds commit's entry in the commit-graph at path and lets
// edit change it in place, as a writer of that repository could. The graph's
// own checksum is kept valid.
func commitGraphEntry(t *testing.T, path, commit string, edit func(data []byte, entry int, position func(id string) uint32)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const hashLen = 20
	chunks := map[string]int{}
	for i := 0; i < int(data[6]); i++ {
		entry := 8 + 12*i
		chunks[string(data[entry:entry+4])] = int(binary.BigEndian.Uint64(data[entry+4 : entry+12]))
	}
	count := int(binary.BigEndian.Uint32(data[chunks["OIDF"]+255*4:]))
	position := func(id string) uint32 {
		raw, _ := hex.DecodeString(id)
		for i := 0; i < count; i++ {
			if bytes.Equal(data[chunks["OIDL"]+i*hashLen:chunks["OIDL"]+(i+1)*hashLen], raw) {
				return uint32(i)
			}
		}
		t.Fatalf("commit %s is not in the graph", id)
		return 0
	}
	edit(data, chunks["CDAT"]+int(position(commit))*(hashLen+16), position)
	sum := sha1.Sum(data[:len(data)-hashLen])
	copy(data[len(data)-hashLen:], sum[:])
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// forgeCommitGraph writes a commit-graph for the history of tip in the
// repository at dir, in which commit's first parent is parent and, unless tree
// is empty, its tree is tree. Git takes a commit's parents and tree from the
// graph instead of the commit object unless commit-graphs are switched off.
func forgeCommitGraph(t *testing.T, dir, tip, commit, parent, tree string) {
	t.Helper()
	gitWithStdin(t, dir, tip+"\n", "commit-graph", "write", "--stdin-commits")
	commitGraphEntry(t, filepath.Join(dir, ".git", "objects", "info", "commit-graph"), commit,
		func(data []byte, entry int, position func(string) uint32) {
			binary.BigEndian.PutUint32(data[entry+20:], position(parent))
			if tree != "" {
				raw, _ := hex.DecodeString(tree)
				copy(data[entry:], raw)
			}
		})
}

// A commit-graph records each commit's parents and tree apart from the commit
// object. One forged by the Run in its view, or by a writer of the source
// (which the view also reads, through its alternates), must not shrink the
// listing to the objects the source still holds.
func TestForgedCommitGraphsDoNotShrinkTheWork(t *testing.T) {
	ctx := context.Background()
	pruneMustKeep := func(t *testing.T, w deliveredWork, retired GitView) {
		t.Helper()
		if _, err := PruneRetiredGitView(ctx, w.views, w.view.ID, w.baseline, nil); err == nil {
			t.Fatal("prune deleted the archive of work whose history the source lacks")
		}
		if _, err := os.Stat(filepath.Join(retired.Path, "a", "b.txt")); err != nil {
			t.Fatalf("archive was not kept: %v", err)
		}
	}
	// The forged graphs below make the first work commit look like an ancestor
	// of the baseline, so a listing that trusts them leaves out that commit's
	// tree and blob while every view object still looks reachable from HEAD.
	t.Run("in the view, written by its Run", func(t *testing.T) {
		w := deliverWork(t, "graphed", "graphed work\n")
		kept := historyKept(t, w)
		forgeCommitGraph(t, w.view.Path, w.head, w.baseline.Commit, w.first, "")
		retired, err := RetireGitView(ctx, w.views, w.view.ID, w.baseline, nil)
		if err != nil {
			t.Fatal(err)
		}
		stripWorkHistory(t, w, kept)
		pruneMustKeep(t, w, retired)
	})
	t.Run("in the source, read through the view's alternates", func(t *testing.T) {
		w := deliverWork(t, "borrowed", "borrowed work\n")
		kept := historyKept(t, w)
		retired, err := RetireGitView(ctx, w.views, w.view.ID, w.baseline, nil)
		if err != nil {
			t.Fatal(err)
		}
		forgeCommitGraph(t, w.root, w.head, w.baseline.Commit, w.first, "")
		stripWorkHistory(t, w, kept)
		pruneMustKeep(t, w, retired)
	})
	t.Run("in the source, after prune", func(t *testing.T) {
		w := deliverWork(t, "pruned", "pruned work\n")
		head := historyKept(t, w)[0]
		if _, err := RetireGitView(ctx, w.views, w.view.ID, w.baseline, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := PruneRetiredGitView(ctx, w.views, w.view.ID, w.baseline, nil); err != nil {
			t.Fatal(err)
		}
		forgeCommitGraph(t, w.root, w.head, w.head, w.baseline.Commit, baselineGit(t, w.root, "rev-parse", w.baseline.Commit+"^{tree}"))
		stripWorkHistory(t, w, []workObject{head})
		if record, err := InspectPrunedGitView(ctx, w.views, w.view.ID, w.baseline); err == nil || !strings.Contains(err.Error(), "no longer complete") {
			t.Fatalf("pruned work the source cannot rebuild was reported as protected: %+v %v", record, err)
		}
	})
	t.Run("genuine graphs and a multi-pack-index change nothing", func(t *testing.T) {
		w := deliverWork(t, "indexed", "indexed work\n")
		gitWithStdin(t, w.view.Path, w.head+"\n", "commit-graph", "write", "--stdin-commits")
		gitWithStdin(t, w.root, w.head+"\n", "commit-graph", "write", "--stdin-commits")
		baselineGit(t, w.root, "multi-pack-index", "write")
		if _, err := RetireGitView(ctx, w.views, w.view.ID, w.baseline, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := PruneRetiredGitView(ctx, w.views, w.view.ID, w.baseline, nil); err != nil {
			t.Fatalf("prune with genuine graphs and a multi-pack-index: %v", err)
		}
		if got := baselineGit(t, w.root, "cat-file", "-p", w.head+":a/b.txt"); got != "indexed work" {
			t.Fatalf("protected content = %q", got)
		}
	})
}

// The work is the view's HEAD and what a verified walk from it reaches. Refs
// and replace refs the Run writes do not change what is delivered, and view
// state that would change Git's own reading makes the view unavailable.
func TestWorkListingIgnoresRunWrittenViewState(t *testing.T) {
	ctx := context.Background()
	t.Run("refs and replace refs", func(t *testing.T) {
		root := cleanGitFixture(t)
		baseline, err := InspectCleanGitBaseline(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		views := filepath.Join(t.TempDir(), "views")
		view, err := EnsureGitView(ctx, views, "listed", baseline)
		if err != nil {
			t.Fatal(err)
		}
		decoy := commitPathInGitView(t, view, "a/b.txt", "decoy\n")
		baselineGit(t, view.Path, "update-ref", "refs/heads/decoy", decoy)
		head := commitPathInGitView(t, view, "a/b.txt", "real work\n")
		for _, object := range []string{":a/b.txt", ":a", "^{tree}"} {
			baselineGit(t, view.Path, "replace", "-f", head+object, decoy+object)
		}
		if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
			t.Fatal(err)
		}
		if got := baselineGit(t, root, "cat-file", "-p", head+":a/b.txt"); got != "real work" {
			t.Fatalf("delivered content = %q", got)
		}
	})
	for name, plant := range map[string]func(t *testing.T, view GitView, head string){
		"grafts": func(t *testing.T, view GitView, head string) {
			writeTestFile(t, filepath.Join(view.Path, ".git", "info", "grafts"), head+"\n")
		},
		"shallow": func(t *testing.T, view GitView, head string) {
			writeTestFile(t, filepath.Join(view.Path, ".git", "shallow"), head+"\n")
		},
		"commit-graphs switched on in the view config": func(t *testing.T, view GitView, head string) {
			baselineGit(t, view.Path, "config", "core.commitGraph", "true")
		},
		"a repository format change in the view config": func(t *testing.T, view GitView, head string) {
			baselineGit(t, view.Path, "config", "core.repositoryformatversion", "1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := cleanGitFixture(t)
			baseline, err := InspectCleanGitBaseline(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			views := filepath.Join(t.TempDir(), "views")
			view, err := EnsureGitView(ctx, views, "altered", baseline)
			if err != nil {
				t.Fatal(err)
			}
			plant(t, view, commitPathInGitView(t, view, "a/b.txt", "altered work\n"))
			if delivered, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err == nil || delivered.DeliveredBranch != "" {
				t.Fatalf("a view with %s was delivered: %+v %v", name, delivered, err)
			}
		})
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Objects the work shares with the source and the view never stored itself
// are read from the source through the view's alternates. Each is hashed by
// the walk or named by the listing and hashed by the proof, so a substitute
// the source holds for one fails.
func TestWorkListingCatchesSubstitutesTheViewBorrows(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	// The source already holds, outside the baseline's history, a directory
	// the work will add.
	blob := gitWithStdin(t, root, "shared\n", "hash-object", "-w", "--stdin")
	shared := gitWithStdin(t, root, "100644 blob "+blob+"\tdata.txt\n", "mktree")
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "borrowing", baseline)
	if err != nil {
		t.Fatal(err)
	}
	commitPathInGitView(t, view, "shared/data.txt", "shared\n")
	if _, err := os.Stat(filepath.Join(view.Path, ".git", "objects", shared[:2], shared[2:])); !errors.Is(err, os.ErrNotExist) {
		t.Skipf("this Git stored the shared tree in the view, so nothing is borrowed: %v", err)
	}
	// A writer of the source puts another tree under the shared tree's id.
	path := filepath.Join(root, ".git", "objects", shared[:2], shared[2:])
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	other := looseObjectData(t, root, baselineGit(t, root, "rev-parse", "HEAD^{tree}"))
	if err := os.WriteFile(path, other, 0o444); err != nil {
		t.Fatal(err)
	}
	if delivered, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err == nil || delivered.DeliveredBranch != "" {
		t.Fatalf("a substitute for a borrowed tree passed for the work: %+v %v", delivered, err)
	}
}
