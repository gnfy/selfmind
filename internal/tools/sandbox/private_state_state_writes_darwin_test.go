package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two halves of one seatbelt policy meet here. A command that may refresh a
// credential cache in place (StateWritePaths, with the canary and the runtime's
// read-only records) is still a command on the same host as the daemon, and the
// daemon's private state must stay closed to it: nothing the cache allowance
// opens may reopen a private name, and nothing the private rules add may break
// the cache write. Each test runs under the real sandbox-exec with the whole
// policy a real call carries, and against a control that lacks the rule under
// test, so the denial shown is that rule's.

// composedTree is a daemonTree with a credential cache beside it that the policy
// lets the command write in place.
type composedTree struct {
	*daemonTree
	auth    string
	aws     string
	cache   string
	records string
}

const composedAuthContent = "auth-secret"

func newComposedTree(t *testing.T, layout ...string) *composedTree {
	t.Helper()
	tree := newDaemonTree(t, layout...)
	c := &composedTree{
		daemonTree: tree,
		auth:       filepath.Join(tree.dataDir, "auth.json"),
		aws:        filepath.Join(tree.root, ".aws"),
		records:    filepath.Join(tree.root, "runtime", "host-state"),
	}
	c.cache = filepath.Join(c.aws, "sso", "cache")
	writeAll(t, map[string]string{
		c.auth:                                  composedAuthContent,
		filepath.Join(c.aws, "config"):          "[default]\n",
		filepath.Join(c.cache, "existing.json"): "{}",
		filepath.Join(c.records, "marker"):      "record",
	})
	info, err := os.Lstat(c.auth)
	if err != nil {
		t.Fatal(err)
	}
	tree.identity[c.auth] = info
	return c
}

// private is the daemon's private state with the credentials file and the
// temporary names its store writes beside it.
func (c *composedTree) private() PrivateState {
	private := c.daemonTree.private()
	private.Files = append(private.Files, c.auth)
	private.FilePrefixes = append(private.FilePrefixes, c.auth+".")
	return private
}

// policy is the whole policy of a call that may write the cache in place: the
// writable roots, the flat cache, the command's canary, and the runtime's records
// kept read-only, with the daemon's private state on top.
func (c *composedTree) policy(private PrivateState, roots ...string) Policy {
	if len(roots) == 0 {
		roots = []string{c.root}
	}
	return Policy{
		WritableRoots:   roots,
		Network:         true,
		Private:         private,
		StateWritePaths: []string{c.cache},
		FlatStatePaths:  []string{c.cache},
		Canary:          NewCanary(),
		DenyWritePaths:  []string{c.records},
	}
}

func (c *composedTree) authIntact() error {
	if got, err := os.ReadFile(c.auth); err != nil || string(got) != composedAuthContent {
		return fmt.Errorf("%s reads %q (%v)", c.auth, got, err)
	}
	return nil
}

func TestSeatbeltInPlaceCredentialWritesAndPrivateStateHoldTogether(t *testing.T) {
	layouts := []struct {
		name     string
		homeWide bool
	}{
		{"home is the writable root", true},
		{"only the project is writable", false},
	}
	for _, layout := range layouts {
		t.Run(layout.name, func(t *testing.T) {
			c := newComposedTree(t, ".selfmind")
			roots := []string{c.project}
			if layout.homeWide {
				roots = []string{c.root}
			}
			policy := c.policy(c.private(), roots...)

			// The credential write is live in this very policy.
			token := filepath.Join(c.cache, "token.json")
			renamed := filepath.Join(c.cache, "renamed.json")
			if out, err := seatbeltRun(t, policy, "printf token > "+token+" && mv "+token+" "+renamed); err != nil {
				t.Fatalf("the in-place credential write must work beside the private rules: %v (%s)", err, out)
			}
			if got, _ := os.ReadFile(renamed); string(got) != "token" {
				t.Fatalf("the cache write did not land: %q", got)
			}
			if out, err := seatbeltRun(t, policy, "printf x > "+filepath.Join(c.project, "new.txt")+" && cat "+filepath.Join(c.dataDir, "skills", "skill.md")); err != nil || !strings.Contains(out, "skill") {
				t.Fatalf("ordinary work must keep working: err=%v out=%q", err, out)
			}

			// The private state is closed in the same policy.
			for _, read := range []struct{ name, script string }{
				{"the control store", "cat " + c.db},
				{"the configuration", "cat " + c.config},
				{"a backup of the configuration", "cat " + c.config + ".bak-1"},
				{"the credentials file", "cat " + c.auth},
				{"the daemon's control socket", curlUnix(c.socket)},
			} {
				if out, err := seatbeltRun(t, policy, read.script); err == nil || leaked(out) || strings.Contains(out, composedAuthContent) {
					t.Errorf("%s must stay unreachable beside an in-place cache write: err=%v out=%q", read.name, err, out)
				}
			}
			for _, plant := range []struct{ name, script string }{
				{"a temporary credentials file", "printf planted > " + c.auth + ".tmp"},
				{"an OAuth refresh temporary file", "printf planted > " + c.auth + ".1234567.tmp"},
				{"a configuration copy", "printf planted > " + c.config + ".bak-9"},
				{"the control store", "printf planted >> " + c.db},
			} {
				if out, err := seatbeltRun(t, policy, plant.script); err == nil {
					t.Errorf("a command planted %s beside an in-place cache write: %q", plant.name, out)
				}
			}
			if err := c.intact(); err != nil {
				t.Fatal(err)
			}
			if err := c.authIntact(); err != nil {
				t.Fatalf("the credentials file changed: %v", err)
			}
			if _, err := os.Lstat(c.auth + ".tmp"); !os.IsNotExist(err) {
				t.Fatalf("a temporary credentials file was planted: %v", err)
			}

			// The cache's own containment holds in the same policy.
			denied := []struct{ name, script string }{
				{"a symbolic link in the cache", "ln -s " + filepath.Join(c.aws, "config") + " " + filepath.Join(c.cache, "link.json")},
				{"a hard link in the cache", "ln " + filepath.Join(c.aws, "config") + " " + filepath.Join(c.cache, "hard.json")},
				{"a directory in a flat cache", "mkdir " + filepath.Join(c.cache, "sub")},
				{"the cache directory's mode", "chmod 0777 " + c.cache},
				{"the runtime's records", "printf forged > " + filepath.Join(c.records, "forged")},
			}
			if !layout.homeWide {
				denied = append(denied, struct{ name, script string }{"the configuration beside the cache", "printf x >> " + filepath.Join(c.aws, "config")})
			}
			for _, attack := range denied {
				if out, err := seatbeltRun(t, policy, attack.script); err == nil {
					t.Errorf("%s must be denied beside the private rules: %q", attack.name, out)
				}
			}
			if out, err := seatbeltRun(t, policy, "rm -f "+c.cache+"/*.json && rmdir "+c.cache); err == nil {
				t.Errorf("the cache directory itself must not be removable: %q", out)
			}
			if info, err := os.Stat(c.cache); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Fatalf("the cache directory changed: %v %v", info, err)
			}

			// The constraint that must change the result: with nothing private the
			// same reads and plants succeed, so the denials above are the private
			// rules'.
			open := c.policy(PrivateState{}, roots...)
			if out, err := seatbeltRun(t, open, "cat "+c.config+" && cat "+c.auth); err != nil || !leaked(out) {
				t.Fatalf("without the private rules the configuration must be readable: err=%v out=%q", err, out)
			}
			if out, err := seatbeltRun(t, open, "printf planted > "+c.auth+".tmp"); err != nil && layout.homeWide {
				t.Fatalf("without the private rules the plant must succeed in a writable root: %v (%s)", err, out)
			}
		})
	}
}

// The moves of a private name's ancestors, each against the composed policy: the
// cache allowance neither reopens a name nor is lost to the pins, and the attack
// is real, because the same command works against a policy with nothing private.
func TestSeatbeltInPlaceCredentialWritesDoNotReopenPrivateNames(t *testing.T) {
	for _, attack := range moveAttacks() {
		t.Run(attack.name, func(t *testing.T) {
			layout := attack.layout
			if layout == nil {
				layout = []string{".selfmind"}
			}

			bare := newComposedTree(t, layout...)
			if out, err := attack.run(t, bare.daemonTree, bare.policy(PrivateState{})); err != nil {
				t.Fatalf("the attack must work against a policy with nothing private, or the test proves nothing: %v (%s)", err, out)
			}
			effect := attack.effect
			if effect == nil {
				effect = func(tree *daemonTree) bool { return tree.intact() != nil }
			}
			if !effect(bare.daemonTree) {
				t.Fatal("the attack changed nothing even without protection")
			}

			c := newComposedTree(t, layout...)
			policy := c.policy(c.private())
			out, err := attack.run(t, c.daemonTree, policy)
			if err == nil {
				t.Fatalf("a command that may write a credential cache moved or exposed the daemon's state: %s", out)
			}
			if leaked(out) {
				t.Fatalf("the attack read private state even though it failed: %s", out)
			}
			if err := c.intact(); err != nil {
				t.Fatalf("the daemon's names no longer point at its state: %v", err)
			}
			if links, _ := filepath.Glob(filepath.Join(c.root, "leak*")); len(links) > 0 {
				t.Fatalf("a hard link to private state landed in the writable root: %v", links)
			}
			// The pins did not take the cache away from the same policy.
			if out, err := seatbeltRun(t, policy, "printf token > "+filepath.Join(c.cache, "after.json")); err != nil {
				t.Fatalf("the cache write must keep working beside the pins: %v (%s)", err, out)
			}
		})
	}
}

// A person whose SelfMind home, configuration or home directory is a symlink
// reaches the daemon through it. Replacing the link must stay denied in a policy
// that also carries an in-place cache write.
func TestSeatbeltInPlaceCredentialWritesDoNotReopenSymlinkedState(t *testing.T) {
	compose := func(build func(*testing.T) *linkCase) func(*testing.T) *linkCase {
		return func(t *testing.T) *linkCase {
			c := build(t)
			cache := filepath.Join(c.root, ".aws", "sso", "cache")
			records := filepath.Join(c.root, "runtime", "host-state")
			writeAll(t, map[string]string{
				filepath.Join(cache, "existing.json"): "{}",
				filepath.Join(records, "marker"):      "record",
			})
			c.decorate = func(policy *Policy) {
				policy.StateWritePaths = []string{cache}
				policy.FlatStatePaths = []string{cache}
				policy.Canary = NewCanary()
				policy.DenyWritePaths = []string{records}
			}
			return c
		}
	}
	for name, build := range map[string]func(*testing.T) *linkCase{
		"a symlinked data directory":           newDataDirLink,
		"a symlinked configuration file":       newConfigLink,
		"a symlinked directory above the data": newAncestorLink,
	} {
		t.Run(name, func(t *testing.T) {
			runLinkCase(t, compose(build))
			// The cache write works in the policy that denies the replacement.
			c := compose(build)(t)
			cache := filepath.Join(c.root, ".aws", "sso", "cache")
			if out, err := seatbeltRun(t, c.policy(true), "printf token > "+filepath.Join(cache, "t.json")); err != nil {
				t.Fatalf("the cache write must keep working beside the link pins: %v (%s)", err, out)
			}
		})
	}
	// A configuration copy planted beside the link is denied as before.
	c := compose(newConfigLink)(t)
	own := filepath.Join(c.root, "own")
	if err := os.WriteFile(own, []byte("evil: true"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.yaml.bak-1", "config.yaml.tmp"} {
		planted := filepath.Join(filepath.Dir(c.link), name)
		if out, err := seatbeltRun(t, c.policy(true), "ln -s "+own+" "+planted); err == nil {
			t.Fatalf("a command planted %s: %q", planted, out)
		}
	}
}

// A state write path is a writable subpath like a root, so the pins on the
// directories and links above private state must count it. Callers declare only
// flat caches, which hold neither; the policy builder is not told that, and must
// not depend on it.
func TestSeatbeltStateWritePathOverPrivateStateStaysPinned(t *testing.T) {
	skipWithoutSeatbelt(t)
	type shape struct {
		root, state, dataDir, gateway, db, decoy string
		private                                  PrivateState
	}
	build := func(t *testing.T) *shape {
		t.Helper()
		root := shortSocketDir(t)
		s := &shape{root: root, state: filepath.Join(root, "tool-cache")}
		s.dataDir = filepath.Join(s.state, "a", "b", ".selfmind")
		s.gateway = filepath.Join(s.dataDir, "gateway")
		s.db = filepath.Join(s.dataDir, "control.db")
		s.decoy = filepath.Join(s.state, "decoy")
		writeAll(t, map[string]string{
			s.db:                                   treeDBContent,
			filepath.Join(s.gateway, "state.json"): "{}",
			filepath.Join(s.state, "other.json"):   "{}",
			filepath.Join(s.decoy, "marker"):       "decoy",
		})
		s.private = PrivateState{Dirs: []string{s.gateway}, Files: []string{s.db}}
		return s
	}
	policy := func(s *shape, private PrivateState) Policy {
		return Policy{StateWritePaths: []string{s.state}, Private: private}
	}
	attacks := []struct {
		name string
		run  func(t *testing.T, s *shape, policy Policy) (string, error)
	}{
		{"rename the directory that holds the data directory", func(t *testing.T, s *shape, p Policy) (string, error) {
			moved := filepath.Join(s.state, "a2")
			return seatbeltRun(t, p, "mv "+filepath.Join(s.state, "a")+" "+moved+" && cat "+filepath.Join(moved, "b", ".selfmind", "control.db"))
		}},
		{"rename a directory between the state path and the data directory", func(t *testing.T, s *shape, p Policy) (string, error) {
			moved := filepath.Join(s.state, "a", "b2")
			return seatbeltRun(t, p, "mv "+filepath.Join(s.state, "a", "b")+" "+moved+" && cat "+filepath.Join(moved, ".selfmind", "control.db"))
		}},
		{"swap a directory above the data directory with a decoy", func(t *testing.T, s *shape, p Policy) (string, error) {
			return seatbeltRunHelper(t, p, "swap", filepath.Join(s.state, "a", "b"), s.decoy)
		}},
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			bare := build(t)
			if out, err := attack.run(t, bare, policy(bare, PrivateState{})); err != nil {
				t.Fatalf("the attack must work against an unprotected tree, or the test proves nothing: %v (%s)", err, out)
			}
			if _, err := os.Stat(bare.db); err == nil {
				t.Fatal("the attack moved nothing even without protection")
			}

			s := build(t)
			before, err := os.Lstat(filepath.Join(s.state, "a", "b"))
			if err != nil {
				t.Fatal(err)
			}
			out, err := attack.run(t, s, policy(s, s.private))
			if err == nil || strings.Contains(out, treeDBContent) {
				t.Fatalf("a directory above private state inside a state write path was moved: err=%v out=%q", err, out)
			}
			if now, statErr := os.Lstat(filepath.Join(s.state, "a", "b")); statErr != nil || !os.SameFile(before, now) {
				t.Fatalf("the directory above the data directory was replaced: %v", statErr)
			}
			if got, _ := os.ReadFile(s.db); string(got) != treeDBContent {
				t.Fatalf("the control store changed: %q", got)
			}
			// Ordinary work in the same state path is untouched.
			if out, err := seatbeltRun(t, policy(s, s.private), "printf x > "+filepath.Join(s.state, "other.json")+" && mkdir "+filepath.Join(s.state, "new")); err != nil {
				t.Fatalf("ordinary work in the state path must keep working: %v (%s)", err, out)
			}
		})
	}

	// A link on the way to private state that stands in the state path: removed
	// and replaced by a directory of the command's own, it would send the daemon
	// to a tree the command made.
	t.Run("a private symlink standing in the state path", func(t *testing.T) {
		makeLink := func(t *testing.T) (s *shape, link, store string) {
			t.Helper()
			s = build(t)
			store = filepath.Join(s.root, "store")
			writeAll(t, map[string]string{filepath.Join(store, "gateway", "state.json"): "{}"})
			link = filepath.Join(s.state, "dot")
			if err := os.Symlink(store, link); err != nil {
				t.Fatal(err)
			}
			s.private = PrivateState{Dirs: []string{filepath.Join(store, "gateway")}, Symlinks: []string{link}}
			return s, link, store
		}
		replace := func(link string) string { return "rm " + link + " && mkdir " + link }

		bare, link, _ := makeLink(t)
		open := policy(bare, PrivateState{Dirs: bare.private.Dirs})
		if out, err := seatbeltRun(t, open, replace(link)); err != nil {
			t.Fatalf("the replacement must work without the link named, or the test proves nothing: %v (%s)", err, out)
		}

		s, link, store := makeLink(t)
		if out, err := seatbeltRun(t, policy(s, s.private), replace(link)); err == nil {
			t.Fatalf("a link in a state write path was replaced: %q", out)
		}
		if target, err := os.Readlink(link); err != nil || target != store {
			t.Fatalf("the link now points at %q (%v), want %q", target, err, store)
		}
	})
}

// Seatbelt applies the last matching rule, so the private denials must come after
// the allowance a state write path grants. A private file inside the written
// subpath is the case that shows it.
func TestSeatbeltPrivateRulesOutrankStateWriteAllowances(t *testing.T) {
	skipWithoutSeatbelt(t)
	build := func(t *testing.T) (state, db, config string) {
		t.Helper()
		root := shortSocketDir(t)
		state = filepath.Join(root, "tool-cache")
		db = filepath.Join(state, "control.db")
		config = filepath.Join(state, "config.yaml")
		writeAll(t, map[string]string{db: treeDBContent, config: treeConfigContent, filepath.Join(state, "token.json"): "{}"})
		return state, db, config
	}
	state, db, config := build(t)
	private := PrivateState{Files: []string{db}, FilePrefixes: []string{config}}
	policy := Policy{StateWritePaths: []string{state}, FlatStatePaths: []string{state}, Private: private}
	for _, denied := range []struct{ name, script string }{
		{"writing the private file", "printf x >> " + db},
		{"removing the private file", "rm " + db},
		{"reading a private prefix", "cat " + config},
		{"planting beside a private prefix", "printf x > " + config + ".bak"},
	} {
		if out, err := seatbeltRun(t, policy, denied.script); err == nil {
			t.Errorf("%s inside a state write path must be denied: %q", denied.name, out)
		}
	}
	if got, err := os.ReadFile(db); err != nil || string(got) != treeDBContent {
		t.Fatalf("the private file changed: %q %v", got, err)
	}
	// The allowance itself is live in that policy.
	if out, err := seatbeltRun(t, policy, "printf token > "+filepath.Join(state, "token.json")); err != nil {
		t.Fatalf("the state write must work beside the private rules: %v (%s)", err, out)
	}
	// The constraint that must change the result: with the files not private the
	// same writes succeed.
	state, db, _ = build(t)
	open := Policy{StateWritePaths: []string{state}, FlatStatePaths: []string{state}}
	if out, err := seatbeltRun(t, open, "printf x >> "+db+" && rm "+db); err != nil {
		t.Fatalf("without the private rules the writes must succeed: %v (%s)", err, out)
	}
}
