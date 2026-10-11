package sandbox

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The daemon's own state must be out of reach of a confined command even
// though that command runs as the same OS user. These tests pin the rendered
// policy (platform-independent) and the real enforcement (darwin only).

func TestSeatbeltProfileDeniesPrivateStateLast(t *testing.T) {
	policy := Policy{
		WritableRoots: []string{"/home/u"},
		Network:       true,
		Private: PrivateState{
			Dirs:     []string{"/home/u/.selfmind/data/gateway"},
			Files:    []string{"/home/u/.selfmind/data/control.db"},
			Sockets:  []string{"/tmp/selfmind-501/abc.sock"},
			TCPPorts: []TCPPort{{Port: 8765}, {Port: 9000, AllHosts: true}},
		},
	}
	profile, params, ok := SeatbeltProfile(policy)
	if !ok {
		t.Fatal("a policy with private state must render")
	}
	for _, path := range []string{"/home/u/.selfmind/data/gateway", "/home/u/.selfmind/data/control.db", "/tmp/selfmind-501/abc.sock"} {
		if strings.Contains(profile, path) {
			t.Fatalf("private paths must travel as parameters, found %s in the policy", path)
		}
		found := false
		for _, param := range params {
			if strings.HasSuffix(param, "="+path) {
				found = true
			}
		}
		if !found {
			t.Fatalf("private path %s has no parameter binding: %v", path, params)
		}
	}
	for _, rule := range []string{
		`(deny file-read* file-write* (subpath (param "PRIVATE_DIR_0")))`,
		`(deny file-read* file-write* (literal (param "PRIVATE_FILE_0")))`,
		`(deny network-outbound (literal (param "PRIVATE_SOCKET_0")))`,
		`(deny network-outbound (remote ip "localhost:8765"))`,
		`(deny network-outbound (remote ip "*:9000"))`,
	} {
		if !strings.Contains(profile, rule) {
			t.Fatalf("missing private-state rule %s in:\n%s", rule, profile)
		}
	}
	// Seatbelt applies the last matching rule, so the denials must follow every
	// allow: a writable root that contains the data directory (a trusted home
	// workspace) and the shared network must not reopen them.
	lastAllow := strings.LastIndex(profile, "(allow ")
	firstPrivate := strings.Index(profile, "PRIVATE_")
	if firstPrivate < lastAllow {
		t.Fatalf("private-state denials must come after every allow rule")
	}
	for _, param := range params {
		key := strings.TrimPrefix(strings.SplitN(param, "=", 2)[0], "-D")
		if !strings.Contains(profile, `(param "`+key+`")`) {
			t.Fatalf("parameter %s is bound but never referenced", key)
		}
	}
}

func TestSeatbeltProfileRefusesUnsoundPrivateState(t *testing.T) {
	cases := map[string]PrivateState{
		"relative dir":    {Dirs: []string{"data/gateway"}},
		"unclean file":    {Files: []string{"/data/../data/control.db"}},
		"relative socket": {Sockets: []string{"gateway/control.sock"}},
		"zero port":       {TCPPorts: []TCPPort{{Port: 0}}},
		"port too large":  {TCPPorts: []TCPPort{{Port: 70000}}},
	}
	for name, private := range cases {
		t.Run(name, func(t *testing.T) {
			// Dropping an entry would run the command with the daemon's state
			// in reach; refusing degrades to an approval-gated host run.
			if _, _, ok := SeatbeltProfile(Policy{Private: private}); ok {
				t.Fatal("unsound private state must not render")
			}
		})
	}
}

func TestWrapArgvMasksPrivateStateAfterWritableRoots(t *testing.T) {
	argv := WrapArgv("/usr/bin/bwrap", Policy{
		WritableRoots: []string{"/home/u"},
		Private: PrivateState{
			Dirs:    []string{"/home/u/.selfmind/data/gateway"},
			Files:   []string{"/home/u/.selfmind/data/control.db"},
			Sockets: []string{"/srv/selfmind/control.sock"},
		},
	}, []string{"sh", "-c", "cat token"})
	joined := strings.Join(argv, " ")
	rootAt := strings.Index(joined, "--bind-try /home/u /home/u")
	dirAt := strings.Index(joined, "--tmpfs /home/u/.selfmind/data/gateway")
	fileAt := strings.Index(joined, "--ro-bind /dev/null /home/u/.selfmind/data/control.db")
	sockAt := strings.Index(joined, "--tmpfs /srv/selfmind")
	if rootAt < 0 || dirAt < 0 || fileAt < 0 || sockAt < 0 {
		t.Fatalf("missing private-state masks: %s", joined)
	}
	if dirAt < rootAt || fileAt < rootAt || sockAt < rootAt {
		t.Fatalf("masks must follow the writable roots they override: %s", joined)
	}
	if sep := indexOf(argv, "--"); sep < indexOf(argv, "/dev/null") || argv[len(argv)-1] != "cat token" {
		t.Fatalf("inner argv must stay last: %v", argv)
	}
}

func TestWrapArgvLeavesSocketUnderTmpToTheTmpReplacement(t *testing.T) {
	// /tmp is always a private tmpfs or the run scratch inside bwrap, so a
	// socket there is already unreachable; masking its parent would shadow the
	// scratch bind at /tmp.
	argv := WrapArgv("/usr/bin/bwrap", Policy{
		ScratchTmp: "/home/u/.selfmind/runtime/leases/l/tmp",
		Private:    PrivateState{Sockets: []string{"/tmp/selfmind-501/abc.sock"}},
	}, []string{"sh", "-c", "true"})
	if strings.Contains(strings.Join(argv, " "), "--tmpfs /tmp/selfmind-501") {
		t.Fatalf("a socket under /tmp must not mask a /tmp subtree: %v", argv)
	}
}

// realPath resolves macOS's /var -> /private/var style links the same way the
// daemon does before it hands paths to the policy.
func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestSeatbeltDeniesPrivateStateReads(t *testing.T) {
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	home := realPath(t, t.TempDir())
	gatewayDir := filepath.Join(home, ".selfmind", "data", "gateway")
	if err := os.MkdirAll(gatewayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(gatewayDir, "local_control.token")
	db := filepath.Join(home, ".selfmind", "data", "control.db")
	workspaceFile := filepath.Join(home, "project", "README")
	for path, content := range map[string]string{token: "secret-token", db: "SQLite", workspaceFile: "workspace text"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The workspace is the whole home directory: the widest writable root a
	// person can trust. Private state must stay closed inside it.
	policy := Policy{
		WritableRoots: []string{home},
		Network:       true,
		Private:       PrivateState{Dirs: []string{gatewayDir}, Files: []string{db}},
	}
	if out, err := seatbeltRun(t, policy, "cat "+token); err == nil || strings.Contains(out, "secret-token") {
		t.Fatalf("a confined cat of the daemon token must fail, got err=%v out=%q", err, out)
	}
	if out, err := seatbeltRun(t, policy, "cat "+db); err == nil || strings.Contains(out, "SQLite") {
		t.Fatalf("a confined read of control.db must fail, got err=%v out=%q", err, out)
	}
	if out, err := seatbeltRun(t, policy, "ls "+gatewayDir); err == nil {
		t.Fatalf("listing the daemon runtime directory must fail, got %q", out)
	}
	if out, err := seatbeltRun(t, policy, "echo forged > "+filepath.Join(gatewayDir, "forged")); err == nil {
		t.Fatalf("writing into the daemon runtime directory must fail even inside a writable root, got %q", out)
	}
	if out, err := seatbeltRun(t, policy, "cat "+workspaceFile); err != nil || !strings.Contains(out, "workspace text") {
		t.Fatalf("workspace reads must keep working: err=%v out=%q", err, out)
	}
	// The constraint that must change the result: the same read with nothing
	// declared private succeeds, so the denial above came from the policy.
	if out, err := seatbeltRun(t, Policy{WritableRoots: []string{home}}, "cat "+token); err != nil || !strings.Contains(out, "secret-token") {
		t.Fatalf("without private state the read must succeed: err=%v out=%q", err, out)
	}
}

// shortSocketDir returns a directory whose socket paths fit sun_path; test
// temp directories on macOS are too long for a Unix socket address.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "smsb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return realPath(t, dir)
}

func TestSeatbeltDeniesDaemonListenersWithSharedNetwork(t *testing.T) {
	if !SeatbeltAvailable() || runtime.GOOS != "darwin" {
		t.Skip("sandbox-exec unavailable")
	}
	if _, err := os.Stat("/usr/bin/curl"); err != nil {
		t.Skip("curl unavailable")
	}
	reached := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "reached") })
	listen := func(network, address string) net.Listener {
		listener, err := net.Listen(network, address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		go http.Serve(listener, reached)
		return listener
	}
	daemonTCP := listen("tcp", "127.0.0.1:0")
	otherTCP := listen("tcp", "127.0.0.1:0")
	socket := filepath.Join(shortSocketDir(t), "control.sock")
	listen("unix", socket)
	daemonPort := daemonTCP.Addr().(*net.TCPAddr).Port
	otherPort := otherTCP.Addr().(*net.TCPAddr).Port

	root := realPath(t, t.TempDir())
	policy := Policy{
		WritableRoots: []string{root},
		Network:       true,
		Private:       PrivateState{Sockets: []string{socket}, TCPPorts: []TCPPort{{Port: daemonPort}}},
	}
	curl := func(args string) string {
		return "/usr/bin/curl -sS -m 3 " + args
	}
	// curl's own connect failure (exit 7) proves the profile parsed and curl
	// ran; a profile syntax error would also fail, for the wrong reason.
	denied := func(policy Policy, target string) {
		t.Helper()
		out, err := seatbeltRun(t, policy, curl(target))
		if err == nil || strings.Contains(out, "reached") || !strings.Contains(out, "curl: (7)") {
			t.Fatalf("a confined client must not reach the daemon listener %s even with shared network: err=%v out=%q", target, err, out)
		}
	}
	for _, target := range []string{
		fmt.Sprintf("http://127.0.0.1:%d/", daemonPort),
		fmt.Sprintf("http://localhost:%d/", daemonPort),
		"--unix-socket " + socket + " http://daemon/",
	} {
		denied(policy, target)
	}
	// A daemon bound beyond loopback is denied on every address of its port.
	allHosts := policy
	allHosts.Private.TCPPorts = []TCPPort{{Port: daemonPort, AllHosts: true}}
	denied(allHosts, fmt.Sprintf("http://127.0.0.1:%d/", daemonPort))
	// Shared network still works for every other listener.
	if out, err := seatbeltRun(t, policy, curl(fmt.Sprintf("http://127.0.0.1:%d/", otherPort))); err != nil || !strings.Contains(out, "reached") {
		t.Fatalf("shared network must still reach other ports: err=%v out=%q", err, out)
	}
	// Without the private listeners the same client reaches the daemon port,
	// so the denial above is the policy's.
	unprotected := Policy{WritableRoots: []string{root}, Network: true}
	if out, err := seatbeltRun(t, unprotected, curl("--unix-socket "+socket+" http://daemon/")); err != nil || !strings.Contains(out, "reached") {
		t.Fatalf("without private state the socket must be reachable: err=%v out=%q", err, out)
	}
}

// Inside bubblewrap /tmp is a private tmpfs or the run's scratch, so nothing
// under the host's /tmp is visible there. A mask named under it would instead
// create a mount point inside that scratch on the host, so none is emitted for
// a directory or a file either; paths outside /tmp are still masked.
func TestWrapArgvEmitsNoMaskBeneathTmp(t *testing.T) {
	argv := WrapArgv("/usr/bin/bwrap", Policy{
		ScratchTmp: "/home/u/.selfmind/runtime/leases/l/tmp",
		Private: PrivateState{
			Dirs:    []string{"/tmp/selfmind-501", "/home/u/.selfmind/data/gateway"},
			Files:   []string{"/tmp/selfmind-501/state.db", "/home/u/.selfmind/data/control.db"},
			Sockets: []string{"/tmp/selfmind-501/abc.sock"},
		},
	}, []string{"sh", "-c", "true"})
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "/tmp/selfmind-501") {
		t.Fatalf("a mask beneath /tmp would write into the run scratch: %s", joined)
	}
	for _, want := range []string{"--tmpfs /home/u/.selfmind/data/gateway", "--ro-bind /dev/null /home/u/.selfmind/data/control.db"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("a private path outside /tmp must still be masked, missing %q: %s", want, joined)
		}
	}
}
