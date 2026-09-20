package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
