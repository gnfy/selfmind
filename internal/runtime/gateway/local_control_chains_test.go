package gateway

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"selfmind/internal/platform/config"
	"selfmind/internal/tools/sandbox"
)

// A link can lead to another link. The daemon, the CLI and the next
// configuration load follow the whole chain, so replacing any link in it, or
// moving the directory one of them stands in, sends them to a tree a command
// made, while every private name still guards the old one.

func TestSymlinkNamesFollowsEveryLinkOnTheWay(t *testing.T) {
	home := resolvedTempDir(t)
	mk := func(path string) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	names := func(raw string) []string {
		out := symlinkNames(raw)
		sort.Strings(out)
		return out
	}

	t.Run("a link to a link to a directory", func(t *testing.T) {
		mk(filepath.Join(home, "real1", "sm", "data"))
		mk(filepath.Join(home, "dots1"))
		mustSymlink(t, filepath.Join(home, "real1", "sm"), filepath.Join(home, "dots1", "sm"))
		mustSymlink(t, filepath.Join(home, "dots1", "sm"), filepath.Join(home, "chain1"))
		got := names(filepath.Join(home, "chain1", "data"))
		want := []string{filepath.Join(home, "chain1"), filepath.Join(home, "dots1", "sm")}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("symlinkNames = %v, want %v", got, want)
		}
	})

	t.Run("a link into a synced folder that is itself a link", func(t *testing.T) {
		mk(filepath.Join(home, "Dropbox2", "dotfiles", "sm"))
		mustSymlink(t, filepath.Join(home, "Dropbox2", "dotfiles"), filepath.Join(home, "dotfiles2"))
		mustSymlink(t, filepath.Join(home, "dotfiles2", "sm"), filepath.Join(home, ".selfmind2"))
		got := names(filepath.Join(home, ".selfmind2", "config.yaml"))
		want := []string{filepath.Join(home, ".selfmind2"), filepath.Join(home, "dotfiles2")}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("symlinkNames = %v, want %v", got, want)
		}
	})

	t.Run("relative targets are read from the link's own directory", func(t *testing.T) {
		mk(filepath.Join(home, "elsewhere3", "sm"))
		mk(filepath.Join(home, "dotfiles3"))
		if err := os.Symlink(filepath.Join("..", "elsewhere3", "sm"), filepath.Join(home, "dotfiles3", "sm")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("dotfiles3", "sm"), filepath.Join(home, ".selfmind3")); err != nil {
			t.Fatal(err)
		}
		got := names(filepath.Join(home, ".selfmind3", "data"))
		want := []string{filepath.Join(home, ".selfmind3"), filepath.Join(home, "dotfiles3", "sm")}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("symlinkNames = %v, want %v", got, want)
		}
	})

	t.Run("a link whose target does not exist yet is still named", func(t *testing.T) {
		mustSymlink(t, filepath.Join(home, "not-yet"), filepath.Join(home, "dangling"))
		if got := names(filepath.Join(home, "dangling", "data")); len(got) != 1 || got[0] != filepath.Join(home, "dangling") {
			t.Fatalf("symlinkNames = %v", got)
		}
	})

	t.Run("a cycle and an endless chain end", func(t *testing.T) {
		mustSymlink(t, filepath.Join(home, "cycle-b"), filepath.Join(home, "cycle-a"))
		mustSymlink(t, filepath.Join(home, "cycle-a"), filepath.Join(home, "cycle-b"))
		for i := 0; i < 80; i++ {
			next := filepath.Join(home, fmt.Sprintf("long-%d", i+1))
			mustSymlink(t, next, filepath.Join(home, fmt.Sprintf("long-%d", i)))
		}
		done := make(chan []string, 2)
		go func() { done <- symlinkNames(filepath.Join(home, "cycle-a", "data")) }()
		go func() { done <- symlinkNames(filepath.Join(home, "long-0", "data")) }()
		for i := 0; i < 2; i++ {
			select {
			case got := <-done:
				if len(got) == 0 || len(got) > 41 {
					t.Fatalf("a cycle or an endless chain must be named within the depth cap, got %d names", len(got))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("symlinkNames did not end on a cycle or an endless chain")
			}
		}
		got := names(filepath.Join(home, "cycle-a", "x"))
		if len(got) != 2 {
			t.Fatalf("both links of a cycle are named once: %v", got)
		}
	})

	t.Run("no links, no names", func(t *testing.T) {
		mk(filepath.Join(home, "plain4", "data"))
		if got := symlinkNames(filepath.Join(home, "plain4", "data")); len(got) != 0 {
			t.Fatalf("a plain path names nothing: %v", got)
		}
	})
}

// chainLayout is a tree of links under a home directory and what a command would
// attack in it.
type chainLayout struct {
	dataDir, configPath string
	// links maps each link, as it is named on disk, to what it was created to
	// point at; dirs are the directories one of the links stands in.
	links map[string]string
	dirs  []string
}

func chainLayouts(home string) map[string]func(t *testing.T) chainLayout {
	mk := func(t *testing.T, paths ...string) {
		for _, path := range paths {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	file := func(t *testing.T, path, content string) {
		mk(t, filepath.Dir(path))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := func(t *testing.T, target, name string, links map[string]string) {
		mk(t, filepath.Dir(name))
		if err := os.Symlink(target, name); err != nil {
			t.Fatal(err)
		}
		links[name] = target
	}
	return map[string]func(t *testing.T) chainLayout{
		"a link to a link to the SelfMind home": func(t *testing.T) chainLayout {
			links := map[string]string{}
			real := filepath.Join(home, "real", "sm")
			mk(t, filepath.Join(real, "data", "gateway"))
			file(t, filepath.Join(real, "config.yaml"), "key: dotfile-secret\n")
			link(t, real, filepath.Join(home, "dots", "sm"), links)
			link(t, filepath.Join(home, "dots", "sm"), filepath.Join(home, ".selfmind"), links)
			return chainLayout{filepath.Join(home, ".selfmind", "data"), filepath.Join(home, ".selfmind", "config.yaml"), links, []string{filepath.Join(home, "dots")}}
		},
		"a chain of configuration links": func(t *testing.T) chainLayout {
			links := map[string]string{}
			mk(t, filepath.Join(home, ".selfmind", "data", "gateway"))
			file(t, filepath.Join(home, "real", "config.yaml"), "key: dotfile-secret\n")
			link(t, filepath.Join("..", "real", "config.yaml"), filepath.Join(home, "dots", "config.yaml"), links)
			link(t, filepath.Join("..", "dots", "config.yaml"), filepath.Join(home, ".selfmind", "config.yaml"), links)
			return chainLayout{filepath.Join(home, ".selfmind", "data"), filepath.Join(home, ".selfmind", "config.yaml"), links, []string{filepath.Join(home, "dots")}}
		},
		"a link into a synced folder that is itself a link": func(t *testing.T) chainLayout {
			links := map[string]string{}
			mk(t, filepath.Join(home, "Dropbox", "dotfiles", "sm", "data", "gateway"))
			file(t, filepath.Join(home, "Dropbox", "dotfiles", "sm", "config.yaml"), "key: dotfile-secret\n")
			link(t, filepath.Join(home, "Dropbox", "dotfiles"), filepath.Join(home, "dotfiles"), links)
			link(t, filepath.Join(home, "dotfiles", "sm"), filepath.Join(home, ".selfmind"), links)
			// The directory the links stand in is the home itself, which cannot be moved;
			// the synced folder holds the real state and is pinned as its ancestor.
			return chainLayout{filepath.Join(home, ".selfmind", "data"), filepath.Join(home, ".selfmind", "config.yaml"), links, nil}
		},
		"a relative chain": func(t *testing.T) chainLayout {
			links := map[string]string{}
			real := filepath.Join(home, "elsewhere", "sm")
			mk(t, filepath.Join(real, "data", "gateway"))
			file(t, filepath.Join(real, "config.yaml"), "key: dotfile-secret\n")
			link(t, filepath.Join("..", "elsewhere", "sm"), filepath.Join(home, "dotfiles", "sm"), links)
			link(t, filepath.Join("dotfiles", "sm"), filepath.Join(home, ".selfmind"), links)
			return chainLayout{filepath.Join(home, ".selfmind", "data"), filepath.Join(home, ".selfmind", "config.yaml"), links, []string{filepath.Join(home, "dotfiles")}}
		},
	}
}

func TestDaemonPrivateStateConfinesARealCommandAgainstEveryLinkInAChain(t *testing.T) {
	if !sandbox.SeatbeltAvailable() {
		t.Skip("enforced on the macOS Seatbelt host; bubblewrap cannot pin a link")
	}
	newHome := func(t *testing.T) string {
		t.Helper()
		home, err := os.MkdirTemp("/tmp", "smlc")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(home) })
		if home, err = filepath.EvalSymlinks(home); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		if err := os.MkdirAll(filepath.Join(home, "decoy"), 0o700); err != nil {
			t.Fatal(err)
		}
		return home
	}
	probe := newHome(t)
	for name := range chainLayouts(probe) {
		t.Run(name, func(t *testing.T) {
			// A layout whose links all stand in the home has no directory to move.
			hasDirs := len(chainLayouts(newHome(t))[name](t).dirs) > 0
			// attack runs every move against a fresh layout and reports what survived.
			type outcome struct {
				failed []string
				moved  []string
			}
			run := func(protected bool, script func(layout chainLayout, link string) string, subject func(layout chainLayout) []string) outcome {
				home := newHome(t)
				layout := chainLayouts(home)[name](t)
				state := daemonPrivateState(layout.dataDir, &config.Config{Path: layout.configPath}, LocalControlSocketPath(layout.dataDir), "127.0.0.1:8765")
				if !protected {
					state.Symlinks = nil
				}
				policy := sandbox.Policy{WritableRoots: []string{home}, Network: true, Private: state}
				var result outcome
				for _, target := range subject(layout) {
					argv, ok := sandbox.WrapSeatbelt(policy, []string{"/bin/sh", "-c", script(layout, target)})
					if !ok {
						t.Fatal("the policy must wrap on a host with sandbox-exec")
					}
					if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
						result.failed = append(result.failed, target)
					} else {
						_ = out
					}
				}
				for link, want := range layout.links {
					if got, err := os.Readlink(link); err != nil || got != want {
						result.moved = append(result.moved, link)
					}
				}
				for _, dir := range layout.dirs {
					if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
						result.moved = append(result.moved, dir)
					}
				}
				return result
			}
			links := func(layout chainLayout) []string {
				var out []string
				for link := range layout.links {
					out = append(out, link)
				}
				sort.Strings(out)
				return out
			}
			dirs := func(layout chainLayout) []string { return layout.dirs }
			decoy := func(layout chainLayout) string { return filepath.Join(filepath.Dir(layout.dataDir), "..", "decoy") }
			attacks := map[string]struct {
				script  func(layout chainLayout, target string) string
				subject func(chainLayout) []string
			}{
				"rename a link away": {func(_ chainLayout, link string) string { return "mv " + link + " " + link + "-moved" }, links},
				"remove a link":      {func(_ chainLayout, link string) string { return "rm " + link }, links},
				"repoint a link": {func(l chainLayout, link string) string {
					return "ln -sfn " + decoy(l) + " " + link
				}, links},
				"rename a directory a link stands in": {func(_ chainLayout, dir string) string { return "mv " + dir + " " + dir + "-moved" }, dirs},
			}
			for attackName, attack := range attacks {
				if attackName == "rename a directory a link stands in" && !hasDirs {
					continue
				}
				// Without the links named the same commands redirect the chain: the
				// denial below is the policy's, and it covers every link in it.
				bare := run(false, attack.script, attack.subject)
				if len(bare.moved) == 0 {
					t.Fatalf("%s: the attack changed nothing against an unprotected chain", attackName)
				}
				protected := run(true, attack.script, attack.subject)
				if len(protected.moved) != 0 {
					t.Fatalf("%s: a confined command redirected the chain: %v (denied: %v)", attackName, protected.moved, protected.failed)
				}
				if len(protected.failed) == 0 {
					t.Fatalf("%s: no attack was denied", attackName)
				}
			}
		})
	}
}
