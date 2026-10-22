// Package sandbox builds OS-level isolation wrappers for exec tools. One
// Policy describes the contract — read-only host view, writes confined to
// declared roots, no network by default — and each host enforces it with what
// it has: bubblewrap namespaces on Linux (this file), seatbelt permission
// filtering on macOS (seatbelt.go). The two are not equivalent: seatbelt has no
// mount namespace, so the Policy fields that need one are refused there rather
// than ignored.
//
// This package only constructs argv and detects capability; it never decides
// policy or approval. The caller decides whether an unavailable sandbox may
// fall back to host execution or must fail closed.
package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	// StateWritePaths are real host directories a tool's own state is written
	// in place, each granted as exactly that subpath. They are how a backend
	// with no mount namespace (seatbelt) realizes state bubblewrap shadows with
	// a private overlay, so they are weaker: the host copy is modified. The
	// bubblewrap backend refuses a plan that carries them.
	StateWritePaths []string
	// FlatStatePaths are the StateWritePaths whose tool keeps only files
	// directly in them: no directory may be created or moved in there.
	FlatStatePaths []string
	// Canary is a unique mach global name only this policy may look up. No
	// service is registered under it; it lets the caller find every process
	// still running under this one policy (ProcessesUnderCanary).
	Canary string
	// DenyWritePaths stay read-only even inside a writable root: the
	// runtime's own records of what sandboxed commands left behind.
	DenyWritePaths []string
	// Network allows egress. Default false: external side effects that need
	// the network must either use an operator-enabled isolated network policy
	// or explicitly request the approval-gated host mode.
	Network bool
	// Private is daemon state the command must never reach, whatever its
	// writable roots or network mode allow. It is applied last, so it wins.
	Private PrivateState
}

// PrivateState names the daemon's own state: the files that hold its records
// and credentials, and the listeners that accept control requests. A confined
// tool runs as the same OS user as the daemon, so file permissions alone do
// not keep it out; the sandbox has to.
type PrivateState struct {
	// Dirs are directory trees hidden from reads and writes.
	Dirs []string
	// Files are single files hidden from reads and writes.
	Files []string
	// FilePrefixes hide every file whose path starts with the prefix, so a file
	// and the copies a tool makes of it beside it (a backup, a temporary file)
	// share one entry: "/home/u/.selfmind/config.yaml." covers config.yaml.bak-1
	// and config.yaml.tmp, including ones created after the policy was built.
	// The last element is a name prefix, not a directory. Seatbelt enforces the
	// prefix itself; bubblewrap can only mask names that exist, so it masks the
	// ones present when the command starts.
	FilePrefixes []string
	// Symlinks are the symbolic links that stand on the paths the daemon and the
	// CLI use to reach private state: a `~/.selfmind` that points into a dotfiles
	// repository, a config file that is a link. The entries above are the
	// resolved real paths, so replacing one of these links would send the daemon
	// and the CLI to a tree the command made, without touching a single private
	// name. Each is the link's own path (its directory resolved, its own name not).
	// Seatbelt denies renaming or removing it; bubblewrap cannot, because a link
	// is a directory entry, not a mount point.
	Symlinks []string
	// Sockets are Unix domain socket paths the command may not connect to.
	Sockets []string
	// TCPPorts are the daemon's TCP listeners the command may not connect to.
	TCPPorts []TCPPort
}

// TCPPort is one daemon TCP listener. AllHosts is set when the daemon listens
// beyond loopback, so the port is reachable through any local address.
type TCPPort struct {
	Port     int
	AllHosts bool
}

// Empty reports whether nothing is protected.
func (p PrivateState) Empty() bool {
	return len(p.Dirs) == 0 && len(p.Files) == 0 && len(p.FilePrefixes) == 0 && len(p.Symlinks) == 0 && len(p.Sockets) == 0 && len(p.TCPPorts) == 0
}

// protectedAncestors lists, shallowest first, the directories a command could
// rename, swap or remove to move private state out from under its names.
//
// A private name only guards the state while it keeps naming it. Seatbelt
// matches paths, so a command that may write the data directory's parent could
// move the directory aside and reach everything inside through a name no rule
// mentions; bubblewrap's masks follow their directory, but the host path the
// daemon uses would still be gone or replaced by the command's own tree. The
// directories to pin are the ancestors of every private path that lie strictly
// inside a writable root: above the roots nothing is writable, a root itself is
// pinned already, and a private directory's own subtree is hidden whole.
func protectedAncestors(policy Policy) []string {
	roots := writableRoots(policy)
	if len(roots) == 0 {
		return nil
	}
	hidden := make([]string, 0, len(policy.Private.Dirs))
	for _, dir := range policy.Private.Dirs {
		if dir = strings.TrimSpace(dir); dir != "" {
			hidden = append(hidden, filepath.Clean(dir))
		}
	}
	seen := map[string]bool{}
	var ancestors []string
	var private []string
	private = append(private, policy.Private.Dirs...)
	private = append(private, policy.Private.Files...)
	private = append(private, policy.Private.FilePrefixes...)
	private = append(private, policy.Private.Sockets...)
	// A link's own directory is as movable as any other: moving it carries the
	// link to a name the daemon does not use.
	private = append(private, policy.Private.Symlinks...)
	for _, path := range private {
		path = strings.TrimSpace(path)
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			continue
		}
		for dir := filepath.Dir(path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			if seen[dir] || withinAny(hidden, dir) {
				continue
			}
			seen[dir] = true
			if insideAny(roots, dir) {
				ancestors = append(ancestors, dir)
			}
		}
	}
	sort.Slice(ancestors, func(i, j int) bool {
		if depth := strings.Count(ancestors[i], "/") - strings.Count(ancestors[j], "/"); depth != 0 {
			return depth < 0
		}
		return ancestors[i] < ancestors[j]
	})
	return ancestors
}

// protectedSymlinks lists the private symlinks a command could replace: those
// whose own directory is writable, that is inside or equal to a writable root.
// Removing or renaming a link needs write access to the directory it stands in.
func protectedSymlinks(policy Policy) []string {
	roots := writableRoots(policy)
	seen := map[string]bool{}
	var links []string
	for _, link := range policy.Private.Symlinks {
		link = strings.TrimSpace(link)
		if !filepath.IsAbs(link) || filepath.Clean(link) != link || seen[link] {
			continue
		}
		seen[link] = true
		if withinAny(roots, filepath.Dir(link)) {
			links = append(links, link)
		}
	}
	sort.Strings(links)
	return links
}

// writableRoots is every directory a command may write: its writable roots, the
// run's scratch, and the tool state it may write in place (StateWritePaths). The
// last are writable subpaths like any root, so a directory between one of them
// and a private path is as movable as one inside a root, and a link inside one is
// as replaceable.
func writableRoots(policy Policy) []string {
	var roots []string
	for _, root := range append(append(append([]string(nil), policy.WritableRoots...), policy.ScratchTmp), policy.StateWritePaths...) {
		if root = strings.TrimSpace(root); root != "" {
			roots = append(roots, filepath.Clean(root))
		}
	}
	return roots
}

// insideAny reports whether path lies strictly below one of roots.
func insideAny(roots []string, path string) bool {
	for _, root := range roots {
		if path != root && within(root, path) {
			return true
		}
	}
	return false
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
	argv = append(argv, pinAncestors(policy)...)
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
	argv = append(argv, privateStateMasks(policy.Private)...)
	argv = append(argv, "--")
	argv = append(argv, innerArgv...)
	return argv
}

// pinAncestors makes every directory that holds private state a mount point
// inside the sandbox. A mount point cannot be renamed, swapped or removed
// (EBUSY), while everything in it stays as writable as the root it lies in: an
// rw bind of a directory that is already inside a writable root changes no
// access. It must follow the writable roots, which it would otherwise be hidden
// by, and precede the overlays and masks that land inside it. Nothing under
// /tmp is visible in the sandbox, and a directory inside a synthesized or
// overlaid path is not the host's, so none of those are bound.
func pinAncestors(policy Policy) []string {
	var hiddenFromHost []string
	for _, dir := range policy.SynthesizedDirs {
		if target := strings.TrimSpace(dir.Target); target != "" {
			hiddenFromHost = append(hiddenFromHost, target)
		}
	}
	for _, overlay := range policy.OverlayMounts {
		if target := strings.TrimSpace(overlay.Target); target != "" {
			hiddenFromHost = append(hiddenFromHost, target)
		}
	}
	var argv []string
	for _, ancestor := range protectedAncestors(policy) {
		if within("/tmp", ancestor) || withinAny(hiddenFromHost, ancestor) {
			continue
		}
		argv = append(argv, "--bind-try", ancestor, ancestor)
	}
	return argv
}

// privateStateMasks hides the daemon's own state. The masks come after every
// bind so a writable root that contains the data directory cannot reopen it: a
// tmpfs replaces a private directory and /dev/null replaces a private file.
// A socket is unreachable once its directory is hidden; one under /tmp is
// already hidden by the private /tmp or the run scratch bound there.
//
// The callers pass only paths that exist: bwrap cannot create a mount point
// under the read-only host root, and a failed mount aborts the whole command.
//
// TCP listeners cannot be closed here. Shared network keeps the host network
// namespace, so a confined command reaches every loopback port; the daemon's
// own authorization of the TCP listener is the boundary on Linux.
func privateStateMasks(private PrivateState) []string {
	var argv []string
	hidden := make([]string, 0, len(private.Dirs))
	for _, dir := range private.Dirs {
		// Nothing under the host's /tmp is visible inside; a mask named there
		// would create a mount point in the scratch bound at /tmp.
		if dir = strings.TrimSpace(dir); dir != "" && !within("/tmp", dir) {
			argv = append(argv, "--tmpfs", dir)
			hidden = append(hidden, dir)
		}
	}
	for _, file := range private.Files {
		if file = strings.TrimSpace(file); file != "" && !within("/tmp", file) {
			argv = append(argv, "--ro-bind", "/dev/null", file)
		}
	}
	for _, prefix := range private.FilePrefixes {
		argv = append(argv, prefixMasks(prefix, hidden)...)
	}
	for _, socket := range private.Sockets {
		socket = strings.TrimSpace(socket)
		if socket == "" {
			continue
		}
		dir := filepath.Dir(socket)
		if within("/tmp", dir) || withinAny(hidden, dir) {
			continue
		}
		argv = append(argv, "--tmpfs", dir)
		hidden = append(hidden, dir)
	}
	return argv
}

// prefixMasks masks the entries that exist beside a file prefix when the command
// starts: a regular file by /dev/null, a directory by a tmpfs. A mount point has
// to exist, so a copy written later is masked from the next command on, not
// from this one. Symlinks are left alone: masking one would mask its target.
func prefixMasks(prefix string, hidden []string) []string {
	dir, base := filepath.Split(strings.TrimSpace(prefix))
	if base == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var argv []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), base) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if within("/tmp", path) || withinAny(hidden, path) {
			continue
		}
		switch {
		case entry.IsDir():
			argv = append(argv, "--tmpfs", path)
		case entry.Type().IsRegular():
			argv = append(argv, "--ro-bind", "/dev/null", path)
		}
	}
	return argv
}

func withinAny(roots []string, path string) bool {
	for _, root := range roots {
		if within(root, path) {
			return true
		}
	}
	return false
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
