package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Profile construction is platform-independent on purpose: the policy text is
// the reviewable artifact, so it must be testable on the Linux CI host too.

func TestSeatbeltProfileClosesByDefault(t *testing.T) {
	profile, params, ok := SeatbeltProfile(Policy{WritableRoots: []string{"/work/ws"}})
	if !ok {
		t.Fatal("a plain writable-root policy must be renderable")
	}
	if !strings.HasPrefix(strings.TrimSpace(profile), "; SelfMind macOS exec sandbox") {
		t.Fatalf("profile lost its header: %.60q", profile)
	}
	if !strings.Contains(profile, "(deny default)") {
		t.Fatal("the profile must start closed")
	}
	if want := `-DWRITABLE_ROOT_0=/work/ws`; len(params) != 1 || params[0] != want {
		t.Fatalf("writable root must travel as a parameter, got %v", params)
	}
	// The path must NOT appear in the policy text: that is what makes a path
	// unable to terminate a literal and inject a rule.
	if strings.Contains(profile, "/work/ws") {
		t.Fatal("paths must travel as parameters, never interpolated into the policy")
	}
	if !strings.Contains(profile, `(allow file-write* (subpath (param "WRITABLE_ROOT_0")))`) {
		t.Fatal("the writable root must be granted by parameter reference")
	}
	if !strings.Contains(profile, `(deny file-write-unlink (require-all (literal (param "WRITABLE_ROOT_0")) (vnode-type DIRECTORY)))`) {
		t.Fatal("the writable root itself must not be unlinkable")
	}
}

func TestSeatbeltProfileNetworkIsOpenedOnlyExplicitly(t *testing.T) {
	closed, _, ok := SeatbeltProfile(Policy{WritableRoots: []string{"/work/ws"}})
	if !ok {
		t.Fatal("policy must render")
	}
	if strings.Contains(closed, "(allow network") {
		t.Fatal("egress must be closed by omission when Network is false")
	}
	open, _, ok := SeatbeltProfile(Policy{WritableRoots: []string{"/work/ws"}, Network: true})
	if !ok {
		t.Fatal("networked policy must render")
	}
	if !strings.Contains(open, "(allow network*)") {
		t.Fatal("Network=true must open egress")
	}
	// Generality: the ONLY difference between the two renderings is the network
	// block, so enabling egress cannot silently widen the filesystem view.
	if strings.TrimSpace(strings.TrimSuffix(open, seatbeltNetworkPolicy)) != strings.TrimSpace(closed) {
		t.Fatal("enabling the network must change nothing but the network block")
	}
}

func TestSeatbeltProfileNumbersEveryRootItDeclares(t *testing.T) {
	profile, params, ok := SeatbeltProfile(Policy{
		WritableRoots: []string{"/work/ws", "/work/other"},
		ScratchTmp:    "/run/scratch",
	})
	if !ok {
		t.Fatal("policy must render")
	}
	if len(params) != 3 {
		t.Fatalf("scratch is a writable root too, got params %v", params)
	}
	// sandbox-exec rejects a policy that reads an undefined parameter, so every
	// referenced key must have a binding and vice versa.
	for _, param := range params {
		key := strings.TrimPrefix(strings.SplitN(param, "=", 2)[0], "-D")
		if !strings.Contains(profile, `(param "`+key+`")`) {
			t.Fatalf("parameter %s is bound but never referenced", key)
		}
	}
	if strings.Contains(profile, "WRITABLE_ROOT_3") {
		t.Fatal("the policy references a parameter it does not bind")
	}
}

// Seatbelt cannot mount, so tool state that bubblewrap would redirect with a
// private overlay is granted as an in-place write on exactly its declared
// subpath. It travels as a parameter like a writable root, is named apart from
// the roots in the policy text, and its directory cannot be unlinked.
func TestSeatbeltProfileGrantsStateWritesByParameter(t *testing.T) {
	profile, params, ok := SeatbeltProfile(Policy{
		WritableRoots:   []string{"/work/ws"},
		StateWritePaths: []string{"/home/u/.aws/sso/cache", "/home/u/.aws/cli/cache"},
	})
	if !ok {
		t.Fatal("a policy with declared state writes must render")
	}
	for index, path := range []string{"/home/u/.aws/sso/cache", "/home/u/.aws/cli/cache"} {
		key := "STATE_WRITE_" + string(rune('0'+index))
		want := "-D" + key + "=" + path
		found := false
		for _, param := range params {
			found = found || param == want
		}
		if !found {
			t.Fatalf("state write %s must travel as %s, got %v", path, want, params)
		}
		if !strings.Contains(profile, `(allow file-write* (subpath (param "`+key+`")))`) {
			t.Fatalf("state write %s is not granted by parameter reference", key)
		}
		if !strings.Contains(profile, `(deny file-write-unlink (require-all (literal (param "`+key+`")) (vnode-type DIRECTORY)))`) {
			t.Fatalf("state write %s may be unlinked", key)
		}
		if !strings.Contains(profile, `(deny file-write-mode file-write-flags file-write-owner file-write-xattr file-write-acl (literal (param "`+key+`")))`) {
			t.Fatalf("state write %s may have its own metadata changed", key)
		}
		if !strings.Contains(profile, `(deny file-write-create (require-all (subpath (param "`+key+`")) (require-not (vnode-type REGULAR-FILE DIRECTORY))))`) ||
			!strings.Contains(profile, `(deny file-link (subpath (param "`+key+`")))`) {
			t.Fatalf("state write %s may plant links or special files", key)
		}
	}
	if strings.Contains(profile, "/home/u/.aws") {
		t.Fatal("state paths must travel as parameters, never interpolated into the policy")
	}
	// Generality: removing the state writes removes exactly their clauses.
	plain, _, _ := SeatbeltProfile(Policy{WritableRoots: []string{"/work/ws"}})
	if strings.Contains(plain, "STATE_WRITE") {
		t.Fatal("a policy without state writes must not grant any")
	}
	for _, unsound := range []string{"home/u/.aws/sso/cache", "/home/u/.aws/../.aws/sso/cache", "/"} {
		if _, _, ok := SeatbeltProfile(Policy{StateWritePaths: []string{unsound}}); ok {
			t.Fatalf("an unsound state write %q must not render", unsound)
		}
	}
}

// A canary travels as a parameter like every path, grants only the lookup of
// its own name, and is refused when it is not a plain mach name.
func TestSeatbeltProfileCarriesTheCanaryByParameter(t *testing.T) {
	profile, params, ok := SeatbeltProfile(Policy{Canary: "com.selfmind.exec.0123456789abcdef01234567"})
	if !ok || !strings.Contains(profile, `(allow mach-lookup (global-name (param "CANARY")))`) {
		t.Fatalf("the canary must be granted by parameter: %v\n%s", ok, profile)
	}
	if !slices.Contains(params, "-DCANARY=com.selfmind.exec.0123456789abcdef01234567") || strings.Contains(profile, "0123456789abcdef") {
		t.Fatalf("the canary must travel as a parameter, never in the policy text: %v", params)
	}
	plain, _, _ := SeatbeltProfile(Policy{})
	if strings.Contains(plain, "CANARY") {
		t.Fatal("a policy without a canary must not grant a lookup")
	}
	for _, unsound := range []string{`com.selfmind")) (allow default`, "com selfmind", strings.Repeat("a", 121), "com.selfmind.exec.0123abcd", "com.selfmind.test.0123456789abcdef01234567"} {
		if _, _, ok := SeatbeltProfile(Policy{Canary: unsound}); ok {
			t.Fatalf("an unsound canary %q must not render", unsound)
		}
	}
}

// Item 2: the runtime's records of what sandboxed commands left — canary
// markers and the quarantine — stay read-only even when a command's writable
// root contains them, while the rest of that root stays writable.
func TestSeatbeltKeepsDenyWritePathsReadOnlyInsideAWritableRoot(t *testing.T) {
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	records := filepath.Join(root, "runtime", "host-state")
	if err := os.MkdirAll(filepath.Join(records, "canaries"), 0o700); err != nil {
		t.Fatal(err)
	}
	policy := Policy{WritableRoots: []string{root}, DenyWritePaths: []string{records}}
	for _, denied := range []string{
		"printf x > " + records + "/canaries/com.selfmind.exec.0123456789abcdef01234567",
		"mkdir " + records + "/quarantine",
		"mv " + records + " " + root + "/moved",
	} {
		if out, err := seatbeltRun(t, policy, denied); err == nil {
			t.Errorf("%q must be denied inside the runtime records, got: %s", denied, out)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(records, "canaries")); len(entries) != 0 {
		t.Fatalf("a record was planted: %v", entries)
	}
	// The constraint that must change the result: elsewhere in the root.
	if out, err := seatbeltRun(t, policy, "printf x > "+root+"/runtime/other && mkdir "+root+"/work"); err != nil {
		t.Fatalf("the rest of the writable root must stay writable: %v (%s)", err, out)
	}
}

// A cache declared flat holds only files directly in it, so its policy refuses
// creating a directory there; a cache not declared flat keeps its nesting.
func TestSeatbeltProfileRefusesDirectoriesInFlatState(t *testing.T) {
	profile, _, ok := SeatbeltProfile(Policy{
		StateWritePaths: []string{"/home/u/.aws/sso/cache", "/home/u/.tool/state"},
		FlatStatePaths:  []string{"/home/u/.aws/sso/cache"},
	})
	if !ok {
		t.Fatal("a policy with a flat state write must render")
	}
	flat := `(deny file-write-create (require-all (subpath (param "STATE_WRITE_0")) (vnode-type DIRECTORY)))`
	if !strings.Contains(profile, flat) {
		t.Fatal("a flat state write must refuse directories")
	}
	if strings.Contains(profile, `(deny file-write-create (require-all (subpath (param "STATE_WRITE_1")) (vnode-type DIRECTORY)))`) {
		t.Fatal("a state write not declared flat must keep its directories")
	}
	// A flat path that is not itself a state write is not a sound policy.
	if _, _, ok := SeatbeltProfile(Policy{StateWritePaths: []string{"/home/u/.tool/state"}, FlatStatePaths: []string{"/home/u/.aws/sso/cache"}}); ok {
		t.Fatal("a flat path outside the state writes must not render")
	}
}

// The kernel evidence for a flat cache: no directory appears in it, whether
// made there, copied in or moved in from a writable root; files still do, and
// a cache not declared flat still nests.
func TestSeatbeltFlatStateRefusesDirectories(t *testing.T) {
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	flat := filepath.Join(root, ".tool", "sso", "cache")
	nested := filepath.Join(root, ".tool", "other", "cache")
	workspace := filepath.Join(root, "ws")
	for _, dir := range []string{flat, nested, filepath.Join(workspace, "dir")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "dir", "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := Policy{WritableRoots: []string{workspace}, StateWritePaths: []string{flat, nested}, FlatStatePaths: []string{flat}}
	if out, err := seatbeltRun(t, policy, "printf token > "+flat+"/token.json && mv "+flat+"/token.json "+flat+"/renamed.json"); err != nil {
		t.Fatalf("files in a flat cache must stay writable: %v (%s)", err, out)
	}
	for _, planted := range []struct{ name, script string }{
		{"mkdir", "mkdir " + flat + "/sub"},
		{"directory renamed in", "mv " + workspace + "/dir " + flat + "/dir"},
		{"directory copied in", "cp -R " + workspace + "/dir " + flat + "/copy"},
	} {
		if out, err := seatbeltRun(t, policy, planted.script); err == nil {
			t.Errorf("%s: %q must be denied in a flat cache, got: %s", planted.name, planted.script, out)
		}
	}
	entries, err := os.ReadDir(flat)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Errorf("a directory appeared in the flat cache: %s", entry.Name())
		}
	}
	// The constraint that must change the result: the same directory in a
	// cache not declared flat.
	if out, err := seatbeltRun(t, policy, "mkdir "+nested+"/sub && printf x > "+nested+"/sub/f"); err != nil {
		t.Fatalf("a cache not declared flat must keep its directories: %v (%s)", err, out)
	}
}

// The kernel evidence: inside the declared state subpath a write lands; its
// parent and a sibling configuration file stay read-only.
func TestSeatbeltConfinesStateWritesToTheDeclaredSubpath(t *testing.T) {
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	state := filepath.Join(root, ".tool", "sso", "cache")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, ".tool", "config")
	if err := os.WriteFile(config, []byte("[default]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := Policy{StateWritePaths: []string{state}}
	if out, err := seatbeltRun(t, policy, "printf token > "+state+"/token.json"); err != nil {
		t.Fatalf("a write inside the declared state subpath must succeed: %v (%s)", err, out)
	}
	for _, denied := range []string{
		"printf x >> " + config,
		"printf x > " + filepath.Join(root, ".tool", "stray"),
		"printf x > " + filepath.Join(root, ".tool", "sso", "stray"),
		// The subpath's own directory is the authority boundary: emptying it
		// is allowed, removing it is not.
		"rm -f " + state + "/token.json && rmdir " + state,
	} {
		if out, err := seatbeltRun(t, policy, denied); err == nil {
			t.Fatalf("%q must be denied outside the declared subpath, got: %s", denied, out)
		}
	}
	if data, _ := os.ReadFile(config); string(data) != "[default]\n" {
		t.Fatal("the configuration changed")
	}
	if info, err := os.Stat(state); err != nil || !info.IsDir() {
		t.Fatalf("the declared state directory was removed: %v", err)
	}
}

// L3: the subpath directory is an authority boundary like a writable root, so
// its own metadata — mode, flags, owner, extended attributes — is not the
// sandbox's to change; the files inside it stay writable.
func TestSeatbeltStateWriteKeepsTheDirectoryMetadataReadOnly(t *testing.T) {
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	state := filepath.Join(root, ".tool", "sso", "cache")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	policy := Policy{StateWritePaths: []string{state}}
	if out, err := seatbeltRun(t, policy, "printf token > "+state+"/token.json && chmod 0600 "+state+"/token.json"); err != nil {
		t.Fatalf("files inside the subpath must stay writable, including their mode: %v (%s)", err, out)
	}
	for _, denied := range []string{
		"chmod 0777 " + state,
		// The same invariant through another metadata operation.
		"xattr -w com.selfmind.test planted " + state,
	} {
		if out, err := seatbeltRun(t, policy, denied); err == nil {
			t.Fatalf("%q must be denied on the subpath directory itself, got: %s", denied, out)
		}
	}
	if info, err := os.Stat(state); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the subpath directory's mode changed: %v %v", info.Mode(), err)
	}
}

// The ACL is directory metadata too: an access-control entry added to the
// subpath directory would outlive the sandbox and widen who can write there.
func TestSeatbeltStateWriteKeepsTheDirectoryACLReadOnly(t *testing.T) {
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	state := filepath.Join(root, ".tool", "sso", "cache")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	policy := Policy{StateWritePaths: []string{state}}
	if out, err := seatbeltRun(t, policy, `chmod +a "everyone allow write" `+state); err == nil {
		t.Fatalf("an ACL entry on the subpath directory must be denied, got: %s", out)
	}
	if out, err := exec.Command("/bin/ls", "-led", state).CombinedOutput(); err != nil || strings.Contains(string(out), "everyone") {
		t.Fatalf("the subpath directory gained an ACL entry: %s %v", out, err)
	}
	// Generality: the same ACL on a file inside the subpath is the file's own
	// business, like its mode.
	if out, err := seatbeltRun(t, policy, "printf token > "+state+"/token.json && chmod +a \"everyone deny write\" "+state+"/token.json"); err != nil {
		t.Fatalf("a file inside the subpath must keep its own metadata writable: %v (%s)", err, out)
	}
}

// Host tools read the credential cache later, outside any sandbox. A link or
// special file planted there would outlive the run: a symbolic link redirects
// the host tool's next write, a hard link aliases a file outside the subpath so
// a write through the cache path lands on it, and a FIFO blocks the reader. The
// sandbox may create only regular files and directories inside the subpath.
func TestSeatbeltStateWriteCreatesOnlyRegularFilesAndDirectories(t *testing.T) {
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	state := filepath.Join(root, ".tool", "sso", "cache")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, ".tool", "config")
	if err := os.WriteFile(config, []byte("[default]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	policy := Policy{WritableRoots: []string{workspace}, StateWritePaths: []string{state}}
	if out, err := seatbeltRun(t, policy, "printf token > "+state+"/token.json && mkdir "+state+"/nested && printf token > "+state+"/nested/token.json && mv "+state+"/token.json "+state+"/renamed.json"); err != nil {
		t.Fatalf("regular files and directories inside the subpath must stay creatable: %v (%s)", err, out)
	}
	// The constraint that must change the result: the same links in an
	// ordinary writable root are the workspace's own business.
	if out, err := seatbeltRun(t, policy, "printf x > "+workspace+"/file && ln -s file "+workspace+"/link && ln "+workspace+"/file "+workspace+"/hard && mkfifo "+workspace+"/pipe"); err != nil {
		t.Fatalf("links in a writable root must stay creatable: %v (%s)", err, out)
	}
	for _, planted := range []struct{ name, script string }{
		{"symlink", "ln -s " + config + " " + state + "/link.json"},
		{"symlink in a nested directory", "ln -s " + config + " " + state + "/nested/link.json"},
		{"fifo", "mkfifo " + state + "/pipe"},
		{"hard link to a file outside", "ln " + config + " " + state + "/hard.json"},
		{"hard link to a file inside", "ln " + state + "/renamed.json " + state + "/nested/hard.json"},
		{"hard link to a workspace file", "ln " + workspace + "/file " + state + "/hard.json"},
		// The same entries made elsewhere and renamed in.
		{"symlink renamed in", "mv " + workspace + "/link " + state + "/link.json"},
		{"fifo renamed in", "mv " + workspace + "/pipe " + state + "/pipe"},
		{"hard link renamed in", "mv " + workspace + "/hard " + state + "/hard.json"},
	} {
		if out, err := seatbeltRun(t, policy, planted.script); err == nil {
			t.Errorf("%s: %q must be denied inside the state subpath, got: %s", planted.name, planted.script, out)
		}
	}
	for _, name := range []string{"link.json", "nested/link.json", "pipe", "hard.json", "nested/hard.json"} {
		if _, err := os.Lstat(filepath.Join(state, name)); !os.IsNotExist(err) {
			t.Errorf("%s was planted in the state subpath: %v", name, err)
		}
	}
	if data, _ := os.ReadFile(config); string(data) != "[default]\n" {
		t.Fatal("the configuration changed")
	}
}

func TestSeatbeltProfileFailsClosedOnUnsoundInput(t *testing.T) {
	cases := []struct {
		name   string
		policy Policy
	}{
		// Refused rather than ignored: a mount-backed redirection that silently
		// did not happen would point tool state at the host.
		{"overlay mount", Policy{WritableRoots: []string{"/work/ws"},
			OverlayMounts: []OverlayMount{{Source: "/state/sso", Target: "/home/u/.aws/sso/cache"}}}},
		{"synthesized dir", Policy{WritableRoots: []string{"/work/ws"},
			SynthesizedDirs: []SynthesizedDir{{Target: "/home/u/.aws"}}}},
		// Refused rather than dropped: a missing writable root surfaces as a
		// confusing denial deep inside a tool.
		{"relative root", Policy{WritableRoots: []string{"work/ws"}}},
		{"unclean root", Policy{WritableRoots: []string{"/work/ws/../ws"}}},
		{"unclean scratch", Policy{ScratchTmp: "/run/./scratch"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := SeatbeltProfile(tc.policy); ok {
				t.Fatal("an unsound policy must not render")
			}
		})
	}
}

// The checks below need the real kernel enforcement, so they are darwin-only.
// They are the evidence a Linux CI run cannot produce.

func seatbeltRun(t *testing.T, policy Policy, script string) (string, error) {
	t.Helper()
	argv, ok := WrapSeatbelt(policy, []string{"/bin/sh", "-c", script})
	if !ok {
		t.Fatal("policy must wrap on a host with sandbox-exec")
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	return string(out), err
}

func TestSeatbeltConfinesWritesToDeclaredRoots(t *testing.T) {
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	root := t.TempDir()
	// t.TempDir is under /var/folders on macOS, which is a symlink target of
	// /private/var; the policy binds the real path, so resolve it the same way
	// the tools layer does.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	policy := Policy{WritableRoots: []string{root}}

	if out, err := seatbeltRun(t, policy, "printf inside > "+root+"/ok.txt"); err != nil {
		t.Fatalf("a write inside the declared root must succeed: %v (%s)", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, "ok.txt")); err != nil {
		t.Fatalf("the in-root write did not land: %v", err)
	}

	// The constraint that must change the result: same command, a path outside
	// every declared root.
	outside := filepath.Join(os.TempDir(), "selfmind-seatbelt-escape.txt")
	os.Remove(outside)
	if out, err := seatbeltRun(t, policy, "printf escaped > "+outside); err == nil {
		t.Fatalf("a write outside every writable root must fail, got: %s", out)
	}
	if _, err := os.Stat(outside); err == nil {
		os.Remove(outside)
		t.Fatal("the escaping write landed on the host")
	}

	// Generality: a different outside path, same invariant.
	if out, err := seatbeltRun(t, policy, "printf escaped > "+filepath.Dir(root)+"/sibling.txt"); err == nil {
		t.Fatalf("a write to the parent of the writable root must fail, got: %s", out)
	}

	// Reads stay unrestricted, matching the bubblewrap read-only host view.
	if out, err := seatbeltRun(t, policy, "cat /etc/hosts > /dev/null"); err != nil {
		t.Fatalf("reads outside the writable roots must still work: %v (%s)", err, out)
	}
}

func TestSeatbeltGatesEgressOnPolicy(t *testing.T) {
	if !SeatbeltAvailable() {
		t.Skip("sandbox-exec unavailable")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("darwin only")
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	// A loopback connect to a port nothing listens on needs no external network
	// and no DNS, so the outcome reflects the policy rather than this host's
	// connectivity. Both policies fail the connect; only the REASON differs,
	// and that is exactly what distinguishes a sandbox denial from the kernel
	// refusing a port. Asserting on the exit status alone would pass for the
	// wrong reason.
	const probe = `exec 3<>/dev/tcp/127.0.0.1/9`
	const denied, refused = "Operation not permitted", "Connection refused"

	out, _ := seatbeltRun(t, Policy{WritableRoots: []string{root}}, probe)
	if !strings.Contains(out, denied) {
		t.Fatalf("the socket must be denied by the sandbox when the policy does not share the network, got: %q", out)
	}

	// The one changed input that must flip the outcome.
	out, _ = seatbeltRun(t, Policy{WritableRoots: []string{root}, Network: true}, probe)
	if strings.Contains(out, denied) {
		t.Fatalf("Network=true must let the socket call reach the kernel, got: %q", out)
	}
	if !strings.Contains(out, refused) {
		t.Fatalf("expected the kernel to refuse the closed port, got: %q", out)
	}
}
