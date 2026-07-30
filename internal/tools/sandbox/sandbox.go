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
	// ScratchTmp, when set, is the run's durable temp directory. It replaces the
	// private tmpfs so temp files survive from one command of a run to the next,
	// and it is bound TWICE — at its real absolute path and at /tmp — so the
	// path exported as $SELFMIND_RUN_TMP is literally the same under host and
	// isolated execution. It must not live under /tmp: the /tmp bind shadows
	// every real path beneath it.
	ScratchTmp string
	// SynthesizedDirs replace a host directory with a writable shell, keeping
	// only the named children readable. They are mounted BEFORE writable roots
	// and overlays, because an overlay target inside one of them only becomes
	// mountable once its writable parent exists.
	SynthesizedDirs []SynthesizedDir
	// OverlayMounts bind a writable directory over another path. They exist for
	// tool state whose location is not configurable, so no environment redirect
	// can move it; the host is untouched because the bind is visible only inside
	// this sandbox.
	OverlayMounts []OverlayMount
	// Network allows egress. Default false: external side effects that need
	// the network must either use an operator-enabled isolated network policy
	// or explicitly request the approval-gated host mode.
	Network bool
}

// OverlayMount binds Source over Target inside the sandbox.
type OverlayMount struct {
	Source string
	Target string
}

// SynthesizedDir is a tmpfs mounted at Target with ReadOnlyChildren bound back
// in read-only.
//
// It exists because a bind mount needs a mount POINT: bwrap must create the
// target path, and under a read-only host root it cannot. A tool whose state
// directory is only partially present on the host — `~/.aws` holding `config`
// but no `sso/` — therefore could not receive a writable `~/.aws/sso/cache`
// overlay at all, and the whole sandbox aborted before the command started
// ("Can't mkdir parents for /home/<user>/.aws/sso/cache: Read-only file
// system"), which made every command matching that profile fail rather than
// degrade. A tmpfs at the state root is writable, so bwrap can create any
// nested mount point inside it, and the named children stay readable.
//
// The host directory is never modified; the tmpfs is visible only inside this
// sandbox. Children that are NOT named become invisible — the mount hides the
// host directory — so the declaration is fail-closed by construction.
type SynthesizedDir struct {
	Target           string
	ReadOnlyChildren []string
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
	}
	if scratch := strings.TrimSpace(policy.ScratchTmp); scratch != "" {
		// Real path first so $SELFMIND_RUN_TMP resolves, then /tmp for tools
		// that hardcode it. Both are the same directory (same inode).
		argv = append(argv, "--bind", scratch, scratch, "--bind", scratch, "/tmp")
	} else {
		argv = append(argv, "--tmpfs", "/tmp")
	}
	if policy.Network {
		// Keep the host network namespace: egress allowed.
	} else {
		argv = append(argv, "--unshare-net")
	}
	// Synthesized directories come FIRST: a later bind whose target lives inside
	// one of them needs its writable parent to already exist. Their read-only
	// children are re-bound immediately after the tmpfs that hides them.
	for _, dir := range policy.SynthesizedDirs {
		target := strings.TrimSpace(dir.Target)
		if target == "" {
			continue
		}
		argv = append(argv, "--tmpfs", target)
		for _, child := range dir.ReadOnlyChildren {
			child = strings.TrimSpace(child)
			if child == "" {
				continue
			}
			argv = append(argv, "--ro-bind-try", child, child)
		}
	}
	for _, root := range policy.WritableRoots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		argv = append(argv, "--bind-try", root, root) // rw bind overrides the ro-bind
	}
	// Overlay binds come after the writable roots so they win over them. The
	// -try suffix only tolerates a missing SOURCE: the TARGET must still be a
	// creatable mount point, which is why an overlay into a path the host does
	// not have needs a SynthesizedDir above it.
	for _, overlay := range policy.OverlayMounts {
		source := strings.TrimSpace(overlay.Source)
		target := strings.TrimSpace(overlay.Target)
		if source == "" || target == "" {
			continue
		}
		argv = append(argv, "--bind-try", source, target)
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
