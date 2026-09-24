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
// the caller fall back to approval-gated host execution instead of running
// with a policy that silently drops the state redirection a tool depends on.
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

	if policy.Network {
		body.WriteString("\n")
		body.WriteString(seatbeltNetworkPolicy)
	}
	return body.String(), params, true
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
