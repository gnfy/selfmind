package executionenv

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// walkCost is what one walk took: its result, wall and CPU time, the peak and
// the live growth of the heap, and the most it charged against its budget.
type walkCost struct {
	ids                []string
	err                error
	took, cpu          time.Duration
	peak, live, charge uint64
	steps              int64
}

func (c walkCost) String() string {
	return fmt.Sprintf("%d ids, %v, %s wall, %s CPU, %d steps, heap peak +%d MiB, live +%d MiB, charged %d MiB",
		len(c.ids), c.err, c.took.Round(time.Millisecond), c.cpu.Round(time.Millisecond), c.steps, c.peak/mib, c.live/mib, c.charge/mib)
}

func processCPU() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// costedWalk runs the walk under ctx and measures it.
func (s rawGitStore) costedWalk(ctx context.Context, head, base string) walkCost {
	s.t.Helper()
	reader, err := newGitObjectReader(ctx, s.dir, gitViewEnv(s.dir), errGitViewIncomplete, errGitViewCorrupt)
	if err != nil {
		s.t.Fatal(err)
	}
	defer reader.close()
	var cost walkCost
	walk := newGitWorkWalk(reader)
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	before := stats.HeapAlloc
	cpu := processCPU()
	cost.took, cost.peak = measureHeap(func() { cost.ids, cost.err = walk.run(head, base) })
	cost.cpu = processCPU() - cpu
	runtime.GC()
	runtime.ReadMemStats(&stats)
	if stats.HeapAlloc > before {
		cost.live = stats.HeapAlloc - before
	}
	cost.charge = uint64(walk.peakCharged())
	cost.steps = walk.steps
	runtime.KeepAlive(walk)
	return cost
}

// parentsWithTreesAtA writes count commits on base, each whose root holds a
// distinct one-file tree at a.
func (s rawGitStore) parentsWithTreesAtA(base string, count int) []string {
	var parents []string
	for i := 0; i < count; i++ {
		root := s.tree(rawGitTreeEntry("40000", "a", s.fileTree(1, fmt.Sprint("a", i))))
		parents = append(parents, s.commit(root, 2000, fmt.Sprintf("parent %d\n", i), base))
	}
	return parents
}

// The walk's CPU is bounded and its deadline honored: an entry it already
// knows needs no comparison with the parents, every comparison and parsed
// entry is charged against a fixed number of steps, and the walk checks its
// context as it goes. A head under 1024 parents whose root repeats one
// directory entry 500,000 times completes in moments; distinct names the
// parents lack, short or 4 KiB long, are refused once the steps are spent;
// and a short deadline ends the walk as an interruption.
func TestWalkBoundsItsWorkAndHonorsItsDeadline(t *testing.T) {
	setup := func(t *testing.T) (rawGitStore, string, []string) {
		store := newRawGitStore(t)
		base := store.commit(store.fileTree(1, "base"), 1000, "base\n")
		return store, base, store.parentsWithTreesAtA(base, 1024)
	}
	t.Run("one directory entry repeated 500000 times", func(t *testing.T) {
		store, base, parents := setup(t)
		repeated := store.fileTree(1, "repeated")
		var root bytes.Buffer
		for i := 0; i < 500000; i++ {
			root.WriteString(rawGitTreeEntry("40000", "a", repeated))
		}
		head := store.commit(store.write("tree", root.Bytes()), 3000, "head\n", parents...)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cost := store.costedWalk(ctx, head, base)
		t.Logf("walk: %s", cost)
		if cost.err != nil || cost.took > 15*time.Second {
			t.Fatalf("a repeated entry under 1024 parents: %s", cost)
		}
	})
	for name, shape := range map[string]struct {
		entries int
		name    func(i int) string
	}{
		"300000 distinct names the parents lack": {entries: 300000, name: func(i int) string { return fmt.Sprintf("n%07d", i) }},
		"16000 distinct 4 KiB names the parents lack": {entries: 16000, name: func(i int) string {
			return fmt.Sprintf("%07d", i) + strings.Repeat("x", 4089)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func(steps int64) { maxGitWalkSteps = steps }(maxGitWalkSteps)
			maxGitWalkSteps = 1 << 24
			store, base, parents := setup(t)
			var root bytes.Buffer
			for i := 0; i < shape.entries; i++ {
				blob := sha1.Sum([]byte(fmt.Sprint("blob", i)))
				fmt.Fprintf(&root, "100644 %s\x00", shape.name(i))
				root.Write(blob[:])
			}
			head := store.commit(store.write("tree", root.Bytes()), 3000, "head\n", parents...)
			cost := store.costedWalk(context.Background(), head, base)
			t.Logf("walk: %s", cost)
			if !errors.Is(cost.err, errGitWorkTooLarge) {
				t.Fatalf("names the parents lack under 1024 parents: %s", cost)
			}
		})
	}
	t.Run("a short deadline", func(t *testing.T) {
		store, base, parents := setup(t)
		var root bytes.Buffer
		for i := 0; i < 300000; i++ {
			blob := sha1.Sum([]byte(fmt.Sprint("blob", i)))
			fmt.Fprintf(&root, "100644 n%07d\x00", i)
			root.Write(blob[:])
		}
		head := store.commit(store.write("tree", root.Bytes()), 3000, "head\n", parents...)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		cost := store.costedWalk(ctx, head, base)
		t.Logf("walk: %s", cost)
		if !gitInterrupted(cost.err) || cost.took > 5*time.Second {
			t.Fatalf("a walk past its deadline: %s", cost)
		}
	})
}

// The heap stays near what the walk charges: an id named by many commits is
// held once, and charges follow Go's allocation sizes. 3000 commits that each
// name the same 1024 parents keep their live heap within what was charged and
// peak at most about twice that, plus the object being read.
func TestWalkHeapStaysNearItsCharge(t *testing.T) {
	store := newRawGitStore(t)
	base := store.commit(store.fileTree(1, "base"), 1000, "base\n")
	shared := store.parentsWithTreesAtA(base, 1023)
	head := base
	for i := 0; i < 3000; i++ {
		head = store.commit(store.fileTree(1, "base"), int64(3000+i), fmt.Sprintf("commit %d\n", i), append([]string{head}, shared...)...)
	}
	cost := store.costedWalk(context.Background(), head, base)
	t.Logf("walk: %s", cost)
	if cost.err != nil || cost.live > cost.charge+cost.charge/8+16*mib || cost.peak > 2*cost.charge+64*mib {
		t.Fatalf("heap beyond the charge: %s", cost)
	}
}

// Only what the walk must retain counts against its budget; trees it is done
// with are cached and read again when evicted. 300 honest commits that each
// change one file in an 8,000-file directory are listed, with the default
// cache and with none.
func TestWalkRetainsOnlyWhatItMust(t *testing.T) {
	for name, cache := range map[string]int64{"default cache": maxGitWalkCacheBytes, "no cache": 0} {
		t.Run(name, func(t *testing.T) {
			defer func(bytes int64) { maxGitWalkCacheBytes = bytes }(maxGitWalkCacheBytes)
			maxGitWalkCacheBytes = cache
			store := newRawGitStore(t)
			wide := func(changed int) string {
				var content bytes.Buffer
				for f := 0; f < 8000; f++ {
					blob := sha1.Sum([]byte(fmt.Sprint("file", f)))
					if f == changed%8000 {
						blob = sha1.Sum([]byte(fmt.Sprint("change", changed)))
					}
					fmt.Fprintf(&content, "100644 f%07d\x00", f)
					content.Write(blob[:])
				}
				return store.tree(rawGitTreeEntry("40000", "wide", store.write("tree", content.Bytes())))
			}
			base := store.commit(wide(-1), 1000, "base\n")
			head := base
			for i := 0; i < 300; i++ {
				head = store.commit(wide(i), int64(2000+i), "change\n", head)
			}
			cost := store.costedWalk(context.Background(), head, base)
			t.Logf("walk: %s", cost)
			if cost.err != nil || len(cost.ids) < 300*4 {
				t.Fatalf("300 honest commits in an 8,000-file directory: %s", cost)
			}
		})
	}
}

// longNameTree is a one-entry tree whose entry name is size bytes long: a
// tree of that size which costs the walk a single entry to parse.
func (s rawGitStore) longNameTree(size int, salt string) string {
	blob := sha1.Sum([]byte(salt))
	return s.tree(rawGitTreeEntry("100644", salt+"-"+strings.Repeat("n", size), hex.EncodeToString(blob[:])))
}

// A commit that names one parent many times keeps that one parent, in a slice
// of that size: commits that each name their parent 1,024 times hold what the
// walk charges for them, not the 16 KiB a slice sized for every line would
// cost each. (At the 100,000-commit bound, 1.7 GiB live.)
func TestWalkHoldsOnlyTheParentsACommitKeeps(t *testing.T) {
	store := newRawGitStore(t)
	tree := store.fileTree(1, "t")
	base := store.commit(tree, 1000, "base\n")
	head := base
	for i := 0; i < 5000; i++ {
		repeated := make([]string, 1024)
		for line := range repeated {
			repeated[line] = head
		}
		head = store.commit(tree, int64(2000+i), fmt.Sprintf("commit %d\n", i), repeated...)
	}
	cost := store.costedWalk(context.Background(), head, base)
	t.Logf("walk: %s", cost)
	if cost.err != nil || cost.live > cost.charge+cost.charge/8+16*mib || cost.peak > 2*cost.charge+64*mib {
		t.Fatalf("5000 commits each naming their parent 1024 times: %s", cost)
	}
}

// What the walk reads is charged by its size, and a tree read again after the
// cache dropped it is read again: trees too large for the cache that many
// comparisons need in turn make the walk read and hash them over and over,
// and that is bounded by its steps, not only by the reader's five minutes.
// Scaled to a 4 MiB cache, a 2^24-step budget, and one-entry trees of 128 KiB
// (names that long): without a charge by size each read costs four steps.
func TestWalkChargesWhatItReadsAgain(t *testing.T) {
	defer func(steps, cache int64) { maxGitWalkSteps, maxGitWalkCacheBytes = steps, cache }(maxGitWalkSteps, maxGitWalkCacheBytes)
	maxGitWalkSteps = 1 << 24
	maxGitWalkCacheBytes = 4 * mib
	const parents, spread, nameBytes = 100, 100, 128 << 10
	for name, build := range map[string]func(store rawGitStore, base string) string{
		// Each of a head's subtrees is compared with the large tree every
		// parent holds at the same path.
		"every subtree of a head against each parent's large tree": func(store rawGitStore, base string) string {
			var named []string
			for j := 0; j < parents; j++ {
				big := store.longNameTree(nameBytes, fmt.Sprint("big", j))
				var root []string
				for d := 0; d < spread; d++ {
					root = append(root, rawGitTreeEntry("40000", fmt.Sprintf("d%03d", d), big))
				}
				named = append(named, store.commit(store.tree(root...), 2000, fmt.Sprintf("parent %d\n", j), base))
			}
			var root []string
			for d := 0; d < spread; d++ {
				root = append(root, rawGitTreeEntry("40000", fmt.Sprintf("d%03d", d), store.fileTree(1, fmt.Sprint("s", d))))
			}
			return store.commit(store.tree(root...), 3000, "head\n", named...)
		},
		// Each of many commits is compared with the large root of every one
		// of the same shared parents.
		"many commits sharing parents whose roots are large": func(store rawGitStore, base string) string {
			var shared []string
			for j := 0; j < parents; j++ {
				shared = append(shared, store.commit(store.longNameTree(nameBytes, fmt.Sprint("shared", j)), 2000, fmt.Sprintf("shared %d\n", j), base))
			}
			head := base
			for k := 0; k < spread; k++ {
				head = store.commit(store.fileTree(1, fmt.Sprint("work", k)), int64(3000+k), "work\n", append([]string{head}, shared...)...)
			}
			return head
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := newRawGitStore(t)
			base := store.commit(store.fileTree(1, "base"), 1000, "base\n")
			head := build(store, base)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cost := store.costedWalk(ctx, head, base)
			t.Logf("walk: %s", cost)
			if !errors.Is(cost.err, errGitWorkTooLarge) || !strings.Contains(cost.err.Error(), "steps") || cost.took > 15*time.Second {
				t.Fatalf("trees read again past the step budget: %s", cost)
			}
		})
	}
}

// Every read costs a round trip to Git besides what its size costs, so trees
// too small to matter by their size are bounded too: with nothing cached, 200
// commits that each name the same 100 parents read 20,000 tiny trees again, and
// that is more than a 2^22-step budget allows.
func TestWalkChargesTheRoundTripOfEveryRead(t *testing.T) {
	defer func(steps, cache int64) { maxGitWalkSteps, maxGitWalkCacheBytes = steps, cache }(maxGitWalkSteps, maxGitWalkCacheBytes)
	maxGitWalkSteps = 1 << 22
	maxGitWalkCacheBytes = 1
	store := newRawGitStore(t)
	base := store.commit(store.fileTree(1, "base"), 1000, "base\n")
	var shared []string
	for j := 0; j < 100; j++ {
		shared = append(shared, store.commit(store.fileTree(1, fmt.Sprint("shared", j)), 2000, fmt.Sprintf("shared %d\n", j), base))
	}
	head := base
	for k := 0; k < 200; k++ {
		head = store.commit(store.fileTree(1, fmt.Sprint("work", k)), int64(3000+k), "work\n", append([]string{head}, shared...)...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cost := store.costedWalk(ctx, head, base)
	t.Logf("walk: %s", cost)
	if !errors.Is(cost.err, errGitWorkTooLarge) || !strings.Contains(cost.err.Error(), "steps") || cost.took > 15*time.Second {
		t.Fatalf("tiny trees read again past the step budget: %s", cost)
	}
}
