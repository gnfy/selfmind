package sandbox

import (
	_ "embed"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

//go:embed seatbelt_base.sbpl
var seatbeltBasePolicy string

//go:embed seatbelt_network.sbpl
var seatbeltNetworkPolicy string

// seatbeltExecutable is deliberately absolute. Resolving `sandbox-exec` through
// PATH would let anything earlier on PATH become the thing that "enforces" the
// sandbox, which inverts the guarantee. The same reasoning is why codex pins
// this path (codex-rs/sandboxing/src/seatbelt.rs).
const seatbeltExecutable = "/usr/bin/sandbox-exec"

var (
	seatbeltOnce sync.Once
	seatbeltOK   bool
)

// SeatbeltAvailable reports whether macOS can enforce a policy on this host.
// Cached for the same reason as bwrapPath: host capability does not change
// within a daemon lifetime.
func SeatbeltAvailable() bool {
	seatbeltOnce.Do(func() {
		if runtime.GOOS != "darwin" {
			return
		}
		info, err := os.Stat(seatbeltExecutable)
		seatbeltOK = err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
	})
	return seatbeltOK
}

// SeatbeltProfile renders the policy text and the matching -D parameter
// bindings for one command.
//
// Paths travel as PARAMETERS, never interpolated into the policy text, so no
// path can terminate a string literal and inject a rule. The profile is
// generated to reference exactly the parameters returned beside it:
// sandbox-exec rejects a policy that reads an undefined parameter, so the two
// must be built together.
//
// ok is false when no sound policy can be built. Seatbelt is a permission
// filter with no mount namespace, so a plan carrying OverlayMounts or
// SynthesizedDirs cannot be honoured here — reporting that as a failure lets
// the caller refuse the call instead of running with a policy that silently
// drops the state redirection a tool depends on. StateWritePaths are the form
// of that state seatbelt can enforce: in-place writes on declared subpaths.
func SeatbeltProfile(policy Policy) (profile string, params []string, ok bool) {
	if len(policy.OverlayMounts) > 0 || len(policy.SynthesizedDirs) > 0 {
		return "", nil, false
	}

	var body strings.Builder
	body.WriteString(seatbeltBasePolicy)

	// ScratchTmp is an ordinary writable root here. Under bwrap it is bound at
	// /tmp as well so $SELFMIND_RUN_TMP and /tmp are one directory; seatbelt
	// cannot mount, so that equivalence does not hold on macOS. TMPDIR is
	// already redirected to the scratch directory (executionenv/scratch.go),
	// which is what keeps mktemp and friends inside the writable view.
	roots := make([]string, 0, len(policy.WritableRoots)+1)
	roots = append(roots, policy.WritableRoots...)
	roots = append(roots, policy.ScratchTmp)

	index := 0
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		// Fail closed rather than dropping the root: a policy that silently
		// omits a writable root produces confusing permission failures deep
		// inside a tool, whereas refusing here surfaces as an ordinary
		// approval-gated host run.
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return "", nil, false
		}
		key := "WRITABLE_ROOT_" + strconv.Itoa(index)
		index++
		params = append(params, "-D"+key+"="+root)
		body.WriteString("\n(allow file-write* (subpath (param \"" + key + "\")))\n")
		// A confined process must not be able to unlink the writable root
		// itself: that root is an authority boundary reused to build the next
		// policy, and replacing it would move the boundary.
		body.WriteString("(deny file-write-unlink (require-all (literal (param \"" +
			key + "\")) (vnode-type DIRECTORY)))\n")
	}

	// Tool state seatbelt cannot redirect with a mount is written in place, on
	// exactly the declared subpath: never its parent, which stays read-only
	// like the rest of the host. The subpath's own directory is an authority
	// boundary like a writable root: it cannot be removed, and its mode, flags,
	// owner, extended attributes and ACL stay as the host made them, while the
	// files inside it remain writable. Host tools read this cache after the
	// run, so the sandbox may create only regular files and directories in it:
	// a symbolic link, hard link or special file planted there would redirect,
	// alias or block the host tool's next access. A cache declared flat holds
	// only files directly in it, so no directory may be created or moved in
	// there either; a flat path that is not itself a state write is not a
	// sound policy.
	stateWrites := map[string]bool{}
	for _, path := range policy.StateWritePaths {
		stateWrites[strings.TrimSpace(path)] = true
	}
	flat := map[string]bool{}
	for _, path := range policy.FlatStatePaths {
		path = strings.TrimSpace(path)
		if !stateWrites[path] {
			return "", nil, false
		}
		flat[path] = true
	}
	for stateIndex, path := range policy.StateWritePaths {
		path = strings.TrimSpace(path)
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path {
			return "", nil, false
		}
		key := "STATE_WRITE_" + strconv.Itoa(stateIndex)
		params = append(params, "-D"+key+"="+path)
		body.WriteString("\n(allow file-write* (subpath (param \"" + key + "\")))\n")
		body.WriteString("(deny file-write-unlink (require-all (literal (param \"" +
			key + "\")) (vnode-type DIRECTORY)))\n")
		body.WriteString("(deny file-write-mode file-write-flags file-write-owner file-write-xattr file-write-acl (literal (param \"" +
			key + "\")))\n")
		body.WriteString("(deny file-write-create (require-all (subpath (param \"" + key +
			"\")) (require-not (vnode-type REGULAR-FILE DIRECTORY))))\n")
		// A hard link to a regular file is itself a regular file, so the type
		// rule alone cannot refuse it.
		body.WriteString("(deny file-link (subpath (param \"" + key + "\")))\n")
		if flat[path] {
			body.WriteString("(deny file-write-create (require-all (subpath (param \"" + key +
				"\")) (vnode-type DIRECTORY)))\n")
		}
	}

	// The canary marks the processes running under this one policy: only it
	// may look the name up, and nothing is registered under it.
	if canary := strings.TrimSpace(policy.Canary); canary != "" {
		if !ValidCanary(canary) {
			return "", nil, false
		}
		params = append(params, "-DCANARY="+canary)
		body.WriteString("\n(allow mach-lookup (global-name (param \"CANARY\")))\n")
	}

	// The runtime's records of what sandboxed commands left — canary markers
	// and the quarantine — stay read-only even inside a writable root. These
	// rules come last so they win over every allowance above.
	for denyIndex, path := range policy.DenyWritePaths {
		path = strings.TrimSpace(path)
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path {
			return "", nil, false
		}
		key := "DENY_WRITE_" + strconv.Itoa(denyIndex)
		params = append(params, "-D"+key+"="+path)
		body.WriteString("\n(deny file-write* (subpath (param \"" + key + "\")))\n")
	}

	if policy.Network {
		body.WriteString("\n")
		body.WriteString(seatbeltNetworkPolicy)
	}
	privateRules, privateParams, ok := seatbeltPrivateRules(policy)
	if !ok {
		return "", nil, false
	}
	body.WriteString(privateRules)
	params = append(params, privateParams...)
	return body.String(), params, true
}

// seatbeltPrivateRules closes the daemon's own state. Seatbelt applies the
// last matching rule, so these denials are appended after every allow: a
// writable root that contains the data directory, or the shared network, must
// not reopen them. Like writable roots, an unsound entry refuses the whole
// policy instead of being dropped.
//
// The denials bind paths, so they hold only while the paths keep naming the
// state. A socket's own name and every directory above private state inside a
// writable root are therefore denied unlink, which is what rename, swap and
// rename-over check on the entry they replace or move. Hard links are refused
// by the sandbox itself for any path with a denial on it.
func seatbeltPrivateRules(policy Policy) (string, []string, bool) {
	private := policy.Private
	if private.Empty() {
		return "", nil, true
	}
	var rules strings.Builder
	var params []string
	rules.WriteString("\n; Daemon private state: unreachable whatever the rules above allow.\n")
	bind := func(prefix string, paths []string, rule func(key string) string) bool {
		for index, path := range paths {
			path = strings.TrimSpace(path)
			if !filepath.IsAbs(path) || filepath.Clean(path) != path {
				return false
			}
			key := prefix + strconv.Itoa(index)
			params = append(params, "-D"+key+"="+path)
			rules.WriteString(rule(key))
		}
		return true
	}
	if !bind("PRIVATE_DIR_", private.Dirs, func(key string) string {
		// Connecting to a socket checks only the network rule, never the file
		// rules, so a socket inside the directory needs its own denial.
		return "(deny file-read* file-write* (subpath (param \"" + key + "\")))\n" +
			"(deny network-outbound (subpath (param \"" + key + "\")))\n"
	}) || !bind("PRIVATE_FILE_", private.Files, func(key string) string {
		return "(deny file-read* file-write* (literal (param \"" + key + "\")))\n"
	}) || !bind("PRIVATE_PREFIX_", private.FilePrefixes, func(key string) string {
		return "(deny file-read* file-write* (prefix (param \"" + key + "\")))\n"
	}) || !bind("PRIVATE_SOCKET_", private.Sockets, func(key string) string {
		return "(deny network-outbound (literal (param \"" + key + "\")))\n" +
			"(deny file-write-unlink (literal (param \"" + key + "\")))\n"
	}) || !bind("PRIVATE_ANCESTOR_", protectedAncestors(policy), func(key string) string {
		return "(deny file-write-unlink (literal (param \"" + key + "\")))\n"
	}) {
		return "", nil, false
	}
	// A link on the way to private state is denied unlink like an ancestor. An
	// unsound entry refuses the policy rather than being filtered out below.
	for _, link := range private.Symlinks {
		if link = strings.TrimSpace(link); !filepath.IsAbs(link) || filepath.Clean(link) != link {
			return "", nil, false
		}
	}
	if !bind("PRIVATE_LINK_", protectedSymlinks(policy), func(key string) string {
		return "(deny file-write-unlink (literal (param \"" + key + "\")))\n"
	}) {
		return "", nil, false
	}
	for _, listener := range private.TCPPorts {
		if listener.Port <= 0 || listener.Port > 65535 {
			return "", nil, false
		}
		// SBPL accepts only "localhost" or "*" as the host of a remote address.
		// localhost covers both loopback families; a daemon bound beyond
		// loopback is reachable through any local address, so every host is
		// denied on that port.
		host := "localhost"
		if listener.AllHosts {
			host = "*"
		}
		rules.WriteString("(deny network-outbound (remote ip \"" + host + ":" + strconv.Itoa(listener.Port) + "\"))\n")
	}
	return rules.String(), params, true
}

// WrapSeatbelt returns the argv that runs innerArgv under the policy, or
// (innerArgv, false) when this host cannot enforce it. Like Wrap, it decides
// capability only — whether an unenforceable policy may fall back to host
// execution is the caller's decision.
func WrapSeatbelt(policy Policy, innerArgv []string) ([]string, bool) {
	if !SeatbeltAvailable() {
		return innerArgv, false
	}
	profile, params, ok := SeatbeltProfile(policy)
	if !ok {
		return innerArgv, false
	}
	argv := make([]string, 0, len(params)+len(innerArgv)+4)
	argv = append(argv, seatbeltExecutable, "-p", profile)
	argv = append(argv, params...)
	argv = append(argv, "--")
	argv = append(argv, innerArgv...)
	return argv, true
}
