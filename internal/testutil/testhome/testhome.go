// Package testhome keeps a test binary away from the developer's per-user
// state. Tests exercise code that derives ~/.selfmind, caches and config from
// HOME, and developers run them on the machine where their daemon lives, so a
// package whose tests reach that code isolates its whole binary:
//
//	func TestMain(m *testing.M) { testhome.Main(m) }
//
// A test that sets HOME itself with t.Setenv still does so; the binary's
// scratch home returns when that test ends.
package testhome

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/platform/scratchhome"
)

// ownerVar marks an environment this test binary already isolated. A helper
// process that re-executes the same binary (a fake CLI, a stdio server) keeps
// the environment it was started with — the owner's scratch home, or a HOME
// the spawning test chose — instead of making a home of its own that its
// os.Exit would never remove. The name avoids the SELF_/SELFMIND_ prefixes,
// which tool process environments strip, because stdio helpers start through
// them.
const ownerVar = "TESTHOME_OWNER"

// Main runs the binary's tests with HOME, the XDG base directories and
// SELF_CONFIG pointed at a scratch home created for this binary, removes that
// home, and exits with the tests' result. Go's own caches stay where the
// original environment keeps them. When no scratch home can be made it runs
// nothing: running against the real HOME is the failure it exists to prevent.
func Main(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	self := executable()
	if self != "" && os.Getenv(ownerVar) == self {
		return m.Run()
	}
	home, err := isolate()
	if err == nil && self != "" {
		err = os.Setenv(ownerVar, self)
	}
	if err != nil {
		if home != "" {
			_ = scratchhome.Remove(home)
		}
		fmt.Fprintf(os.Stderr, "testhome: cannot isolate HOME for this test binary: %v\n", err)
		return 1
	}
	defer func() {
		if err := scratchhome.Remove(home); err != nil {
			fmt.Fprintf(os.Stderr, "testhome: remove scratch home: %v\n", err)
		}
	}()
	return m.Run()
}

// executable is this binary's resolved path, or "" when it cannot be known.
func executable() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return path
}

func isolate() (string, error) {
	// Resolve Go's locations while HOME is still the original one. Without
	// them a child `go` still works, only slower: it rebuilds into the scratch
	// home, which goes away with the binary.
	var goDirs []string
	if goBin, err := exec.LookPath("go"); err == nil {
		if goDirs, err = scratchhome.GoDirs(context.Background(), goBin, os.Environ()); err != nil {
			fmt.Fprintf(os.Stderr, "testhome: Go caches not pinned: %v\n", err)
		}
	}
	home, err := scratchhome.New("selfmind-test-" + binaryName() + "-")
	if err != nil {
		return "", err
	}
	for _, assignment := range append(scratchhome.Env(home), goDirs...) {
		key, value, _ := strings.Cut(assignment, "=")
		if err := os.Setenv(key, value); err != nil {
			return home, err
		}
	}
	return home, nil
}

// binaryName names the scratch home after the test binary (tools.test), so a
// home left behind by a killed binary can be traced to its package.
func binaryName() string {
	name := filepath.Base(os.Args[0])
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_':
			return r
		}
		return '_'
	}, name)
}
