// Package sandbox builds bubblewrap (bwrap) command wrappers for exec tools.
// Landlock alone cannot gate network egress, so the isolation is namespace-
// based: read-only host root, writable workspace only, and no network by
// default.
//
// This package only constructs argv and detects capability; it never decides
// policy or approval. The caller decides whether an unavailable sandbox may
// fall back to host execution or must fail closed.
package sandbox

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Policy is the isolation contract for one command.
type Policy struct {
	// WritableRoots are absolute paths mounted read-write inside the sandbox
	// (typically the active workspace root). Everything else is read-only.
	WritableRoots []string
	// Network allows egress. Default false: external side effects that need
	// the network must either use an operator-enabled isolated network policy
	// or explicitly request the approval-gated host mode.
	Network bool
}

// detector abstracts capability probing for tests.
type detector struct {
	lookPath func(string) (string, error)
	userns   func() bool
}

var defaultDetector = detector{
	lookPath: exec.LookPath,
	userns:   unprivilegedUserNSAvailable,
}

var (
	availOnce sync.Once
	availPath string
	availOK   bool
)

// bwrapPath returns the resolved bwrap binary path and whether the sandbox is
// usable (bwrap present AND unprivileged user namespaces enabled). Cached: the
// host capability does not change within a daemon lifetime.
func bwrapPath() (string, bool) {
	availOnce.Do(func() {
		availPath, availOK = defaultDetector.resolve()
	})
	return availPath, availOK
}

func (d detector) resolve() (string, bool) {
	path, err := d.lookPath("bwrap")
	if err != nil || strings.TrimSpace(path) == "" {
		return "", false
	}
	if !d.userns() {
		return "", false
	}
	return path, true
}

// Available reports whether commands can be sandboxed on this host.
func Available() bool {
	_, ok := bwrapPath()
	return ok
}

// unprivilegedUserNSAvailable reads the kernel's unprivileged user-namespace
// budget. bwrap needs it to build the namespace without setuid.
func unprivilegedUserNSAvailable() bool {
	// max_user_namespaces > 0 is the portable signal; the older
	// unprivileged_userns_clone toggle is a fallback on some distros.
	if v, err := os.ReadFile("/proc/sys/user/max_user_namespaces"); err == nil {
		if n, perr := strconv.Atoi(strings.TrimSpace(string(v))); perr == nil {
			return n > 0
		}
	}
	if v, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil {
		return strings.TrimSpace(string(v)) == "1"
	}
	return false
}

// WrapArgv returns the full argv that runs innerArgv inside a bwrap sandbox
// under the policy: read-only root, writable roots bind-mounted rw, private
// /tmp and /dev and /proc, and network unshared unless Policy.Network. The
// caller supplies innerArgv (e.g. ["sh","-c",command]).
func WrapArgv(bwrap string, policy Policy, innerArgv []string) []string {
	argv := []string{
		bwrap,
		"--die-with-parent", // the sandbox dies if the daemon does
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-cgroup-try",
		"--ro-bind", "/", "/", // read-only view of the host root
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	if policy.Network {
		// Keep the host network namespace: egress allowed.
	} else {
		argv = append(argv, "--unshare-net")
	}
	for _, root := range policy.WritableRoots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		argv = append(argv, "--bind", root, root) // rw bind overrides the ro-bind
	}
	argv = append(argv, "--")
	argv = append(argv, innerArgv...)
	return argv
}

// Wrap is the convenience path: returns (wrappedArgv, true) when the sandbox is
// available, else (innerArgv, false) so the caller runs directly. It never
// panics or errors — capability, not policy, lives here.
func Wrap(policy Policy, innerArgv []string) ([]string, bool) {
	bwrap, ok := bwrapPath()
	if !ok {
		return innerArgv, false
	}
	return WrapArgv(bwrap, policy, innerArgv), true
}
