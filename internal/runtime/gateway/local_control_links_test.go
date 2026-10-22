package gateway

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/platform/config"
	"selfmind/internal/tools/sandbox"
)

// A dotfiles layout makes ~/.selfmind (or just config.yaml) a symlink. The
// daemon and the CLI reach private state through the link, while every private
// name is the real path it leads to. The daemon therefore also names the links,
// so a command cannot swap one for a tree of its own.

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonPrivateStateNamesTheSymlinksThatLeadToPrivateState(t *testing.T) {
	home := resolvedTempDir(t)
	t.Setenv("HOME", home)
	addr := "127.0.0.1:8765"
	mkdirs := func(paths ...string) {
		for _, path := range paths {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("a symlinked SelfMind home", func(t *testing.T) {
		store := filepath.Join(home, "dotfiles", "selfmind")
		mkdirs(filepath.Join(store, "data", "gateway"))
		if err := os.WriteFile(filepath.Join(store, "config.yaml"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(home, ".selfmind")
		mustSymlink(t, store, link)
		cfg := &config.Config{Path: filepath.Join(link, "config.yaml")}
		dataDir := filepath.Join(link, "data")
		private := daemonPrivateState(dataDir, cfg, LocalControlSocketPath(dataDir), addr)
		if !listContains(private.Symlinks, link) {
			t.Fatalf("the link the daemon and the CLI reach the data directory through must be named: %v", private.Symlinks)
		}
		if !listContains(private.Dirs, filepath.Join(store, "data", "gateway")) || !listContains(private.Files, filepath.Join(store, "config.yaml")) {
			t.Fatalf("the private names stay the real paths: %v %v", private.Dirs, private.Files)
		}
	})

	t.Run("a symlinked configuration file", func(t *testing.T) {
		target := filepath.Join(home, "dotfiles2", "config.yaml")
		mkdirs(filepath.Dir(target))
		if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(home, "sm2", "config.yaml")
		mustSymlink(t, target, link)
		private := daemonPrivateState(filepath.Join(home, "sm2", "data"), &config.Config{Path: link}, "/tmp/x.sock", addr)
		if !listContains(private.Symlinks, link) {
			t.Fatalf("the configuration link must be named: %v", private.Symlinks)
		}
		if !listContains(private.Files, target) {
			t.Fatalf("the target stays private: %v", private.Files)
		}
		// The daemon saves beside the link, not beside its target: both
		// neighbourhoods are private.
		for _, prefix := range []string{link + ".", target + "."} {
			if !listContains(private.FilePrefixes, prefix) {
				t.Fatalf("private prefixes %v miss %s", private.FilePrefixes, prefix)
			}
		}
	})

	t.Run("a symlinked ancestor and a link inside a link", func(t *testing.T) {
		real1 := filepath.Join(home, "real1")
		real2 := filepath.Join(home, "real2")
		mkdirs(filepath.Join(real2, "data", "gateway"))
		mustSymlink(t, real1, filepath.Join(home, "l1"))
		mkdirs(real1)
		mustSymlink(t, real2, filepath.Join(real1, "l2"))
		dataDir := filepath.Join(home, "l1", "l2", "data")
		private := daemonPrivateState(dataDir, &config.Config{}, LocalControlSocketPath(dataDir), addr)
		// l2 stands in real1, whatever name it is reached by.
		for _, want := range []string{filepath.Join(home, "l1"), filepath.Join(real1, "l2")} {
			if !listContains(private.Symlinks, want) {
				t.Fatalf("link %s must be named: %v", want, private.Symlinks)
			}
		}
	})

	t.Run("nothing to name without links", func(t *testing.T) {
		plain := filepath.Join(home, "plain")
		mkdirs(filepath.Join(plain, "data", "gateway"))
		private := daemonPrivateState(filepath.Join(plain, "data"), &config.Config{Path: filepath.Join(plain, "config.yaml")}, filepath.Join(plain, "data", "gateway", "control.sock"), addr)
		for _, link := range private.Symlinks {
			if strings.HasPrefix(link, plain) {
				t.Fatalf("no link stands on a plain layout: %v", private.Symlinks)
			}
		}
	})
}

// The reviewer's probes against the real sandbox, through the daemon's own
// derivation: a command whose root is the home replaces ~/.selfmind (a link
// into a dotfiles tree) and the configuration link.
func TestDaemonPrivateStateConfinesARealCommandOnADotfilesLayout(t *testing.T) {
	if !sandbox.SeatbeltAvailable() {
		t.Skip("enforced on the macOS Seatbelt host; bubblewrap cannot pin a link")
	}
	home, err := os.MkdirTemp("/tmp", "smlc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if home, err = filepath.EvalSymlinks(home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	store := filepath.Join(home, "dotfiles", "selfmind")
	for _, dir := range []string{filepath.Join(store, "data", "gateway"), filepath.Join(home, "decoy", "gateway")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configTarget := filepath.Join(home, "dotfiles", "config.yaml")
	if err := os.WriteFile(configTarget, []byte("delegation:\n  api_key: dotfile-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".selfmind")
	mustSymlink(t, store, link)
	configLink := filepath.Join(store, "config.yaml")
	mustSymlink(t, configTarget, configLink)
	dataDir := filepath.Join(link, "data")
	socket := LocalControlSocketPath(dataDir)
	listener, err := listenLocalControl(socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "daemon-reply") })}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	cfg := &config.Config{Path: filepath.Join(link, "config.yaml")}
	policy := sandbox.Policy{
		WritableRoots: []string{home},
		Network:       true,
		Private:       daemonPrivateState(dataDir, cfg, socket, "127.0.0.1:8765"),
	}
	run := func(script string) (string, error) {
		argv, ok := sandbox.WrapSeatbelt(policy, []string{"/bin/sh", "-c", script})
		if !ok {
			t.Fatal("the policy must wrap on a host with sandbox-exec")
		}
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		return string(out), err
	}
	decoy := filepath.Join(home, "decoy")
	own := filepath.Join(home, "own.yaml")
	if err := os.WriteFile(own, []byte("delegation:\n  api_key: attacker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"rename the SelfMind home link":             "mv " + link + " " + link + "-moved",
		"remove it":                                 "rm " + link,
		"repoint it at a tree of the command's own": "ln -sfn " + decoy + " " + link,
		"replace the configuration link":            "mv " + own + " " + configLink,
		"repoint the configuration link":            "ln -sf " + own + " " + configLink,
		"read the configuration through the link":   "cat " + configLink,
		"reach the socket through the link":         "/usr/bin/curl -sS -m 3 --unix-socket " + socket + " http://daemon/",
	} {
		out, err := run(script)
		if err == nil || strings.Contains(out, "dotfile-secret") || strings.Contains(out, "daemon-reply") {
			t.Fatalf("%s: err=%v out=%q", name, err, out)
		}
	}
	if got, err := os.Readlink(link); err != nil || got != store {
		t.Fatalf("the SelfMind home link now points at %q (%v)", got, err)
	}
	if got, err := os.Readlink(configLink); err != nil || got != configTarget {
		t.Fatalf("the configuration link now points at %q (%v)", got, err)
	}
	if got, _ := os.ReadFile(configTarget); !strings.Contains(string(got), "dotfile-secret") {
		t.Fatalf("the configuration changed: %q", got)
	}
}
