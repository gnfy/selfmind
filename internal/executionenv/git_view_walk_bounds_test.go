package executionenv

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// rawGitStore writes objects straight into a repository's loose store, so a
// test can build histories no Git command would, quickly.
type rawGitStore struct {
	t   *testing.T
	dir string
}

func newRawGitStore(t *testing.T) rawGitStore {
	t.Helper()
	dir := t.TempDir()
	baselineGit(t, dir, "init", "-q")
	return rawGitStore{t: t, dir: dir}
}

func (s rawGitStore) write(kind string, content []byte) string {
	s.t.Helper()
	header := fmt.Sprintf("%s %d\x00", kind, len(content))
	digest := sha1.New()
	digest.Write([]byte(header))
	digest.Write(content)
	id := hex.EncodeToString(digest.Sum(nil))
	path := filepath.Join(s.dir, ".git", "objects", id[:2], id[2:])
	if _, err := os.Stat(path); err == nil {
		return id
	}
	var out bytes.Buffer
	writer, err := zlib.NewWriterLevel(&out, zlib.BestSpeed)
	if err != nil {
		s.t.Fatal(err)
	}
	writer.Write([]byte(header))
	writer.Write(content)
	if err := writer.Close(); err != nil {
		s.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o444); err != nil {
		s.t.Fatal(err)
	}
	return id
}

// tree writes a tree of the given raw entries, which must be in Git's order.
func (s rawGitStore) tree(entries ...string) string {
	return s.write("tree", []byte(strings.Join(entries, "")))
}

func (s rawGitStore) commit(tree string, date int64, message string, parents ...string) string {
	var text strings.Builder
	fmt.Fprintf(&text, "tree %s\n", tree)
	for _, parent := range parents {
		fmt.Fprintf(&text, "parent %s\n", parent)
	}
	fmt.Fprintf(&text, "author A <a@example.invalid> %d +0000\ncommitter A <a@example.invalid> %d +0000\n\n%s", date, date, message)
	return s.write("commit", []byte(text.String()))
}

// fileTree is a tree of n files with made-up blob ids, which the walk lists
// without reading.
func (s rawGitStore) fileTree(n int, salt string) string {
	var content bytes.Buffer
	for i := 0; i < n; i++ {
		blob := sha1.Sum([]byte(fmt.Sprintf("%s %d", salt, i)))
		fmt.Fprintf(&content, "100644 f%07d\x00", i)
		content.Write(blob[:])
	}
	return s.write("tree", content.Bytes())
}

// measuredWalk runs the walk from head over base in the store, and reports
// how long it took and how far the heap grew while it ran.
func (s rawGitStore) measuredWalk(head, base string) ([]string, error, time.Duration, uint64) {
	s.t.Helper()
	reader, err := newGitObjectReader(context.Background(), s.dir, gitViewEnv(s.dir), errGitViewIncomplete, errGitViewCorrupt)
	if err != nil {
		s.t.Fatal(err)
	}
	defer reader.close()
	var ids []string
	took, peak := measureHeap(func() { ids, err = listGitWorkObjects(reader, head, base) })
	return ids, err, took, peak
}

// measureHeap runs work and returns its duration and the peak growth of the
// live heap over what it was before.
func measureHeap(work func()) (time.Duration, uint64) {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	before, peak := stats.HeapAlloc, stats.HeapAlloc
	done := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		var sample runtime.MemStats
		for {
			runtime.ReadMemStats(&sample)
			if sample.HeapAlloc > peak {
				peak = sample.HeapAlloc
			}
			select {
			case <-done:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	start := time.Now()
	work()
	took := time.Since(start)
	close(done)
	wait.Wait()
	if peak < before {
		return took, 0
	}
	return took, peak - before
}

const mib = 1 << 20

// A commit names at most 1024 parents, far above any octopus merge Git
// writes; each parent and each parent tree is read and held once. A commit
// that names one parent thousands of times, or more distinct parents than
// that, is refused before any of them is read.
func TestWalkBoundsTheParentsOfACommit(t *testing.T) {
	ctx := context.Background()
	t.Run("one parent named 20000 times, through /apply", func(t *testing.T) {
		root := cleanGitFixture(t)
		for i := 0; i < 300; i++ {
			writeTestFile(t, filepath.Join(root, "files", fmt.Sprintf("f%03d.txt", i)), fmt.Sprintf("file %d\n", i))
		}
		baselineGit(t, root, "add", "files")
		baselineGit(t, root, "commit", "-qm", "300 files")
		baseline, err := InspectCleanGitBaseline(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		views := filepath.Join(t.TempDir(), "views")
		view, err := EnsureGitView(ctx, views, "parents", baseline)
		if err != nil {
			t.Fatal(err)
		}
		head := commitPathInGitView(t, view, "files/f000.txt", "changed\n")
		// The commit changes a file too, so its tree is compared with its
		// parent's, once for each time it names that parent.
		writeTestFile(t, filepath.Join(view.Path, "files", "f001.txt"), "changed too\n")
		baselineGit(t, view.Path, "add", "files/f001.txt")
		var text strings.Builder
		fmt.Fprintf(&text, "tree %s\n", baselineGit(t, view.Path, "write-tree"))
		for i := 0; i < 20000; i++ {
			fmt.Fprintf(&text, "parent %s\n", head)
		}
		text.WriteString("author A <a@example.invalid> 1 +0000\ncommitter A <a@example.invalid> 1 +0000\n\nmany\n")
		many := gitWithStdin(t, view.Path, text.String(), "hash-object", "-t", "commit", "--literally", "-w", "--stdin")
		baselineGit(t, view.Path, "update-ref", "HEAD", many)
		var delivered GitViewDelivery
		took, peak := measureHeap(func() { delivered, err = DeliverGitViewBranch(ctx, views, view.ID, baseline) })
		t.Logf("/apply: %v after %s, heap +%d MiB", err, took, peak/mib)
		if !errors.Is(err, errGitWorkTooLarge) {
			t.Fatalf("a commit naming its parent 20000 times was delivered: %+v %v", delivered, err)
		}
	})
	t.Run("200 distinct parents sharing one 20000-entry tree", func(t *testing.T) {
		store := newRawGitStore(t)
		shared := store.fileTree(20000, "shared")
		base := store.commit(store.fileTree(1, "base"), 1000, "base\n")
		var named []string
		for i := 0; i < 200; i++ {
			named = append(named, store.commit(shared, 2000, fmt.Sprintf("side %d\n", i), base))
		}
		head := store.commit(store.fileTree(20001, "shared"), 3000, "merge\n", named...)
		ids, err, took, peak := store.measuredWalk(head, base)
		t.Logf("walk: %d ids, %v after %s, heap +%d MiB", len(ids), err, took, peak/mib)
		if err != nil || peak > 96*mib {
			t.Fatalf("200 parents sharing one tree: %v, heap +%d MiB", err, peak/mib)
		}
	})
	for name, parents := range map[string]struct {
		distinct, refused bool
		count             int
	}{
		"1100 distinct parents":     {distinct: true, refused: true, count: 1100},
		"an octopus of 64 parents":  {distinct: true, count: 64},
		"one parent named 32 times": {count: 32},
	} {
		t.Run(name, func(t *testing.T) {
			store := newRawGitStore(t)
			tree := store.fileTree(1000, "base")
			base := store.commit(tree, 1000, "base\n")
			var named []string
			for i := 0; i < parents.count; i++ {
				parent := base
				if parents.distinct {
					parent = store.commit(store.fileTree(2, fmt.Sprint(i)), 2000, fmt.Sprintf("side %d\n", i), base)
				}
				named = append(named, parent)
			}
			head := store.commit(tree, 3000, "merge\n", named...)
			ids, err, took, peak := store.measuredWalk(head, base)
			t.Logf("walk: %d ids, %v after %s, heap +%d MiB", len(ids), err, took, peak/mib)
			if parents.refused != errors.Is(err, errGitWorkTooLarge) || (!parents.refused && err != nil) {
				t.Fatalf("walk of %s = %v", name, err)
			}
		})
	}
}

// The walk keeps what it needs of a commit, not its text: commits with
// 16 MiB messages or headers leave nothing of them behind.
func TestWalkKeepsNoCommitText(t *testing.T) {
	for name, text := range map[string]func(i int) (header, message string){
		"16 MiB messages": func(i int) (string, string) {
			return "", fmt.Sprintf("%d %s\n", i, strings.Repeat("m", 16*mib))
		},
		"16 MiB headers": func(i int) (string, string) {
			return fmt.Sprintf("x-padding %d %s\n", i, strings.Repeat("h", 16*mib)), "small\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := newRawGitStore(t)
			tree := store.fileTree(3, "base")
			base := store.commit(tree, 1000, "base\n")
			head := base
			for i := 0; i < 40; i++ {
				header, message := text(i)
				body := fmt.Sprintf("tree %s\nparent %s\nauthor A <a@example.invalid> %d +0000\ncommitter A <a@example.invalid> %d +0000\n%s\n%s",
					store.fileTree(3, fmt.Sprint(i)), head, 2000+i, 2000+i, header, message)
				head = store.write("commit", []byte(body))
			}
			ids, err, took, peak := store.measuredWalk(head, base)
			t.Logf("walk: %d ids, %v after %s, heap +%d MiB", len(ids), err, took, peak/mib)
			if err != nil || peak > 256*mib {
				t.Fatalf("40 commits with %s: %v, heap +%d MiB", name, err, peak/mib)
			}
		})
	}
}

// nestedTree is a tree holding a file depth directories below it.
func (s rawGitStore) nestedTree(depth int) string {
	blob := sha1.Sum([]byte("deep file"))
	tree := s.tree(rawGitTreeEntry("100644", "f", hex.EncodeToString(blob[:])))
	for i := 0; i < depth; i++ {
		tree = s.tree(rawGitTreeEntry("40000", "d", tree))
	}
	return tree
}

// The walk follows a tree at most 2048 directories deep, Git's own default
// core.maxTreeDepth, and refuses a deeper one at once instead of exhausting
// the daemon's stack.
func TestWalkRefusesTreesNestedDeeperThanGitReads(t *testing.T) {
	for name, depth := range map[string]struct {
		levels  int
		refused bool
	}{
		"2048 directories deep": {levels: 2048},
		"2049 directories deep": {levels: 2049, refused: true},
		"5000 directories deep": {levels: 5000, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			store := newRawGitStore(t)
			base := store.commit(store.fileTree(1, "base"), 1000, "base\n")
			head := store.commit(store.nestedTree(depth.levels), 2000, "deep\n", base)
			ids, err, took, peak := store.measuredWalk(head, base)
			t.Logf("walk: %d ids, %v after %s, heap +%d MiB", len(ids), err, took, peak/mib)
			if depth.refused != errors.Is(err, errGitWorkTooLarge) || (!depth.refused && err != nil) {
				t.Fatalf("walk of a tree %s = %v", name, err)
			}
		})
	}
}

// What the walk keeps, the trees it has read among it, stays within its
// budget: trees that compress to little on disk but take hundreds of MiB once
// read are refused once the budget is spent, while honest histories are not.
func TestWalkKeepsWithinItsBudget(t *testing.T) {
	t.Run("compressible trees past the budget", func(t *testing.T) {
		defer func(budget int64) { maxGitWalkBytes = budget }(maxGitWalkBytes)
		maxGitWalkBytes = 96 * mib
		store := newRawGitStore(t)
		base := store.commit(store.fileTree(1, "base"), 1000, "base\n")
		head := base
		// Nearly 64 MiB of entries each, 1.86 million.
		for i := 0; i < 2; i++ {
			head = store.commit(store.fileTree(1860000, fmt.Sprint(i)), int64(2000+i), "wide\n", head)
		}
		ids, err, took, peak := store.measuredWalk(head, base)
		t.Logf("walk: %d ids, %v after %s, heap +%d MiB", len(ids), err, took, peak/mib)
		if !errors.Is(err, errGitWorkTooLarge) {
			t.Fatalf("trees past the budget were listed: %v", err)
		}
	})
	t.Run("an honest history over a lowered budget", func(t *testing.T) {
		defer func(budget int64) { maxGitWalkBytes = budget }(maxGitWalkBytes)
		maxGitWalkBytes = 1 * mib
		store := newRawGitStore(t)
		base := store.commit(store.fileTree(10, "base"), 1000, "base\n")
		head := base
		for i := 0; i < 50; i++ {
			head = store.commit(store.fileTree(500, fmt.Sprint(i)), int64(2000+i), "work\n", head)
		}
		if _, err, _, _ := store.measuredWalk(head, base); !errors.Is(err, errGitWorkTooLarge) {
			t.Fatalf("a walk past a 1 MiB budget = %v", err)
		}
	})
	t.Run("an honest large history within the default budget", func(t *testing.T) {
		store := newRawGitStore(t)
		tree := store.fileTree(100, "base")
		base := ""
		for i := 0; i < 25000; i++ {
			var parents []string
			if base != "" {
				parents = []string{base}
			}
			base = store.commit(tree, int64(1600000000+i), fmt.Sprintf("baseline %d\n", i), parents...)
		}
		head := base
		for i := 0; i < 3; i++ {
			head = store.commit(store.fileTree(20000, fmt.Sprint(i)), int64(1000000000+i), "old-dated work\n", head)
		}
		ids, err, took, peak := store.measuredWalk(head, base)
		t.Logf("walk: %d ids, %v after %s, heap +%d MiB", len(ids), err, took, peak/mib)
		if err != nil || !containsString(ids, head) {
			t.Fatalf("an honest large history was refused: %v", err)
		}
	})
}
