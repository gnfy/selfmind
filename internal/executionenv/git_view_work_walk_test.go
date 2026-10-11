package executionenv

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// commitFilesInGitView writes each file and commits them together.
func commitFilesInGitView(t *testing.T, view GitView, files map[string]string) string {
	t.Helper()
	for path, content := range files {
		writeTestFile(t, filepath.Join(view.Path, filepath.FromSlash(path)), content)
		baselineGit(t, view.Path, "add", path)
	}
	baselineGit(t, view.Path, "commit", "-qm", "work")
	return baselineGit(t, view.Path, "rev-parse", "HEAD")
}

// treeBroughtBack are histories in which a directory's tree returns to one an
// older work commit first added, after a commit in between changed it.
var treeBroughtBack = map[string]func(t *testing.T, view GitView){
	"a file edited and reverted beside another": func(t *testing.T, view GitView) {
		commitFilesInGitView(t, view, map[string]string{"q/e.txt": "e content\n", "q/f.txt": "f content\n"})
		commitFilesInGitView(t, view, map[string]string{"q/f.txt": "f edited\n"})
		commitFilesInGitView(t, view, map[string]string{"q/f.txt": "f content\n"})
	},
	"a file moved out of its directory and back": func(t *testing.T, view GitView) {
		commitFilesInGitView(t, view, map[string]string{"q/e.txt": "e content\n", "q/f.txt": "f content\n"})
		baselineGit(t, view.Path, "mv", "q/f.txt", "f.txt")
		baselineGit(t, view.Path, "commit", "-qm", "out")
		baselineGit(t, view.Path, "mv", "f.txt", "q/f.txt")
		baselineGit(t, view.Path, "commit", "-qm", "back")
	},
}

// Every object of the work is listed, also one under a tree that a later
// commit brought back: honest work is delivered whole, and a substitute the
// source holds for such an object stops delivery instead of standing in.
func TestWorkListingKeepsWhatATreeBroughtBackHolds(t *testing.T) {
	ctx := context.Background()
	for name, steps := range treeBroughtBack {
		setup := func(t *testing.T) (string, GitBaseline, string, GitView, string) {
			root := cleanGitFixture(t)
			baseline, err := InspectCleanGitBaseline(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			views := filepath.Join(t.TempDir(), "views")
			view, err := EnsureGitView(ctx, views, "back", baseline)
			if err != nil {
				t.Fatal(err)
			}
			steps(t, view)
			return root, baseline, views, view, baselineGit(t, view.Path, "rev-parse", "HEAD")
		}
		t.Run("honest/"+name, func(t *testing.T) {
			root, baseline, views, view, head := setup(t)
			if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
				t.Fatalf("delivery: %v", err)
			}
			if got := baselineGit(t, root, "cat-file", "-p", head+":q/e.txt"); got != "e content" {
				t.Fatalf("delivered q/e.txt = %q", got)
			}
		})
		t.Run("substitute/"+name, func(t *testing.T) {
			root, baseline, views, view, head := setup(t)
			blob := baselineGit(t, view.Path, "rev-parse", head+":q/e.txt")
			writeLooseGitObject(t, filepath.Join(root, ".git"), blob, looseGitObject(t, "blob", []byte("base\n")))
			if delivered, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err == nil {
				t.Fatalf("delivery passed over a substitute for q/e.txt: %+v", delivered)
			}
			if _, err := RetireGitView(ctx, views, view.ID, baseline, nil); err == nil {
				t.Fatal("archive took undelivered work")
			}
			if got := baselineGit(t, view.Path, "cat-file", "-p", head+":q/e.txt"); got != "e content" {
				t.Fatalf("the view's q/e.txt = %q", got)
			}
		})
	}
}

// historyGen writes generated histories with plumbing: each commit's tree
// from a model of paths, its parents, and its date chosen by the test.
type historyGen struct {
	t       *testing.T
	dir     string
	rng     *rand.Rand
	blobs   []string
	links   []string
	trees   map[string]map[string]genEntry
	parents map[string][]string
	order   []string
	date    int64
	reach   map[string]map[string]bool
	gitEnv  []string
}

type genEntry struct{ mode, id string }

var genPaths = []string{"e.txt", "f.txt", "q", "q/e.txt", "q/f.txt", "q/r", "q/r/e.txt", "s/e.txt", "s/f.txt", "s"}

func newHistoryGen(t *testing.T, seed int64) *historyGen {
	t.Helper()
	g := &historyGen{t: t, dir: t.TempDir(), rng: rand.New(rand.NewSource(seed)),
		trees: map[string]map[string]genEntry{}, parents: map[string][]string{}, reach: map[string]map[string]bool{}, date: 1700000000}
	baselineGit(t, g.dir, "init", "-q")
	g.gitEnv = append(cleanGitProbeEnv(os.Environ()), "GIT_INDEX_FILE="+filepath.Join(t.TempDir(), "index"),
		"GIT_AUTHOR_NAME=Gen", "GIT_AUTHOR_EMAIL=gen@example.invalid", "GIT_COMMITTER_NAME=Gen", "GIT_COMMITTER_EMAIL=gen@example.invalid")
	for i := 0; i < 6; i++ {
		g.blobs = append(g.blobs, gitWithStdin(t, g.dir, fmt.Sprintf("content %d\n", i), "hash-object", "-w", "--stdin"))
	}
	for _, target := range []string{"q", "q/e.txt", "../outside"} {
		g.links = append(g.links, gitWithStdin(t, g.dir, target, "hash-object", "-w", "--stdin"))
	}
	return g
}

func (g *historyGen) git(stdin string, env []string, args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", g.dir}, args...)...)
	cmd.Env = append(append([]string{}, g.gitEnv...), env...)
	cmd.Stdin = strings.NewReader(stdin)
	output, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// set puts entry at path, first removing what would make it a file and a
// directory at once.
func set(model map[string]genEntry, path string, entry genEntry) {
	for other := range model {
		if strings.HasPrefix(other, path+"/") || strings.HasPrefix(path, other+"/") {
			delete(model, other)
		}
	}
	model[path] = entry
}

func clone(model map[string]genEntry) map[string]genEntry {
	out := make(map[string]genEntry, len(model))
	for path, entry := range model {
		out[path] = entry
	}
	return out
}

func (g *historyGen) pick(model map[string]genEntry) string {
	paths := make([]string, 0, len(model))
	for path := range model {
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return ""
	}
	sort.Strings(paths)
	return paths[g.rng.Intn(len(paths))]
}

// mutate applies one to three changes: an added or edited file, a removal, a
// move, a mode-only change, a symlink, a gitlink, a type change, or a return
// to an earlier commit's tree (a revert, or a move back).
func (g *historyGen) mutate(model map[string]genEntry, parent string) map[string]genEntry {
	model = clone(model)
	for n := 1 + g.rng.Intn(3); n > 0; n-- {
		path := genPaths[g.rng.Intn(len(genPaths))]
		switch g.rng.Intn(9) {
		case 0, 1:
			set(model, path, genEntry{"100644", g.blobs[g.rng.Intn(len(g.blobs))]})
		case 2:
			delete(model, g.pick(model))
		case 3:
			if from := g.pick(model); from != "" && from != path {
				entry := model[from]
				delete(model, from)
				set(model, path, entry)
			}
		case 4:
			if at := g.pick(model); at != "" && (model[at].mode == "100644" || model[at].mode == "100755") {
				entry := model[at]
				entry.mode = map[string]string{"100644": "100755", "100755": "100644"}[entry.mode]
				model[at] = entry
			}
		case 5:
			set(model, path, genEntry{"120000", g.links[g.rng.Intn(len(g.links))]})
		case 6:
			set(model, path, genEntry{"160000", fmt.Sprintf("%040x", g.rng.Int63())})
		case 7:
			set(model, path+"/x", genEntry{"100644", g.blobs[g.rng.Intn(len(g.blobs))]})
		default:
			// Back to an earlier tree: most often the grandparent's.
			if len(g.order) == 0 {
				continue
			}
			earlier := g.order[g.rng.Intn(len(g.order))]
			if parents := g.parents[parent]; len(parents) > 0 && g.rng.Intn(2) == 0 {
				earlier = parents[0]
			}
			model = clone(g.trees[earlier])
		}
	}
	return model
}

// merge combines the parents' trees path by path, as a resolved merge may.
func (g *historyGen) merge(parents []string) map[string]genEntry {
	model := map[string]genEntry{}
	for _, parent := range parents {
		for path, entry := range g.trees[parent] {
			if _, held := model[path]; !held || g.rng.Intn(2) == 0 {
				set(model, path, entry)
			}
		}
	}
	return model
}

// commit writes model as a commit of parents, dated with some clock skew.
func (g *historyGen) commit(model map[string]genEntry, parents ...string) string {
	g.t.Helper()
	g.date += int64(g.rng.Intn(400)) - 100
	if g.rng.Intn(8) == 0 {
		g.date -= 86400
	}
	var lines strings.Builder
	for path, entry := range model {
		kind := "blob"
		if entry.mode == "160000" {
			kind = "commit"
		}
		fmt.Fprintf(&lines, "%s %s %s\t%s\n", entry.mode, kind, entry.id, path)
	}
	index := ""
	for _, variable := range g.gitEnv {
		if value, ok := strings.CutPrefix(variable, "GIT_INDEX_FILE="); ok {
			index = value
		}
	}
	_ = os.Remove(index)
	g.git(lines.String(), nil, "update-index", "--index-info")
	tree := g.git("", nil, "write-tree")
	args := []string{"commit-tree", tree, "-m", fmt.Sprintf("commit %d", len(g.order))}
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}
	stamp := fmt.Sprintf("%d +0000", g.date)
	commit := g.git("", []string{"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp}, args...)
	g.trees[commit] = model
	g.parents[commit] = parents
	g.order = append(g.order, commit)
	return commit
}

// reachable is every object rev-list reaches from commit.
func (g *historyGen) reachable(commit string) map[string]bool {
	if set := g.reach[commit]; set != nil {
		return set
	}
	set := map[string]bool{}
	for _, id := range strings.Fields(g.git("", nil, "rev-list", "--objects", "--no-object-names", commit)) {
		set[id] = true
	}
	g.reach[commit] = set
	return set
}

// The walk never lists less than the work needs. On generated honest
// histories (merges, octopus and criss-cross merges, renames, mode-only
// changes, symlinks, gitlinks, type changes, reverts, moves back, clock skew)
// every object rev-list reaches from a head but not from a base is listed,
// and nothing outside the head's history is.
func TestWorkListingCoversGeneratedHistories(t *testing.T) {
	var pairs atomic.Int64
	// The seeds run in parallel inside this group, which returns once they are
	// done, so the count is logged after it.
	t.Run("seeds", func(t *testing.T) {
		for seed := int64(1); seed <= 30; seed++ {
			t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) { checkGeneratedHistory(t, seed, &pairs) })
		}
	})
	t.Logf("%d generated head and base pairs", pairs.Load())
}

// checkGeneratedHistory generates one history and checks the walk over every
// pair of a work commit and a base it names.
func checkGeneratedHistory(t *testing.T, seed int64, pairs *atomic.Int64) {
	t.Parallel()
	// Odd seeds walk with no tree cache, so every tree the walk is done
	// with is read again when it is needed.
	cacheLimit := maxGitWalkCacheBytes
	if seed%2 == 1 {
		cacheLimit = 0
	}
	g := newHistoryGen(t, seed)
	var chain []string
	model := map[string]genEntry{}
	for i := 0; i < 5; i++ {
		parent := ""
		var parents []string
		if len(chain) > 0 {
			parent = chain[len(chain)-1]
			parents = []string{parent}
		}
		model = g.mutate(model, parent)
		chain = append(chain, g.commit(model, parents...))
	}
	// Side branches off old baseline commits: one merged into the
	// baseline, one only ever merged by the work.
	merged := g.commit(g.mutate(g.trees[chain[1]], chain[1]), chain[1])
	unmerged := g.commit(g.mutate(g.trees[chain[2]], chain[2]), chain[2])
	base := g.commit(g.merge([]string{chain[len(chain)-1], merged}), chain[len(chain)-1], merged)
	pool := append(append([]string{}, chain...), merged, unmerged, base)
	tips := []string{base}
	var work []string
	for step := 0; step < 12; step++ {
		switch op := g.rng.Intn(10); {
		case op < 5:
			i := g.rng.Intn(len(tips))
			tips[i] = g.commit(g.mutate(g.trees[tips[i]], tips[i]), tips[i])
			work = append(work, tips[i])
		case op < 6:
			from := pool[g.rng.Intn(len(pool))]
			tips = append(tips, g.commit(g.mutate(g.trees[from], from), from))
			work = append(work, tips[len(tips)-1])
		case op < 8:
			i := g.rng.Intn(len(tips))
			other := pool[g.rng.Intn(len(pool))]
			if other == tips[i] {
				continue
			}
			tips[i] = g.commit(g.mutate(g.merge([]string{tips[i], other}), tips[i]), tips[i], other)
			work = append(work, tips[i])
		case op < 9:
			parents := []string{tips[g.rng.Intn(len(tips))]}
			for _, candidate := range []string{pool[g.rng.Intn(len(pool))], pool[g.rng.Intn(len(pool))]} {
				if !containsString(parents, candidate) {
					parents = append(parents, candidate)
				}
			}
			if len(parents) < 3 {
				continue
			}
			tips = append(tips, g.commit(g.merge(parents), parents...))
			work = append(work, tips[len(tips)-1])
		default:
			if len(tips) < 2 {
				tips = append(tips, g.commit(g.mutate(g.trees[base], base), base))
				work = append(work, tips[len(tips)-1])
				continue
			}
			a, b := tips[0], tips[1]
			tips[0] = g.commit(g.merge([]string{a, b}), a, b)
			tips[1] = g.commit(g.merge([]string{b, a}), b, a)
			work = append(work, tips[0], tips[1])
		}
		pool = append(pool, work[len(work)-1])
	}
	head := tips[0]
	if len(tips) > 1 {
		head = g.commit(g.merge(tips), tips...)
	}
	work = append(work, head)
	bases := []string{base, chain[2], unmerged}
	reader, err := newGitObjectReader(context.Background(), g.dir, gitViewEnv(g.dir), errGitViewIncomplete, errGitViewCorrupt)
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range work {
		for _, against := range bases {
			if from == against {
				continue
			}
			pairs.Add(1)
			walk := newGitWorkWalk(reader)
			walk.cacheLimit = cacheLimit
			ids, err := walk.run(from, against)
			if err != nil {
				t.Fatalf("seed %d: walk %s over %s: %v", seed, from, against, err)
			}
			listed := map[string]bool{}
			for _, id := range ids {
				listed[id] = true
			}
			needed, held := g.reachable(from), g.reachable(against)
			for id := range needed {
				if !held[id] && !listed[id] {
					t.Fatalf("seed %d: walk %s over %s leaves out %s (%s)", seed, from, against, id, g.git("", nil, "cat-file", "-t", id))
				}
			}
			for id := range listed {
				if !needed[id] {
					t.Fatalf("seed %d: walk %s over %s lists %s from outside the head's history", seed, from, against, id)
				}
			}
		}
	}
	reader.close()
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// An id a work tree names both as a directory and as a file is refused,
// whichever comes first: listed as a file, the directory's contents would
// never be read.
func TestWorkListingRefusesAnIDNamedAsTreeAndFile(t *testing.T) {
	ctx := context.Background()
	for name, names := range map[string][2]string{
		"the file first":      {"x", "y"},
		"the directory first": {"y", "x"},
	} {
		t.Run(name, func(t *testing.T) {
			root := cleanGitFixture(t)
			baseline, err := InspectCleanGitBaseline(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			view, err := EnsureGitView(ctx, filepath.Join(t.TempDir(), "views"), "conflict", baseline)
			if err != nil {
				t.Fatal(err)
			}
			head := commitPathInGitView(t, view, "a/b.txt", "work\n")
			tree := baselineGit(t, view.Path, "rev-parse", head+":a")
			raw := rawGitTreeEntry("100644", names[0], tree) + rawGitTreeEntry("40000", names[1], tree)
			if names[1] < names[0] {
				raw = rawGitTreeEntry("40000", names[1], tree) + rawGitTreeEntry("100644", names[0], tree)
			}
			root2 := gitWithStdin(t, view.Path, raw, "hash-object", "-t", "tree", "--literally", "-w", "--stdin")
			commit := baselineGit(t, view.Path, "commit-tree", root2, "-p", head, "-m", "both")
			if _, err := listGitViewWork(ctx, view.Path, commit, baseline.Commit); !errors.Is(err, errGitViewCorrupt) || !strings.Contains(err.Error(), "both as a tree and as a file") {
				t.Fatalf("a tree id named as a file and as a directory = %v", err)
			}
		})
	}
}

func rawGitTreeEntry(mode, name, id string) string {
	raw, err := hex.DecodeString(id)
	if err != nil {
		panic(err)
	}
	return mode + " " + name + "\x00" + string(raw)
}

// The commit bound counts commits the walk does not yet know to be the
// baseline's. Work dated long before a long baseline history, or a merge of
// an old side branch, makes the walk read all of that history first; it is
// the baseline's, so the work is listed. More work than the bound is not.
func TestWalkBoundCountsOnlyCommitsNotKnownToBeTheBaselines(t *testing.T) {
	defer func(bound int) { maxGitWalkCommits = bound }(maxGitWalkCommits)
	maxGitWalkCommits = 20
	setup := func(t *testing.T) (*historyGen, string, []string) {
		g := newHistoryGen(t, 7)
		g.date = 1600000000
		var chain []string
		model := map[string]genEntry{}
		for i := 0; i < 60; i++ {
			var parents []string
			if len(chain) > 0 {
				parents = []string{chain[len(chain)-1]}
			}
			model = clone(model)
			set(model, "e.txt", genEntry{"100644", g.blobs[i%len(g.blobs)]})
			g.date += 1000
			chain = append(chain, g.commit(model, parents...))
		}
		return g, chain[len(chain)-1], chain
	}
	walk := func(t *testing.T, g *historyGen, head, base string) ([]string, error) {
		reader, err := newGitObjectReader(context.Background(), g.dir, gitViewEnv(g.dir), errGitViewIncomplete, errGitViewCorrupt)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.close()
		return listGitWorkObjects(reader, head, base)
	}
	t.Run("old-dated work on a long baseline history", func(t *testing.T) {
		g, base, _ := setup(t)
		g.date = 1000000000
		head := base
		for i := 0; i < 3; i++ {
			model := clone(g.trees[head])
			set(model, "f.txt", genEntry{"100644", g.blobs[i]})
			head = g.commit(model, head)
		}
		if ids, err := walk(t, g, head, base); err != nil || !containsString(ids, head) {
			t.Fatalf("old-dated work: %v", err)
		}
	})
	t.Run("a merge of an old side branch", func(t *testing.T) {
		g, base, chain := setup(t)
		g.date = 1000000000
		side := chain[5]
		for i := 0; i < 3; i++ {
			model := clone(g.trees[side])
			set(model, "s/e.txt", genEntry{"100644", g.blobs[i]})
			side = g.commit(model, side)
		}
		g.date = 1700000000
		head := g.commit(g.merge([]string{base, side}), base, side)
		if ids, err := walk(t, g, head, base); err != nil || !containsString(ids, side) {
			t.Fatalf("merge of an old side branch: %v", err)
		}
	})
	t.Run("more work than the bound", func(t *testing.T) {
		g, base, _ := setup(t)
		head := base
		for i := 0; i < maxGitWalkCommits+5; i++ {
			model := clone(g.trees[head])
			set(model, "f.txt", genEntry{"100644", g.blobs[i%len(g.blobs)]})
			head = g.commit(model, head)
		}
		if _, err := walk(t, g, head, base); !errors.Is(err, errGitWorkTooLong) {
			t.Fatalf("work longer than the bound: %v", err)
		}
	})
}
