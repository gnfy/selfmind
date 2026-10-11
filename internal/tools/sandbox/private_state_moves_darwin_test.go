package sandbox

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// A private name protects the daemon's state only while it keeps naming that
// state. Seatbelt denies by path, so a confined command that may write the
// data directory's parent could move the directory aside and then read and
// connect through the new name, which no rule mentions. These tests run the
// moves under the real sandbox-exec on scratch trees.

const confinedHelperEnv = "SELFMIND_SANDBOX_CONFINED_HELPER"

// TestConfinedHelper is the program the attacks below run inside the sandbox
// when they need a system call no shell utility makes. It does nothing unless a
// test re-executes this binary for it.
func TestConfinedHelper(t *testing.T) {
	if os.Getenv(confinedHelperEnv) != "1" {
		t.Skip("runs only as the confined helper of the filesystem attack tests")
	}
	args := flag.Args()
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "helper:", err)
		os.Exit(1)
	}
	switch {
	case len(args) == 3 && args[0] == "swap":
		if err := unix.RenamexNp(args[1], args[2], unix.RENAME_SWAP); err != nil {
			fail(err)
		}
	default:
		fail(fmt.Errorf("unknown helper operation %q", args))
	}
	os.Exit(0)
}

// seatbeltRunHelper runs one helper operation of this test binary under policy.
func seatbeltRunHelper(t *testing.T, policy Policy, operation ...string) (string, error) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := append([]string{self, "-test.run=^TestConfinedHelper$", "--"}, operation...)
	wrapped, ok := WrapSeatbelt(policy, argv)
	if !ok {
		t.Fatal("policy must wrap on a host with sandbox-exec")
	}
	cmd := exec.Command(wrapped[0], wrapped[1:]...)
	cmd.Env = append(os.Environ(), confinedHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// daemonTree is a data directory laid out like the daemon's inside a person's
// home, which is also the one writable root of the confined command: the
// widest authority a person trusts, and the one that holds the data directory.
type daemonTree struct {
	root     string
	dataDir  string
	gateway  string
	backups  string
	db       string
	config   string
	socket   string
	skills   string
	decoy    string
	project  string
	identity map[string]os.FileInfo
}

const (
	treeDBContent     = "SQLite format 3"
	treeConfigContent = "cfg-secret"
)

// newDaemonTree builds the tree under a short /tmp directory, so its socket
// fits sun_path, with the data directory at root/<layout...>.
func newDaemonTree(t *testing.T, layout ...string) *daemonTree {
	t.Helper()
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	if _, err := os.Stat("/usr/bin/curl"); err != nil {
		t.Skip("curl unavailable")
	}
	root := shortSocketDir(t)
	dataDir := filepath.Join(append([]string{root}, layout...)...)
	tree := &daemonTree{
		root:     root,
		dataDir:  dataDir,
		gateway:  filepath.Join(dataDir, "gateway"),
		backups:  filepath.Join(dataDir, "backups"),
		db:       filepath.Join(dataDir, "control.db"),
		config:   filepath.Join(dataDir, "config.yaml"),
		skills:   filepath.Join(dataDir, "skills"),
		decoy:    filepath.Join(root, "decoy"),
		project:  filepath.Join(root, "project"),
		identity: map[string]os.FileInfo{},
	}
	tree.socket = filepath.Join(tree.gateway, "control.sock")
	for _, dir := range []string{tree.gateway, tree.backups, tree.skills, tree.decoy, tree.project} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		tree.db:                    treeDBContent,
		tree.config:                treeConfigContent,
		tree.config + ".bak-1":     treeConfigContent + "-backup",
		tree.config + ".tmp":       treeConfigContent + "-temporary",
		tree.config + "-unrelated": "unrelated",
		filepath.Join(tree.gateway, "state.json"): "{}",
		filepath.Join(tree.project, "README"):     "workspace text",
		filepath.Join(tree.decoy, "marker"):       "decoy",
		filepath.Join(tree.skills, "skill.md"):    "skill",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	serveSocket(t, tree.socket)
	// Every directory from the root down, and every private entry: their
	// identity (the inode behind the name) must survive whatever is attempted.
	guarded := []string{tree.dataDir, tree.gateway, tree.backups, tree.db, tree.config, tree.socket}
	for dir := filepath.Dir(tree.dataDir); dir != tree.root; dir = filepath.Dir(dir) {
		guarded = append(guarded, dir)
	}
	for _, path := range guarded {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		tree.identity[path] = info
	}
	return tree
}

// serveSocket answers every request on a Unix socket with "reached".
func serveSocket(t *testing.T, path string) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "reached") }))
}

func (tree *daemonTree) private() PrivateState {
	return PrivateState{
		Dirs:         []string{tree.gateway, tree.backups},
		Files:        []string{tree.db, tree.config},
		FilePrefixes: []string{tree.config + "."},
		Sockets:      []string{tree.socket},
	}
}

// policy confines a command to the whole home with the shared network, so the
// private rules are the only thing between it and the daemon.
func (tree *daemonTree) policy(private PrivateState) Policy {
	return Policy{WritableRoots: []string{tree.root}, Network: true, Private: private}
}

// intact reports the first change an attack made to what the daemon's names
// point at, judged from outside the sandbox.
func (tree *daemonTree) intact() error {
	for path, before := range tree.identity {
		now, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("%s is gone: %w", path, err)
		}
		if !os.SameFile(before, now) || now.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s no longer names the same file", path)
		}
	}
	for path, want := range map[string]string{tree.db: treeDBContent, tree.config: treeConfigContent} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			return fmt.Errorf("%s changed: %q %v", path, got, err)
		}
	}
	return nil
}

func curlUnix(socket string) string {
	return "/usr/bin/curl -sS -m 3 --unix-socket " + socket + " http://daemon/"
}

// leaked reports whether an attack's output carries anything private: the
// control store, the configuration, or a reply from the daemon's listener.
func leaked(out string) bool {
	return strings.Contains(out, treeDBContent) || strings.Contains(out, treeConfigContent) || strings.Contains(out, "reached")
}

type moveAttack struct {
	name   string
	layout []string
	// run executes the attack inside the sandbox.
	run func(t *testing.T, tree *daemonTree, policy Policy) (string, error)
	// effect reports whether the attack changed anything visible from outside
	// the sandbox. Unset means the daemon's names no longer point at its state.
	effect func(tree *daemonTree) bool
}

func shell(script func(tree *daemonTree) string) func(*testing.T, *daemonTree, Policy) (string, error) {
	return func(t *testing.T, tree *daemonTree, policy Policy) (string, error) {
		return seatbeltRun(t, policy, script(tree))
	}
}

func moveAttacks() []moveAttack {
	return []moveAttack{
		{
			name: "rename the data directory, then read and connect at the new name",
			run: shell(func(tree *daemonTree) string {
				moved := filepath.Join(tree.root, ".moved")
				return "mv " + tree.dataDir + " " + moved +
					" && cat " + filepath.Join(moved, "control.db") +
					" && cat " + filepath.Join(moved, "config.yaml") +
					" && " + curlUnix(filepath.Join(moved, "gateway", "control.sock"))
			}),
		},
		{
			name: "rename the data directory and leave a symlink at its old name",
			run: shell(func(tree *daemonTree) string {
				moved := filepath.Join(tree.root, ".moved")
				return "mv " + tree.dataDir + " " + moved + " && ln -s " + moved + " " + tree.dataDir +
					" && cat " + filepath.Join(tree.dataDir, "control.db")
			}),
		},
		{
			name: "swap the data directory with a decoy",
			run: func(t *testing.T, tree *daemonTree, policy Policy) (string, error) {
				return seatbeltRunHelper(t, policy, "swap", tree.dataDir, tree.decoy)
			},
		},
		{
			name: "remove the data directory",
			run:  shell(func(tree *daemonTree) string { return "rm -rf " + tree.dataDir }),
		},
		{
			name:   "rename the directory that holds the data directory",
			layout: []string{"a", "b", ".selfmind"},
			run: shell(func(tree *daemonTree) string {
				moved := filepath.Join(tree.root, "a2")
				return "mv " + filepath.Join(tree.root, "a") + " " + moved +
					" && cat " + filepath.Join(moved, "b", ".selfmind", "control.db")
			}),
		},
		{
			name:   "rename a directory between the root and the data directory",
			layout: []string{"a", "b", ".selfmind"},
			run: shell(func(tree *daemonTree) string {
				moved := filepath.Join(tree.root, "a", "b2")
				return "mv " + filepath.Join(tree.root, "a", "b") + " " + moved +
					" && cat " + filepath.Join(moved, ".selfmind", "control.db")
			}),
		},
		{
			name:   "swap a directory above the data directory with a decoy",
			layout: []string{"a", "b", ".selfmind"},
			run: func(t *testing.T, tree *daemonTree, policy Policy) (string, error) {
				return seatbeltRunHelper(t, policy, "swap", filepath.Join(tree.root, "a", "b"), tree.decoy)
			},
		},
		{
			name: "rename private entries by name",
			run: shell(func(tree *daemonTree) string {
				return "mv " + tree.db + " " + filepath.Join(tree.root, "stolen.db") +
					" && mv " + tree.gateway + " " + filepath.Join(tree.root, "stolen-gateway")
			}),
		},
		{
			name: "rename a decoy over a private file",
			run: shell(func(tree *daemonTree) string {
				decoy := filepath.Join(tree.root, "decoy.db")
				return "echo forged > " + decoy + " && mv " + decoy + " " + tree.db
			}),
		},
		{
			name: "hard-link private files elsewhere",
			run: shell(func(tree *daemonTree) string {
				leak := filepath.Join(tree.root, "leak")
				return "ln " + tree.db + " " + leak + " && cat " + leak
			}),
			effect: func(tree *daemonTree) bool {
				_, err := os.Lstat(filepath.Join(tree.root, "leak"))
				return err == nil
			},
		},
		{
			name: "hard-link a file inside a private directory elsewhere",
			run: shell(func(tree *daemonTree) string {
				leak := filepath.Join(tree.root, "leak-state")
				return "ln " + filepath.Join(tree.gateway, "state.json") + " " + leak
			}),
			effect: func(tree *daemonTree) bool {
				_, err := os.Lstat(filepath.Join(tree.root, "leak-state"))
				return err == nil
			},
		},
	}
}

func TestSeatbeltPrivateNamesSurviveMovesOfTheirAncestors(t *testing.T) {
	for _, attack := range moveAttacks() {
		t.Run(attack.name, func(t *testing.T) {
			layout := attack.layout
			if layout == nil {
				layout = []string{".selfmind"}
			}

			// The constraint that must change the result: with nothing declared
			// private the same command completes and changes what the daemon's
			// names point at, so the protection below is the policy's.
			bare := newDaemonTree(t, layout...)
			if out, err := attack.run(t, bare, bare.policy(PrivateState{})); err != nil {
				t.Fatalf("the attack must work against an unprotected tree, or the test proves nothing: %v (%s)", err, out)
			}
			effect := attack.effect
			if effect == nil {
				effect = func(tree *daemonTree) bool { return tree.intact() != nil }
			}
			if !effect(bare) {
				t.Fatal("the attack changed nothing even without protection")
			}

			tree := newDaemonTree(t, layout...)
			out, err := attack.run(t, tree, tree.policy(tree.private()))
			if err == nil {
				t.Fatalf("a confined command moved or exposed the daemon's state: %s", out)
			}
			if leaked(out) {
				t.Fatalf("the attack read private state even though it failed: %s", out)
			}
			if err := tree.intact(); err != nil {
				t.Fatalf("the daemon's names no longer point at its state: %v", err)
			}
			if links, _ := filepath.Glob(filepath.Join(tree.root, "leak*")); len(links) > 0 {
				t.Fatalf("a hard link to private state landed in the writable root: %v", links)
			}
		})
	}
}

// Pinning the daemon's ancestors must not turn the writable root read-only
// around them: ordinary files in the root, in the data directory beside the
// private entries, and the root's other directories keep every operation.
func TestSeatbeltAncestorPinsLeaveOrdinaryWorkAlone(t *testing.T) {
	tree := newDaemonTree(t, ".selfmind")
	policy := tree.policy(tree.private())
	script := strings.Join([]string{
		"printf one > " + filepath.Join(tree.project, "new.txt"),
		"mv " + filepath.Join(tree.project, "new.txt") + " " + filepath.Join(tree.project, "renamed.txt"),
		"rm " + filepath.Join(tree.project, "renamed.txt"),
		"mv " + tree.project + " " + filepath.Join(tree.root, "project2"),
		"mv " + tree.decoy + " " + filepath.Join(tree.root, "decoy2"),
		"mkdir " + filepath.Join(tree.dataDir, "notes"),
		"printf note > " + filepath.Join(tree.dataDir, "notes", "n.txt"),
		"mv " + tree.skills + " " + filepath.Join(tree.dataDir, "skills2"),
		"cat " + filepath.Join(tree.dataDir, "skills2", "skill.md"),
		"ls " + tree.dataDir,
		"echo ordinary-work-ok",
	}, " && ")
	out, err := seatbeltRun(t, policy, script)
	if err != nil || !strings.Contains(out, "ordinary-work-ok") {
		t.Fatalf("ordinary work in the same writable root must keep working: err=%v out=%q", err, out)
	}
	if err := tree.intact(); err != nil {
		t.Fatal(err)
	}
}

// The configuration holds every provider key and chat token, and `selfmind
// config` leaves timestamped copies of it beside the file. A confined command
// must read none of them, whether they existed when the policy was built or
// were written after, and must not plant a copy either.
func TestSeatbeltHidesConfigurationBackupsAndTemporaryCopies(t *testing.T) {
	tree := newDaemonTree(t, ".selfmind")
	policy := tree.policy(tree.private())
	for _, name := range []string{"config.yaml", "config.yaml.bak-1", "config.yaml.tmp"} {
		path := filepath.Join(tree.dataDir, name)
		if out, err := seatbeltRun(t, policy, "cat "+path); err == nil || leaked(out) {
			t.Fatalf("a confined cat of %s must fail: err=%v out=%q", name, err, out)
		}
	}
	// A backup written after the daemon built its policy, from outside the
	// sandbox the way `selfmind config set` would.
	later := filepath.Join(tree.dataDir, "config.yaml.bak-2")
	if err := os.WriteFile(later, []byte(treeConfigContent+"-later"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := seatbeltRun(t, policy, "cat "+later); err == nil || leaked(out) {
		t.Fatalf("a backup written after the policy must be hidden too: err=%v out=%q", err, out)
	}
	if out, err := seatbeltRun(t, policy, "printf planted > "+filepath.Join(tree.dataDir, "config.yaml.bak-3")); err == nil {
		t.Fatalf("a confined command must not plant a copy beside the configuration: %q", out)
	}
	// What does not start with the prefix stays readable.
	for _, name := range []string{"config.yaml-unrelated", filepath.Join("skills", "skill.md")} {
		if out, err := seatbeltRun(t, policy, "cat "+filepath.Join(tree.dataDir, name)); err != nil {
			t.Fatalf("%s is not private and must stay readable: err=%v out=%q", name, err, out)
		}
	}
	// The constraint that must change the result: with only the file itself
	// listed, its backup is readable.
	narrow := tree.private()
	narrow.FilePrefixes = nil
	if out, err := seatbeltRun(t, tree.policy(narrow), "cat "+filepath.Join(tree.dataDir, "config.yaml.bak-1")); err != nil || !leaked(out) {
		t.Fatalf("without the prefix the backup must be readable: err=%v out=%q", err, out)
	}
}

// Another daemon's socket in a private directory is unreachable although only
// the directory is listed: the fallback directory holds one socket per data
// directory, and a confined command must not talk to any of them.
func TestSeatbeltSocketsInsideAPrivateDirectoryAreUnreachable(t *testing.T) {
	tree := newDaemonTree(t, ".selfmind")
	// The socket's directory is private; the socket itself is not listed.
	policy := tree.policy(PrivateState{Dirs: []string{tree.gateway}})
	out, err := seatbeltRun(t, policy, curlUnix(tree.socket))
	if err == nil || leaked(out) {
		t.Fatalf("a socket in a private directory must be unreachable: err=%v out=%q", err, out)
	}
	// The constraint that must change the result: without the directory the
	// same socket answers.
	if out, err := seatbeltRun(t, tree.policy(PrivateState{}), curlUnix(tree.socket)); err != nil || !strings.Contains(out, "reached") {
		t.Fatalf("without the private directory the socket must answer: err=%v out=%q", err, out)
	}
}

// The listener's socket sits outside the data directory when the data
// directory's path is too long for a socket address. A confined command that
// may write that directory must not move the socket, or its directory, to a
// name no rule mentions.
func TestSeatbeltSocketOutsideTheDataDirectorySurvivesMoves(t *testing.T) {
	type socketAttack struct {
		name string
		run  func(t *testing.T, dir, sock string, policy Policy) (string, error)
	}
	attacks := []socketAttack{
		{"rename the socket directory, then connect at the new name", func(t *testing.T, dir, sock string, policy Policy) (string, error) {
			moved := filepath.Join(filepath.Dir(dir), "moved")
			return seatbeltRun(t, policy, "mv "+dir+" "+moved+" && "+curlUnix(filepath.Join(moved, filepath.Base(sock))))
		}},
		{"rename the socket, then connect at the new name", func(t *testing.T, dir, sock string, policy Policy) (string, error) {
			moved := filepath.Join(dir, "other.sock")
			return seatbeltRun(t, policy, "mv "+sock+" "+moved+" && "+curlUnix(moved))
		}},
		{"remove the socket", func(t *testing.T, dir, sock string, policy Policy) (string, error) {
			return seatbeltRun(t, policy, "rm "+sock)
		}},
		{"hard-link the socket, then connect through the link", func(t *testing.T, dir, sock string, policy Policy) (string, error) {
			link := filepath.Join(filepath.Dir(dir), "alias.sock")
			return seatbeltRun(t, policy, "ln "+sock+" "+link+" && "+curlUnix(link))
		}},
		{"swap the socket directory with a decoy", func(t *testing.T, dir, sock string, policy Policy) (string, error) {
			decoy := filepath.Join(filepath.Dir(dir), "decoy")
			if err := os.Mkdir(decoy, 0o700); err != nil {
				t.Fatal(err)
			}
			return seatbeltRunHelper(t, policy, "swap", dir, decoy)
		}},
	}
	build := func(t *testing.T) (root, dir, sock string) {
		t.Helper()
		if !SeatbeltAvailable() {
			t.Skip("sandbox-exec unavailable")
		}
		if _, err := os.Stat("/usr/bin/curl"); err != nil {
			t.Skip("curl unavailable")
		}
		root = shortSocketDir(t)
		dir = filepath.Join(root, "selfmind-501")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		sock = filepath.Join(dir, "daemon.sock")
		serveSocket(t, sock)
		return root, dir, sock
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			// The shape of the daemon's /tmp fallback: only the socket is named.
			root, dir, sock := build(t)
			policy := Policy{WritableRoots: []string{root}, Network: true, Private: PrivateState{Sockets: []string{sock}}}
			beforeDir, err := os.Lstat(dir)
			if err != nil {
				t.Fatal(err)
			}
			beforeSock, err := os.Lstat(sock)
			if err != nil {
				t.Fatal(err)
			}
			out, err := attack.run(t, dir, sock, policy)
			t.Logf("confined attempt: err=%v out=%q", err, out)
			if err == nil || leaked(out) {
				t.Fatalf("a confined command reached the daemon's socket through a new name: err=%v out=%q", err, out)
			}
			if now, statErr := os.Lstat(dir); statErr != nil || !os.SameFile(beforeDir, now) {
				t.Fatalf("the socket directory no longer names the same directory: %v", statErr)
			}
			if now, statErr := os.Lstat(sock); statErr != nil || !os.SameFile(beforeSock, now) {
				t.Fatalf("the socket no longer names the same file: %v", statErr)
			}

			// Without the rule the same attack works: the denial is the policy's.
			root, dir, sock = build(t)
			open := Policy{WritableRoots: []string{root}, Network: true}
			if out, err := attack.run(t, dir, sock, open); err != nil {
				t.Fatalf("the attack must work against an unprotected socket, or the test proves nothing: %v (%s)", err, out)
			}
		})
	}
}
