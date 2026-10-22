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

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func listContains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// The configuration holds every provider key and chat token, so the daemon
// hides the file it actually loaded, wherever that is, together with the copies
// `selfmind config` leaves beside it, and the chat account credentials the
// adapters keep under the SelfMind home.
func TestDaemonPrivateStateCoversTheLoadedConfigurationAndAccountSecrets(t *testing.T) {
	home := resolvedTempDir(t)
	t.Setenv("HOME", home)
	dataDir := filepath.Join(home, ".selfmind", "data")
	addr := "127.0.0.1:8765"

	cfg := &config.Config{Path: filepath.Join(home, ".selfmind", "config.yaml")}
	cfg.Auth.CredentialsFile = filepath.Join(home, ".selfmind", "auth.json")
	private := daemonPrivateState(dataDir, cfg, LocalControlSocketPath(dataDir), addr)
	if !listContains(private.Files, cfg.Path) {
		t.Fatalf("the loaded configuration must be private: %v", private.Files)
	}
	for _, prefix := range []string{
		cfg.Path + ".",                             // config.yaml.tmp, config.yaml.bak-<time>
		cfg.Auth.CredentialsFile + ".",             // auth.json.<random>.tmp from the OAuth refresh, auth.json.lock
		filepath.Join(home, ".selfmind", ".auth-"), // the credential store's temporary files
	} {
		if !listContains(private.FilePrefixes, prefix) {
			t.Fatalf("private prefixes %v miss %s", private.FilePrefixes, prefix)
		}
	}
	if accounts := filepath.Join(home, ".selfmind", "weixin", "accounts"); !listContains(private.Dirs, accounts) {
		t.Fatalf("the chat account credentials must be private: %v", private.Dirs)
	}

	// The file the daemon loaded, not a default: --config and SELF_CONFIG move it.
	elsewhere := &config.Config{Path: filepath.Join(home, "srv", "prod.yaml")}
	moved := daemonPrivateState(dataDir, elsewhere, LocalControlSocketPath(dataDir), addr)
	if !listContains(moved.Files, elsewhere.Path) || !listContains(moved.FilePrefixes, elsewhere.Path+".") || listContains(moved.Files, cfg.Path) {
		t.Fatalf("a configuration at another path must be named, and only it: %v %v", moved.Files, moved.FilePrefixes)
	}

	// A configuration built in memory has no file: nothing empty is named.
	for _, state := range []sandbox.PrivateState{
		daemonPrivateState(dataDir, &config.Config{}, LocalControlSocketPath(dataDir), addr),
		daemonPrivateState(dataDir, nil, LocalControlSocketPath(dataDir), addr),
	} {
		for _, path := range append(append([]string(nil), state.Files...), state.FilePrefixes...) {
			if !filepath.IsAbs(path) {
				t.Fatalf("a path-less configuration must add no entry, got %q in %v %v", path, state.Files, state.FilePrefixes)
			}
		}
		if listContains(state.Files, "") || listContains(state.Files, ".") {
			t.Fatalf("empty entries: %v", state.Files)
		}
	}

	// Seatbelt matches the real path: a symlinked configuration is named by its
	// target, or the denial would guard a name nothing reads.
	real := filepath.Join(home, "real.yaml")
	if err := os.WriteFile(real, []byte("gateway:\n  token: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "linked.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	viaLink := daemonPrivateState(dataDir, &config.Config{Path: link}, LocalControlSocketPath(dataDir), addr)
	if !listContains(viaLink.Files, real) {
		t.Fatalf("a symlinked configuration must be named by its target: %v", viaLink.Files)
	}
}

// policyPath names a path the way seatbelt will see it, also for a path that
// does not exist yet: the deepest ancestor that exists is resolved and the rest
// is appended.
func TestPolicyPathResolvesTheDeepestExistingAncestor(t *testing.T) {
	root := resolvedTempDir(t)
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "present"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(link, "present"):                   filepath.Join(real, "present"),
		filepath.Join(link, "present", "missing"):        filepath.Join(real, "present", "missing"),
		filepath.Join(link, "missing", "deeper", "file"): filepath.Join(real, "missing", "deeper", "file"),
		filepath.Join(root, "absent", "x"):               filepath.Join(root, "absent", "x"),
	} {
		if got := policyPath(path); got != want {
			t.Fatalf("policyPath(%s) = %s, want %s", path, got, want)
		}
	}
}

// The /tmp fallback has the socket in a directory of its own, outside the data
// directory, that every daemon of the person shares. Through the daemon's own
// derivation and the real sandbox, a command that may write around it moves
// neither the socket nor its directory to reach the daemon by another name, and
// reaches no other socket in that directory either.
func TestDaemonPrivateStateConfinesARealCommandOnTheFallbackLayout(t *testing.T) {
	if !sandbox.SeatbeltAvailable() {
		t.Skip("enforced on the macOS Seatbelt host; bubblewrap is covered by argv tests")
	}
	if _, err := os.Stat("/usr/bin/curl"); err != nil {
		t.Skip("curl unavailable")
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
	dataDir := filepath.Join(home, ".selfmind", "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "gateway"), 0o700); err != nil {
		t.Fatal(err)
	}
	socketDir := filepath.Join(home, "fallback")
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	serve := func(path string) {
		listener, err := listenLocalControl(path)
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "daemon-reply") })}
		go server.Serve(listener)
		t.Cleanup(func() { server.Close() })
	}
	socket := filepath.Join(socketDir, "mine.sock")
	neighbour := filepath.Join(socketDir, "neighbour.sock")
	serve(socket)
	serve(neighbour)

	policy := sandbox.Policy{
		WritableRoots: []string{home},
		Network:       true,
		Private:       daemonPrivateState(dataDir, &config.Config{}, socket, "127.0.0.1:8765"),
	}
	run := func(script string) (string, error) {
		argv, ok := sandbox.WrapSeatbelt(policy, []string{"/bin/sh", "-c", script})
		if !ok {
			t.Fatal("the policy must wrap on a host with sandbox-exec")
		}
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		return string(out), err
	}
	curl := func(path string) string { return "/usr/bin/curl -sS -m 3 --unix-socket " + path + " http://daemon/" }
	for name, script := range map[string]string{
		"the socket":                        curl(socket),
		"another daemon's socket beside it": curl(neighbour),
		"the directory moved aside":         "mv " + socketDir + " " + socketDir + "-moved && " + curl(filepath.Join(socketDir+"-moved", "mine.sock")),
		"the socket renamed":                "mv " + socket + " " + filepath.Join(socketDir, "other.sock") + " && " + curl(filepath.Join(socketDir, "other.sock")),
		"the socket removed and replaced":   "rm " + socket,
	} {
		out, err := run(script)
		if err == nil || strings.Contains(out, "daemon-reply") {
			t.Fatalf("%s: a confined command reached or moved the daemon's socket: err=%v out=%q", name, err, out)
		}
	}
	for _, path := range []string{socket, neighbour} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s is gone after confined attempts: %v", path, err)
		}
	}
	if out, err := run("mkdir " + filepath.Join(home, "project") + " && printf x > " + filepath.Join(home, "project", "f") + " && cat " + filepath.Join(home, "project", "f")); err != nil || !strings.Contains(out, "x") {
		t.Fatalf("ordinary work must keep working: err=%v out=%q", err, out)
	}
}

// The daemon's own derivation against the real sandbox: a command whose
// writable root is the person's home, on the default layout, can read none of
// the daemon's secrets, cannot move the directories that hold them, and cannot
// reach the daemon's socket through any name.
func TestDaemonPrivateStateConfinesARealCommandOnTheDefaultLayout(t *testing.T) {
	if !sandbox.SeatbeltAvailable() {
		t.Skip("enforced on the macOS Seatbelt host; bubblewrap is covered by argv tests")
	}
	if _, err := os.Stat("/usr/bin/curl"); err != nil {
		t.Skip("curl unavailable")
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
	smhome := filepath.Join(home, ".selfmind")
	dataDir := filepath.Join(smhome, "data")
	account := filepath.Join(smhome, "weixin", "accounts", "bot.json")
	secrets := map[string]string{
		filepath.Join(smhome, "config.yaml"):            "provider-api-key-in-config",
		filepath.Join(smhome, "config.yaml.bak-1"):      "provider-api-key-in-backup",
		filepath.Join(smhome, "auth.json"):              "oauth-token-in-credentials",
		filepath.Join(dataDir, "control.db"):            "control-store-rows",
		account:                                         "weixin-bot-token",
		filepath.Join(dataDir, "gateway", "state.json"): "runtime-state",
	}
	for path, content := range secrets {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{filepath.Join(dataDir, "backups"), filepath.Join(smhome, "skills"), filepath.Join(home, "project")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "project", "README"), []byte("workspace text"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := LocalControlSocketPath(dataDir)
	listener, err := listenLocalControl(socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "daemon-reply") })}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	cfg := &config.Config{Path: filepath.Join(smhome, "config.yaml")}
	cfg.Auth.CredentialsFile = filepath.Join(smhome, "auth.json")
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
	leaks := func(out string) bool {
		if strings.Contains(out, "daemon-reply") {
			return true
		}
		for _, secret := range secrets {
			if strings.Contains(out, secret) {
				return true
			}
		}
		return false
	}
	curl := func(path string) string { return "/usr/bin/curl -sS -m 3 --unix-socket " + path + " http://daemon/" }

	for path := range secrets {
		if out, err := run("cat " + path); err == nil || leaks(out) {
			t.Fatalf("a confined cat of %s must fail: err=%v out=%q", path, err, out)
		}
	}
	if out, err := run(curl(socket)); err == nil || leaks(out) {
		t.Fatalf("the daemon's socket must be unreachable: err=%v out=%q", err, out)
	}
	// The OAuth refresh used to write `auth.json.tmp` and rename it into place: a
	// command that planted a symlink there got the refresh token. Nothing may be
	// created beside the credentials file, under any suffix.
	mine := filepath.Join(home, "project", "mine")
	if err := os.WriteFile(mine, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".tmp", ".lock", ".123.tmp"} {
		planted := filepath.Join(smhome, "auth.json"+suffix)
		if out, err := run("ln -s " + mine + " " + planted); err == nil {
			t.Fatalf("a confined command planted %s: %q", planted, out)
		}
		if _, err := os.Lstat(planted); err == nil {
			t.Fatalf("%s exists after a confined attempt", planted)
		}
	}
	// The reviewer's exploit: move the directory, then read and connect through
	// the new name. Each directory that holds a secret is pinned.
	for _, dir := range []string{smhome, dataDir, filepath.Dir(account), filepath.Dir(filepath.Dir(account))} {
		moved := dir + "-moved"
		out, err := run("mv " + dir + " " + moved + " && cat " + filepath.Join(moved, "config.yaml") + " && " + curl(filepath.Join(moved, "data", "gateway", "control.sock")))
		if err == nil || leaks(out) {
			t.Fatalf("moving %s must fail: err=%v out=%q", dir, err, out)
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("%s is gone after a confined move: %v", dir, statErr)
		}
	}
	// What is not the daemon's stays ordinary: the workspace, and the rest of the
	// SelfMind home beside the secrets.
	ordinary := "cat " + filepath.Join(home, "project", "README") +
		" && printf note > " + filepath.Join(home, "project", "note") +
		" && mkdir " + filepath.Join(smhome, "skills", "mine") +
		" && ls " + smhome
	if out, err := run(ordinary); err != nil || !strings.Contains(out, "workspace text") {
		t.Fatalf("ordinary work must keep working: err=%v out=%q", err, out)
	}
}
