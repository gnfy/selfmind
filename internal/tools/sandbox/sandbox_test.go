package sandbox

import (
	"strings"
	"testing"
)

func TestWrapArgvReadOnlyRootAndWritableWorkspace(t *testing.T) {
	argv := WrapArgv("/usr/bin/bwrap", Policy{WritableRoots: []string{"/work/ws"}}, []string{"sh", "-c", "echo hi"})
	joined := strings.Join(argv, " ")
	// Read-only host root.
	if !strings.Contains(joined, "--ro-bind / /") {
		t.Fatalf("missing ro-bind of root: %v", argv)
	}
	// Workspace bound read-write AFTER the ro-bind so it overrides.
	roIdx := indexOf(argv, "--ro-bind")
	rwIdx := indexOfBindTo(argv, "/work/ws")
	if roIdx < 0 || rwIdx < 0 || rwIdx < roIdx {
		t.Fatalf("workspace rw bind must follow ro-bind: ro=%d rw=%d %v", roIdx, rwIdx, argv)
	}
	// Network unshared by default.
	if !strings.Contains(joined, "--unshare-net") {
		t.Fatalf("network must be unshared by default: %v", argv)
	}
	// Inner command preserved after the -- separator.
	sep := indexOf(argv, "--")
	if sep < 0 || sep+4 != len(argv) || argv[sep+1] != "sh" || argv[sep+3] != "echo hi" {
		t.Fatalf("inner argv not preserved after --: %v", argv)
	}
	if argv[0] != "/usr/bin/bwrap" {
		t.Fatalf("bwrap binary must lead argv: %v", argv)
	}
}

func TestWrapArgvNetworkOptIn(t *testing.T) {
	argv := WrapArgv("/usr/bin/bwrap", Policy{Network: true}, []string{"sh", "-c", "curl x"})
	if strings.Contains(strings.Join(argv, " "), "--unshare-net") {
		t.Fatalf("network opt-in must keep the host net namespace: %v", argv)
	}
}

func TestWrapFallsBackWhenUnavailable(t *testing.T) {
	// Force the detector to report no bwrap by resolving a fresh detector.
	d := detector{lookPath: func(string) (string, error) { return "", errNotFound }, userns: func() bool { return true }}
	if _, ok := d.resolve(); ok {
		t.Fatal("resolve must fail when bwrap is absent")
	}
	// userns unavailable also disqualifies.
	d2 := detector{lookPath: func(string) (string, error) { return "/usr/bin/bwrap", nil }, userns: func() bool { return false }}
	if _, ok := d2.resolve(); ok {
		t.Fatal("resolve must fail when user namespaces are unavailable")
	}
	// Both present → resolves.
	d3 := detector{lookPath: func(string) (string, error) { return "/usr/bin/bwrap", nil }, userns: func() bool { return true }}
	if path, ok := d3.resolve(); !ok || path != "/usr/bin/bwrap" {
		t.Fatalf("resolve must succeed with bwrap + userns: %q %v", path, ok)
	}
}

var errNotFound = &lookErr{}

type lookErr struct{}

func (*lookErr) Error() string { return "not found" }

func indexOf(argv []string, tok string) int {
	for i, a := range argv {
		if a == tok {
			return i
		}
	}
	return -1
}

func indexOfBindTo(argv []string, target string) int {
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == "--bind" && argv[i+1] == target && argv[i+2] == target {
			return i
		}
	}
	return -1
}
