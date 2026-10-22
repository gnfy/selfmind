package testhome

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/platform/config"
)

// startHome is HOME as this binary started; package variables are set before
// TestMain runs.
var startHome = os.Getenv("HOME")

func TestMain(m *testing.M) { Main(m) }

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func TestBinaryRunsInAScratchHome(t *testing.T) {
	home := os.Getenv("HOME")
	if home == "" || home == startHome {
		t.Fatalf("HOME = %q; want a scratch home, not the starting HOME %q", home, startHome)
	}
	if !strings.HasPrefix(filepath.Base(home), "selfmind-test-") {
		t.Fatalf("HOME = %q; want a directory named for its test binary", home)
	}
	if got, err := os.UserHomeDir(); err != nil || got != home {
		t.Fatalf("UserHomeDir = %q, %v; want %q", got, err, home)
	}
	if got, err := os.UserCacheDir(); err != nil || !within(home, got) {
		t.Fatalf("UserCacheDir = %q, %v; want a directory inside %s", got, err, home)
	}
	if got, err := os.UserConfigDir(); err != nil || !within(home, got) {
		t.Fatalf("UserConfigDir = %q, %v; want a directory inside %s", got, err, home)
	}
	for _, key := range []string{"XDG_DATA_HOME", "XDG_STATE_HOME"} {
		if got := os.Getenv(key); !within(home, got) {
			t.Fatalf("%s = %q; want a directory inside %s", key, got, home)
		}
	}
	if path, isDefault := config.ResolveConfigPath(""); !isDefault || !within(home, path) {
		t.Fatalf("config resolves to %q (default=%v); want the per-user default inside %s", path, isDefault, home)
	}
}

// Go's own caches are not SelfMind state: a child `go` keeps using them rather
// than rebuilding into the scratch home.
func TestGoKeepsItsOwnCachesOutsideTheScratchHome(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go tool unavailable")
	}
	home := os.Getenv("HOME")
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOENV"} {
		value := os.Getenv(key)
		if value == "" || within(home, value) {
			t.Fatalf("%s = %q; want the original location pinned outside the scratch home %s", key, value, home)
		}
	}
}

// A test that points HOME somewhere else still can, and the binary's scratch
// home is back when it finishes.
func TestSetenvStillOverridesHome(t *testing.T) {
	scratch := os.Getenv("HOME")
	t.Run("override", func(t *testing.T) {
		other := t.TempDir()
		t.Setenv("HOME", other)
		if got, err := os.UserHomeDir(); err != nil || got != other {
			t.Fatalf("UserHomeDir = %q, %v; want %q", got, err, other)
		}
	})
	if got := os.Getenv("HOME"); got != scratch {
		t.Fatalf("HOME = %q after the override; want the scratch home %q", got, scratch)
	}
}

// runHelper starts this test binary again as TestHelperReportsHome with
// environ and returns the HOME the helper saw.
func runHelper(t *testing.T, environ []string, mode string) string {
	t.Helper()
	report := filepath.Join(t.TempDir(), "helper-home")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperReportsHome$", "-test.count=1")
	cmd.Env = append(environ, "TESTHOME_HELPER_REPORT="+report, "TESTHOME_HELPER_MODE="+mode)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper binary failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("helper did not report its HOME: %v", err)
	}
	return strings.TrimSpace(string(data))
}

// A binary that `go test` starts makes its own scratch home, and the home
// disappears with it together with whatever the tests left there, including
// a read-only tree such as a Go module cache.
func TestMainRemovesTheScratchHomeAtExit(t *testing.T) {
	var fresh []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, ownerVar+"=") {
			fresh = append(fresh, entry)
		}
	}
	childHome := runHelper(t, fresh, "leave-state")
	if childHome == "" || childHome == os.Getenv("HOME") || childHome == startHome {
		t.Fatalf("helper HOME = %q; want its own scratch home", childHome)
	}
	if _, err := os.Lstat(childHome); !os.IsNotExist(err) {
		t.Fatalf("helper scratch home %s survived its binary (err=%v)", childHome, err)
	}
}

// A helper process that re-executes this binary keeps the environment it was
// started with, whatever HOME that is, and makes no home that its os.Exit
// would strand.
func TestHelperProcessesKeepTheEnvironmentTheyInherit(t *testing.T) {
	scratch := os.Getenv("HOME")
	if got := runHelper(t, os.Environ(), "exit"); got != scratch {
		t.Fatalf("helper HOME = %q; want the owner's scratch home %q", got, scratch)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("the owner's scratch home must outlive its helper: %v", err)
	}
	// Stdio helpers start through the tool process environment, which strips
	// SelfMind's control-plane variables.
	toolEnv := executionenv.BuildProcessEnv(os.Environ(), executionenv.ProcessEnvPolicy{StripControlPlane: true})
	if got := runHelper(t, toolEnv, "exit"); got != scratch {
		t.Fatalf("helper started through a tool environment: HOME = %q; want %q", got, scratch)
	}
	chosen := t.TempDir()
	t.Setenv("HOME", chosen)
	if got := runHelper(t, os.Environ(), "exit"); got != chosen {
		t.Fatalf("helper HOME = %q; want the HOME its test chose %q", got, chosen)
	}
}

func TestHelperReportsHome(t *testing.T) {
	report := os.Getenv("TESTHOME_HELPER_REPORT")
	if report == "" {
		t.Skip("helper process for the tests above")
	}
	home := os.Getenv("HOME")
	if os.Getenv("TESTHOME_HELPER_MODE") == "leave-state" {
		leaveState(t, home)
	}
	if err := os.WriteFile(report, []byte(home), 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TESTHOME_HELPER_MODE") == "exit" {
		// As a fake CLI or stdio server does: no deferred cleanup runs.
		os.Exit(0)
	}
}

func leaveState(t *testing.T, home string) {
	t.Helper()
	state := filepath.Join(home, ".selfmind", "learning")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "learning-log.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(home, "go", "pkg", "mod", "example.com@v1.0.0")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "go.mod"), []byte("module example.com\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
}
