package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The private rules name resolved real paths, and the pins name resolved
// ancestors. A person whose SelfMind home or configuration is a symlink
// (a dotfiles repository) reaches the daemon through the link, and so does the
// daemon: replace the link and the CLI dials a socket in a tree the command
// made, the daemon opens its files there, or the next configuration load reads
// the command's own file, while every private name still guards the old tree.

// linkCase is one symlink standing on a private path, and what a command would
// replace it with.
type linkCase struct {
	root    string
	link    string
	target  string
	decoy   string
	private PrivateState
	info    os.FileInfo
	// decorate adds to every policy the case builds; the composition tests use
	// it to give the command the rest of what a real policy carries.
	decorate func(*Policy)
}

func (c *linkCase) policy(protected bool) Policy {
	private := c.private
	if !protected {
		private.Symlinks = nil
	}
	policy := Policy{WritableRoots: []string{c.root}, Network: true, Private: private}
	if c.decorate != nil {
		c.decorate(&policy)
	}
	return policy
}

// intact reports how the link changed, judged from outside the sandbox.
func (c *linkCase) intact() error {
	now, err := os.Lstat(c.link)
	if err != nil {
		return fmt.Errorf("%s is gone: %w", c.link, err)
	}
	if now.Mode()&os.ModeSymlink == 0 || !os.SameFile(c.info, now) {
		return fmt.Errorf("%s was replaced", c.link)
	}
	if target, err := os.Readlink(c.link); err != nil || target != c.target {
		return fmt.Errorf("%s now points at %q (%v), want %q", c.link, target, err, c.target)
	}
	return nil
}

func writeAll(t *testing.T, files map[string]string) {
	t.Helper()
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func skipWithoutSeatbelt(t *testing.T) {
	t.Helper()
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
}

func finishLinkCase(t *testing.T, c *linkCase, readlinkTarget string) *linkCase {
	t.Helper()
	info, err := os.Lstat(c.link)
	if err != nil {
		t.Fatal(err)
	}
	c.info = info
	c.target = readlinkTarget
	return c
}

// ~/.selfmind is a link to a dotfiles tree: the data directory is reached
// through it.
func newDataDirLink(t *testing.T) *linkCase {
	skipWithoutSeatbelt(t)
	root := shortSocketDir(t)
	store := filepath.Join(root, "dotfiles", "selfmind")
	writeAll(t, map[string]string{
		filepath.Join(store, "data", "control.db"):            treeDBContent,
		filepath.Join(store, "data", "gateway", "state.json"): "{}",
		filepath.Join(store, "data", "backups", "b.json"):     "{}",
		filepath.Join(store, "config.yaml"):                   treeConfigContent,
		filepath.Join(root, "decoy", "marker"):                "decoy",
	})
	link := filepath.Join(root, ".selfmind")
	if err := os.Symlink(store, link); err != nil {
		t.Fatal(err)
	}
	c := &linkCase{root: root, link: link, decoy: filepath.Join(root, "decoy")}
	c.private = PrivateState{
		Dirs:     []string{filepath.Join(store, "data", "gateway"), filepath.Join(store, "data", "backups")},
		Files:    []string{filepath.Join(store, "data", "control.db"), filepath.Join(store, "config.yaml")},
		Symlinks: []string{link},
	}
	return finishLinkCase(t, c, store)
}

// config.yaml is a link to a file in a dotfiles repository.
func newConfigLink(t *testing.T) *linkCase {
	skipWithoutSeatbelt(t)
	root := shortSocketDir(t)
	target := filepath.Join(root, "dotfiles", "config.yaml")
	writeAll(t, map[string]string{
		target: treeConfigContent,
		filepath.Join(root, ".selfmind", "skills", "s.md"): "skill",
		filepath.Join(root, "decoy"):                       "decoy file",
	})
	link := filepath.Join(root, ".selfmind", "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	c := &linkCase{root: root, link: link, decoy: filepath.Join(root, "decoy")}
	c.private = PrivateState{
		Files:        []string{target},
		FilePrefixes: []string{link + ".", target + "."},
		Symlinks:     []string{link},
	}
	return finishLinkCase(t, c, target)
}

// ~/home is a link, and the data directory lies beneath it.
func newAncestorLink(t *testing.T) *linkCase {
	skipWithoutSeatbelt(t)
	root := shortSocketDir(t)
	real := filepath.Join(root, "volume", "home")
	writeAll(t, map[string]string{
		filepath.Join(real, ".selfmind", "data", "control.db"):            treeDBContent,
		filepath.Join(real, ".selfmind", "data", "gateway", "state.json"): "{}",
		filepath.Join(root, "decoy", "marker"):                            "decoy",
	})
	link := filepath.Join(root, "home")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	c := &linkCase{root: root, link: link, decoy: filepath.Join(root, "decoy")}
	c.private = PrivateState{
		Dirs:     []string{filepath.Join(real, ".selfmind", "data", "gateway")},
		Files:    []string{filepath.Join(real, ".selfmind", "data", "control.db")},
		Symlinks: []string{link},
	}
	return finishLinkCase(t, c, real)
}

type linkAttack struct {
	name string
	run  func(t *testing.T, c *linkCase, policy Policy) (string, error)
}

func linkAttacks() []linkAttack {
	return []linkAttack{
		{"rename the link away", func(t *testing.T, c *linkCase, policy Policy) (string, error) {
			return seatbeltRun(t, policy, "mv "+c.link+" "+c.link+"-moved")
		}},
		{"remove the link", func(t *testing.T, c *linkCase, policy Policy) (string, error) {
			return seatbeltRun(t, policy, "rm "+c.link)
		}},
		{"point the link at the command's own tree", func(t *testing.T, c *linkCase, policy Policy) (string, error) {
			return seatbeltRun(t, policy, "ln -sfn "+c.decoy+" "+c.link)
		}},
		{"rename another link over it", func(t *testing.T, c *linkCase, policy Policy) (string, error) {
			spare := filepath.Join(c.root, "spare-link")
			return seatbeltRun(t, policy, "ln -s "+c.decoy+" "+spare+" && mv -h "+spare+" "+c.link)
		}},
		{"swap it with a decoy", func(t *testing.T, c *linkCase, policy Policy) (string, error) {
			return seatbeltRunHelper(t, policy, "swap", c.link, c.decoy)
		}},
	}
}

func runLinkCase(t *testing.T, build func(*testing.T) *linkCase) {
	t.Helper()
	for _, attack := range linkAttacks() {
		t.Run(attack.name, func(t *testing.T) {
			// The constraint that must change the result: without the link listed
			// the same command replaces it, so the denial below is the policy's.
			bare := build(t)
			if out, err := attack.run(t, bare, bare.policy(false)); err != nil {
				t.Fatalf("the attack must work against an unprotected link, or the test proves nothing: %v (%s)", err, out)
			}
			if bare.intact() == nil {
				t.Fatal("the attack changed nothing even without protection")
			}

			c := build(t)
			out, err := attack.run(t, c, c.policy(true))
			if err == nil {
				t.Fatalf("a confined command replaced the link: %s", out)
			}
			if err := c.intact(); err != nil {
				t.Fatalf("the daemon's name for its state now leads somewhere else: %v", err)
			}
		})
	}
}

func TestSeatbeltSymlinkedDataDirectoryCannotBeRedirected(t *testing.T) {
	runLinkCase(t, newDataDirLink)
}

func TestSeatbeltSymlinkedConfigurationCannotBeReplaced(t *testing.T) {
	runLinkCase(t, newConfigLink)
	// Writing through the link reaches the private target and is denied by its
	// own rule; the target stays as it was.
	c := newConfigLink(t)
	own := filepath.Join(c.root, "own")
	if err := os.WriteFile(own, []byte("evil: true"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := seatbeltRun(t, c.policy(true), "cp "+own+" "+c.link); err == nil {
		t.Fatalf("a write through the link must be denied: %q", out)
	}
	if got, _ := os.ReadFile(c.target); string(got) != treeConfigContent {
		t.Fatalf("the configuration changed: %q", got)
	}
	// A backup or temporary copy is created beside the link, in its own
	// directory, not beside the target: both are private.
	for _, name := range []string{"config.yaml.bak-1", "config.yaml.tmp"} {
		planted := filepath.Join(filepath.Dir(c.link), name)
		if out, err := seatbeltRun(t, c.policy(true), "ln -s "+own+" "+planted); err == nil {
			t.Fatalf("a command planted %s: %q", planted, out)
		}
	}
	// Ordinary work beside the link is untouched.
	if out, err := seatbeltRun(t, c.policy(true), "cat "+filepath.Join(filepath.Dir(c.link), "skills", "s.md")+" && printf x > "+filepath.Join(filepath.Dir(c.link), "notes")); err != nil || !strings.Contains(out, "skill") {
		t.Fatalf("ordinary work beside the link must keep working: err=%v out=%q", err, out)
	}
}

func TestSeatbeltSymlinkedAncestorCannotBeRedirected(t *testing.T) {
	runLinkCase(t, newAncestorLink)
}
