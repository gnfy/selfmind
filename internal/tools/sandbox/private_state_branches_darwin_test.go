package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// Private state does not share one directory chain. The daemon's data directory,
// the configuration, the credential store and a socket outside the data
// directory each hang off the writable root by their own path, and every
// directory on every one of those paths has to be pinned. The earlier deep test
// put all of them under one parent, so a pinning that only followed the first
// path, or only the directories, or only the immediate parent, passed it.

type branchedTree struct {
	root    string
	pinned  []string
	private PrivateState
	decoy   string
}

func newBranchedTree(t *testing.T) *branchedTree {
	t.Helper()
	skipWithoutSeatbelt(t)
	root := shortSocketDir(t)
	tree := &branchedTree{root: root, decoy: filepath.Join(root, "decoy")}
	writeAll(t, map[string]string{
		filepath.Join(root, "a", "b", "data", "control.db"):            treeDBContent,
		filepath.Join(root, "a", "b", "data", "gateway", "state.json"): "{}",
		filepath.Join(root, "a", "b", "data", "backups", "b.json"):     "{}",
		filepath.Join(root, "c", "d", "e", "config.yaml"):              treeConfigContent,
		filepath.Join(root, "f", "auth.json"):                          "token",
		filepath.Join(tree.decoy, "marker"):                            "decoy",
	})
	sockDir := filepath.Join(root, "g", "h", "sockdir")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(sockDir, "x.sock")
	serveSocket(t, socket)
	tree.private = PrivateState{
		Dirs:         []string{filepath.Join(root, "a", "b", "data", "gateway"), filepath.Join(root, "a", "b", "data", "backups")},
		Files:        []string{filepath.Join(root, "a", "b", "data", "control.db"), filepath.Join(root, "c", "d", "e", "config.yaml"), filepath.Join(root, "f", "auth.json")},
		FilePrefixes: []string{filepath.Join(root, "c", "d", "e", "config.yaml."), filepath.Join(root, "f", "auth.json.")},
		Sockets:      []string{socket},
	}
	for _, rel := range []string{"a", "a/b", "a/b/data", "c", "c/d", "c/d/e", "f", "g", "g/h", "g/h/sockdir"} {
		tree.pinned = append(tree.pinned, filepath.Join(root, filepath.FromSlash(rel)))
	}
	return tree
}

func (tree *branchedTree) policy(private PrivateState) Policy {
	return Policy{WritableRoots: []string{tree.root}, Network: true, Private: private}
}

func TestSeatbeltEveryDirectoryOnEveryPrivatePathIsPinned(t *testing.T) {
	probe := newBranchedTree(t)
	for _, dir := range probe.pinned {
		rel, _ := filepath.Rel(probe.root, dir)
		for _, attack := range []string{"rename", "swap"} {
			t.Run(attack+" "+rel, func(t *testing.T) {
				run := func(tree *branchedTree, policy Policy) (string, error) {
					target := filepath.Join(tree.root, rel)
					if attack == "rename" {
						return seatbeltRun(t, policy, "mv "+target+" "+target+"-moved")
					}
					return seatbeltRunHelper(t, policy, "swap", target, tree.decoy)
				}
				// Without the pin the same command moves the directory: the denial
				// below is the policy's.
				bare := newBranchedTree(t)
				if out, err := run(bare, bare.policy(PrivateState{})); err != nil {
					t.Fatalf("the attack must work against an unprotected tree: %v (%s)", err, out)
				}

				tree := newBranchedTree(t)
				target := filepath.Join(tree.root, rel)
				before, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				if out, err := run(tree, tree.policy(tree.private)); err == nil {
					t.Fatalf("a confined command moved %s: %s", rel, out)
				}
				if after, err := os.Stat(target); err != nil || !os.SameFile(before, after) {
					t.Fatalf("%s no longer names the same directory: %v", rel, err)
				}
			})
		}
	}
}
