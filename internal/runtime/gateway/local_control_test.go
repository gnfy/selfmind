package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/platform/config"
	"selfmind/internal/tools/sandbox"
)

func shortDataDir(t *testing.T) string {
	t.Helper()
	// Short enough for the socket to live in the private runtime directory.
	dir, err := os.MkdirTemp("/tmp", "smlc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "data")
}

func TestLocalControlSocketPathPrefersThePrivateRuntimeDir(t *testing.T) {
	short := shortDataDir(t)
	if got, want := LocalControlSocketPath(short), filepath.Join(short, "gateway", "control.sock"); got != want {
		t.Fatalf("socket path = %q, want %q", got, want)
	}
	// A data directory too long for a socket address moves the socket to a
	// per-user directory derived from the data directory, so the daemon and
	// the CLI agree without talking.
	long := filepath.Join("/home/someone", strings.Repeat("deep/", 30), "data")
	got := LocalControlSocketPath(long)
	if len(got) > maxSocketPathLen || !strings.HasPrefix(got, fmt.Sprintf("/tmp/selfmind-%d/", os.Getuid())) {
		t.Fatalf("long data dir socket = %q", got)
	}
	if got != LocalControlSocketPath(long+"/") || got == LocalControlSocketPath(long+"2") {
		t.Fatal("the fallback must be a stable function of the data directory")
	}
	if ResolvePaths(short).LocalControlSocket != LocalControlSocketPath(short) {
		t.Fatal("the resolved paths must name the same socket")
	}
}

func TestListenLocalControlReplacesOnlyAStaleSocket(t *testing.T) {
	dataDir := shortDataDir(t)
	path := LocalControlSocketPath(dataDir)
	// An older install created the runtime directory world-readable.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A socket left by a daemon that crashed.
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	listener, err := listenLocalControl(path)
	if err != nil {
		t.Fatalf("a stale socket must be replaced: %v", err)
	}
	defer listener.Close()
	dirInfo, _ := os.Stat(filepath.Dir(path))
	sockInfo, _ := os.Stat(path)
	if dirInfo.Mode().Perm() != 0o700 || sockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("socket dir mode %o, socket mode %o", dirInfo.Mode().Perm(), sockInfo.Mode().Perm())
	}
	listener.Close()

	// Anything that is not a socket is never deleted.
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenLocalControl(path); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("a regular file at the socket path must be refused, got %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "keep" {
		t.Fatal("the regular file was replaced")
	}
}

func TestLocalControlClientReachesOnlyTheSocket(t *testing.T) {
	dataDir := shortDataDir(t)
	listener, err := listenLocalControl(LocalControlSocketPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "socket:"+r.URL.Path)
	})}
	go server.Serve(listener)
	defer server.Close()

	// The URL host is only a label; nothing listens on it.
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/v1/gateway/status", nil)
	response, err := LocalControlClient(dataDir).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "socket:/v1/gateway/status" {
		t.Fatalf("body = %q", body)
	}

	// A socket directory other users can enter is not trusted.
	if err := os.Chmod(filepath.Dir(LocalControlSocketPath(dataDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/health", nil)
	if _, err := LocalControlClient(dataDir).Do(request); err == nil || !strings.Contains(err.Error(), "accessible to other users") {
		t.Fatalf("an open socket directory must be refused, got %v", err)
	}
	// No daemon at all: the error names the socket, and nothing falls back
	// to TCP.
	request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/health", nil)
	if _, err := LocalControlClient(shortDataDir(t)).Do(request); err == nil {
		t.Fatal("a missing socket must fail")
	}
}

func TestRemoveLegacyLocalControlToken(t *testing.T) {
	dataDir := t.TempDir()
	token := filepath.Join(dataDir, "gateway", "local_control.token")
	if err := os.MkdirAll(filepath.Dir(token), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("old-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyLocalControlToken(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(token); !os.IsNotExist(err) {
		t.Fatalf("the retired token must be removed: %v", err)
	}
	if err := removeLegacyLocalControlToken(dataDir); err != nil {
		t.Fatalf("an absent token is not an error: %v", err)
	}
}

func TestDaemonPrivateStateFollowsTheDataLayout(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(home, ".selfmind", "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "gateway"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Auth.CredentialsFile = filepath.Join(home, ".selfmind", "auth.json")
	socket := LocalControlSocketPath(dataDir)
	private := daemonPrivateState(dataDir, cfg, socket, "127.0.0.1:8765")
	contains := func(list []string, want string) bool {
		for _, item := range list {
			if item == want {
				return true
			}
		}
		return false
	}
	for _, dir := range []string{filepath.Join(dataDir, "gateway"), filepath.Join(dataDir, "backups")} {
		if !contains(private.Dirs, dir) {
			t.Fatalf("private dirs %v miss %s", private.Dirs, dir)
		}
	}
	for _, file := range []string{filepath.Join(dataDir, "control.db"), filepath.Join(dataDir, "control.db-wal"), cfg.Auth.CredentialsFile} {
		if !contains(private.Files, file) {
			t.Fatalf("private files %v miss %s", private.Files, file)
		}
	}
	if len(private.Sockets) != 1 || len(private.TCPPorts) != 1 || private.TCPPorts[0].Port != 8765 || private.TCPPorts[0].AllHosts {
		t.Fatalf("listeners = %v %v", private.Sockets, private.TCPPorts)
	}
	if strings.HasPrefix(socket, filepath.Join(dataDir, "gateway")) && len(private.Dirs) != 2 {
		t.Fatalf("a socket inside the runtime dir needs no separate dir: %v", private.Dirs)
	}
	// A daemon bound beyond loopback is closed on every address.
	if wide := daemonPrivateState(dataDir, cfg, socket, "0.0.0.0:9000"); !wide.TCPPorts[0].AllHosts || wide.TCPPorts[0].Port != 9000 {
		t.Fatalf("non-loopback listener = %v", wide.TCPPorts)
	}
	// Paths are resolved the way seatbelt matches them: a symlinked data
	// directory is named by its target.
	link := filepath.Join(home, "linked-data")
	if err := os.Symlink(dataDir, link); err != nil {
		t.Fatal(err)
	}
	if viaLink := daemonPrivateState(link, cfg, socket, "127.0.0.1:8765"); !contains(viaLink.Dirs, filepath.Join(dataDir, "gateway")) {
		t.Fatalf("symlinked data dir must resolve to its target: %v", viaLink.Dirs)
	}
}

// A data directory too deep for a socket address moves the socket under /tmp,
// into a directory shared by every daemon of the person. That directory is
// private as a whole: its socket, its siblings' sockets, and the names above
// them cannot be reached or moved by a confined command. Inside bubblewrap /tmp
// is the run's own scratch, bound over it, so nothing there is masked, which
// the sandbox package guarantees for every private entry beneath /tmp.
func TestDaemonPrivateStateListsTheFallbackSocketDirectoryButNeverMasksItUnderTmp(t *testing.T) {
	long := filepath.Join("/home/someone", strings.Repeat("deep/", 30), "data")
	socket := LocalControlSocketPath(long)
	if !strings.HasPrefix(socket, "/tmp/") {
		t.Fatalf("expected the fallback socket under /tmp, got %q", socket)
	}
	private := daemonPrivateState(long, &config.Config{}, socket, "127.0.0.1:8765")
	socketDir := policyPath(filepath.Dir(socket))
	listed := false
	for _, dir := range private.Dirs {
		listed = listed || dir == socketDir
	}
	if !listed {
		t.Fatalf("the fallback socket directory %s must be private: %v", socketDir, private.Dirs)
	}
	if len(private.Sockets) != 1 || !strings.HasSuffix(private.Sockets[0], filepath.Base(socket)) {
		t.Fatalf("the fallback socket itself must stay private: %v", private.Sockets)
	}
	// The daemon's state, through the real bubblewrap argv builder: the run
	// scratch is bound at /tmp, so a mask named beneath it would create a mount
	// point inside that scratch on the host.
	// bubblewrap only runs where /tmp is a real directory; macOS resolves it to
	// /private/tmp, so name the paths as Linux would.
	linux := func(paths []string) []string {
		out := make([]string, len(paths))
		for i, path := range paths {
			out[i] = strings.TrimPrefix(path, "/private")
			if !strings.HasPrefix(path, "/private/tmp/") {
				out[i] = path
			}
		}
		return out
	}
	argv := strings.Join(sandbox.WrapArgv("/usr/bin/bwrap", sandbox.Policy{
		ScratchTmp: "/home/someone/.selfmind/runtime/leases/l/tmp",
		Private:    sandbox.PrivateState{Dirs: linux(private.Dirs), Files: linux(private.Files), Sockets: linux(private.Sockets)},
	}, []string{"true"}), " ")
	for _, mask := range []string{"--tmpfs /tmp/selfmind-", "--ro-bind /dev/null /tmp/"} {
		if strings.Contains(argv, mask) {
			t.Fatalf("a mask beneath /tmp would write into the run scratch: %s", argv)
		}
	}

	// The variant that must still mask: a socket in a directory outside both the
	// runtime directory and /tmp (a path this version never produces, but the
	// derivation must not depend on that).
	outside := daemonPrivateState(t.TempDir(), &config.Config{}, "/srv/selfmind/control.sock", "127.0.0.1:8765")
	found := false
	for _, dir := range outside.Dirs {
		found = found || dir == policyPath("/srv/selfmind")
	}
	if !found {
		t.Fatalf("a socket directory outside the runtime dir and /tmp must be masked: %v", outside.Dirs)
	}
}
