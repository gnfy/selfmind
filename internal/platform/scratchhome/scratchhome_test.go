package scratchhome

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"selfmind/internal/executionenv"
	"selfmind/internal/platform/config"
)

// withEnv returns environ with each key replaced by its override; an empty
// override is what the Go tool reads as unset.
func withEnv(environ []string, overrides map[string]string) []string {
	out := make([]string, 0, len(environ)+len(overrides))
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			out = append(out, entry)
		}
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}

// files lists the regular files under home, relative to it.
func files(t *testing.T, home string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			rel, _ := filepath.Rel(home, path)
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// startsClean reports whether home holds nothing but Go's telemetry setting.
func startsClean(t *testing.T, home string) bool {
	got := files(t, home)
	return len(got) == 1 && strings.HasSuffix(got[0], "go/telemetry/mode")
}

// A scratch home holds whatever tests put under HOME, including an execution
// runtime root. That root refuses the system temp tree, because a sandbox
// binds a private /tmp over it, so a home created there would quietly turn
// every sandbox test into a skip.
func TestNewHomeHoldsAnExecutionRuntimeRoot(t *testing.T) {
	if info, err := os.Stat("/var/tmp"); err != nil || !info.IsDir() || within(os.TempDir(), "/var/tmp") {
		t.Skip("no persistent temp directory outside the system temp tree")
	}
	for _, prefix := range []string{"scratchhome-test-", "scratchhome-other-"} {
		home, err := New(prefix)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = Remove(home) })
		if !strings.HasPrefix(filepath.Base(home), prefix) {
			t.Fatalf("home %s does not carry prefix %q", home, prefix)
		}
		if !startsClean(t, home) {
			t.Fatalf("new home must hold only Go's telemetry setting: %q", files(t, home))
		}
		if err := executionenv.SetRuntimeRoot(filepath.Join(home, ".selfmind-test-runtime", "probe")); err != nil {
			t.Fatalf("a runtime root under the scratch home was refused: %v", err)
		}
	}
}

func TestEnvRootsEveryPerUserDirectoryInHome(t *testing.T) {
	for _, home := range []string{t.TempDir(), filepath.Join(t.TempDir(), "nested", "home")} {
		// The operator's own relocations must not survive into the scratch view.
		t.Setenv("SELF_CONFIG", "/operator/.selfmind/config.yaml")
		t.Setenv("XDG_CACHE_HOME", "/operator/.cache")
		t.Setenv("XDG_CONFIG_HOME", "/operator/.config")
		for _, assignment := range Env(home) {
			key, value, ok := strings.Cut(assignment, "=")
			if !ok {
				t.Fatalf("malformed assignment %q", assignment)
			}
			t.Setenv(key, value)
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
		if path, isDefault := config.ResolveConfigPath(""); !isDefault || path != filepath.Join(home, ".selfmind", "config.yaml") {
			t.Fatalf("config resolves to %q (default=%v); want the per-user default inside %s", path, isDefault, home)
		}
	}
}

// stableName returns a name for a stable home that no other test or run
// uses, and removes what a test leaves at that path and its lock.
func stableName(t *testing.T, label string) string {
	t.Helper()
	name := "scratchhome-" + label + "-" + strconv.Itoa(os.Getpid())
	t.Cleanup(func() {
		path := filepath.Join(Parent(), name)
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink == 0 {
			_ = Remove(path)
		} else if err == nil {
			_ = os.Remove(path)
		}
		_ = os.Remove(path + ".lock")
	})
	return name
}

func claim(t *testing.T, name string) *Claim {
	t.Helper()
	got, err := Fresh(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = got.Release() })
	return got
}

// Fresh keeps one path across runs, so cache keys that record HOME stay
// stable, and starts every run empty, even after an interrupted run left its
// own tree there, including one its owner cannot write.
func TestFreshKeepsItsPathAndStartsEmpty(t *testing.T) {
	name := stableName(t, "fresh")
	stale := filepath.Join(Parent(), name, ".selfmind", "stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "state.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stale, 0o500); err != nil {
		t.Fatal(err)
	}

	first := claim(t, name)
	if first.Fallback != "" || first.Busy {
		t.Fatalf("an interrupted run's own home must be reused: %+v", first)
	}
	if !startsClean(t, first.Home) {
		t.Fatalf("Fresh must start clean: %q", files(t, first.Home))
	}
	if info, err := os.Stat(first.Home); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("Fresh home must be private: info=%v err=%v", info, err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if second := claim(t, name); second.Home != first.Home || second.Fallback != "" {
		t.Fatalf("Fresh moved from %s to %+v; the path must stay stable", first.Home, second)
	}
}

// A predictable name in a shared directory can be planted before a run
// starts. Whatever is planted there — a symbolic link, a file, another
// user's directory — stays untouched, and the run gets a home of its own.
func TestFreshLeavesWhatOthersPlantAtTheStablePathAlone(t *testing.T) {
	me := uint32(os.Getuid())
	for _, plant := range []string{"symlink to a directory", "dangling symlink", "file", "another user's directory"} {
		t.Run(plant, func(t *testing.T) {
			name := stableName(t, "planted")
			stable := filepath.Join(Parent(), name)
			victim := t.TempDir()
			sentinel := filepath.Join(victim, "keep")
			if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			ownerOf := fileOwner
			switch plant {
			case "symlink to a directory":
				if err := os.Symlink(victim, stable); err != nil {
					t.Fatal(err)
				}
			case "dangling symlink":
				if err := os.Symlink(filepath.Join(victim, "missing"), stable); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(stable, []byte("planted"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "another user's directory":
				if err := os.Mkdir(stable, 0o700); err != nil {
					t.Fatal(err)
				}
				sentinel = filepath.Join(stable, "keep")
				if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				// The planted directory reads as another uid's; everything
				// else keeps its real owner.
				ownerOf = func(info fs.FileInfo) (uint32, bool) {
					if info.Name() == name {
						return me + 1, true
					}
					return fileOwner(info)
				}
			}
			before, err := os.Lstat(stable)
			if err != nil {
				t.Fatal(err)
			}

			got, err := fresh(Parent(), name, me, ownerOf)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = got.Release() })
			if got.Fallback == "" || got.Home == stable || strings.HasPrefix(got.Home, stable+string(filepath.Separator)) {
				t.Fatalf("a planted %s was adopted: %+v", plant, got)
			}
			t.Logf("fallback: %s", got.Fallback)
			after, err := os.Lstat(stable)
			if err != nil || after.Mode() != before.Mode() {
				t.Fatalf("the planted %s was changed: before=%v after=%v err=%v", plant, before.Mode(), after, err)
			}
			if _, err := os.Stat(sentinel); err != nil {
				t.Fatalf("what the planted %s pointed at was removed: %v", plant, err)
			}
			if err := got.Release(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(got.Home); !os.IsNotExist(err) {
				t.Fatalf("the fallback home %s survived Release (err=%v)", got.Home, err)
			}
		})
	}
}

// The lock file has a predictable name too: Fresh does not follow a link
// planted there or lock what it did not create.
func TestFreshRefusesALockFileItDoesNotOwn(t *testing.T) {
	for _, plant := range []string{"symlink", "directory"} {
		t.Run(plant, func(t *testing.T) {
			name := stableName(t, "lock")
			lock := filepath.Join(Parent(), name+".lock")
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch plant {
			case "symlink":
				if err := os.Symlink(target, lock); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(lock, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			got := claim(t, name)
			if got.Fallback == "" || got.Home == filepath.Join(Parent(), name) {
				t.Fatalf("a planted lock %s was used: %+v", plant, got)
			}
			t.Logf("fallback: %s", got.Fallback)
			if data, err := os.ReadFile(target); err != nil || string(data) != "keep" {
				t.Fatalf("the link target changed: %q %v", data, err)
			}
		})
	}
}

// The stable path lives in a shared directory only when that directory keeps
// other users from replacing what this user creates there: sticky, and owned
// by root or this user.
func TestFreshTrustsOnlyAStickyParent(t *testing.T) {
	me := uint32(os.Getuid())
	for _, tc := range []struct {
		mode   os.FileMode
		stable bool
	}{
		{0o777, false},
		{0o777 | os.ModeSticky, true},
	} {
		parent := filepath.Join(t.TempDir(), "shared")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, tc.mode); err != nil {
			t.Fatal(err)
		}
		got, err := fresh(parent, "home", me, fileOwner)
		if err != nil {
			t.Fatal(err)
		}
		inParent := strings.HasPrefix(got.Home, mustCanonical(t, parent)+string(filepath.Separator))
		if tc.stable && (got.Fallback != "" || !inParent) {
			t.Fatalf("mode %v: want the stable home in the sticky parent, got %+v", tc.mode, got)
		}
		if !tc.stable && (got.Fallback == "" || inParent) {
			t.Fatalf("mode %v: want a home outside the untrusted parent, got %+v", tc.mode, got)
		}
		if err := got.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

func mustCanonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// Two runs that ask for the same stable home at once never share it: the
// second gets a home of its own and says why, the first keeps its home
// intact, and the stable path is free again once the first releases it.
func TestFreshGivesAConcurrentClaimItsOwnHome(t *testing.T) {
	name := stableName(t, "concurrent")
	first := claim(t, name)
	if first.Busy || first.Fallback != "" {
		t.Fatalf("the first claim must get the stable home: %+v", first)
	}
	sentinel := filepath.Join(first.Home, ".selfmind-run", "state")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}

	second := claim(t, name)
	if !second.Busy || second.Home == first.Home {
		t.Fatalf("a concurrent claim must get its own home: first=%+v second=%+v", first, second)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("the concurrent claim disturbed the first run's home: %v", err)
	}
	// Releasing the second claim gives up only its own home.
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	if third := claim(t, name); !third.Busy {
		t.Fatalf("the first run still holds the stable home: %+v", third)
	}
	lock := filepath.Join(Parent(), name+".lock")
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(lock); !os.IsNotExist(err) {
		t.Fatalf("release must not leave the lock file behind (err=%v)", err)
	}
	if fourth := claim(t, name); fourth.Busy || fourth.Home != first.Home {
		t.Fatalf("the stable home must be free after release: %+v", fourth)
	}
}

// A Go module cache is read-only by design; a home a tool filled with one
// must still be removable.
func TestRemoveDeletesReadOnlyTrees(t *testing.T) {
	for _, depth := range []string{"mod", filepath.Join("go", "pkg", "mod", "example.com@v1.0.0")} {
		home, err := New("scratchhome-remove-")
		if err != nil {
			t.Fatal(err)
		}
		locked := filepath.Join(home, depth)
		if err := os.MkdirAll(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(locked, "go.mod"), []byte("module example.com\n"), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o555); err != nil {
			t.Fatal(err)
		}
		if err := Remove(home); err != nil {
			t.Fatalf("Remove(%s): %v", home, err)
		}
		if _, err := os.Lstat(home); !os.IsNotExist(err) {
			_ = os.Chmod(locked, 0o700)
			_ = os.RemoveAll(home)
			t.Fatalf("home still present after Remove (err=%v)", err)
		}
	}
}

// A `go` run under a scratch home records no telemetry there and starts no
// detached upload process that could recreate the home after Remove.
func TestScratchHomesKeepGoTelemetryOff(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool unavailable")
	}
	created, err := New("scratchhome-telemetry-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Remove(created) })
	stable := claim(t, stableName(t, "telemetry"))
	for _, home := range []string{created, stable.Home} {
		overrides := map[string]string{}
		for _, assignment := range Env(home) {
			key, value, _ := strings.Cut(assignment, "=")
			overrides[key] = value
		}
		cmd := exec.Command(goBin, "env", "GOTELEMETRY")
		cmd.Env = withEnv(os.Environ(), overrides)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go env GOTELEMETRY: %v", err)
		}
		if mode := strings.TrimSpace(string(out)); mode != "off" {
			t.Fatalf("GOTELEMETRY = %q under %s; want off", mode, home)
		}
		if !startsClean(t, home) {
			t.Fatalf("go wrote telemetry into the scratch home: %q", files(t, home))
		}
	}
}

// GoDirs answers for the environment it is given — the original one, before
// HOME moves — whether the locations are explicit or derived from HOME.
func TestGoDirsReportsTheEnvironmentItIsGiven(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool unavailable")
	}
	base := t.TempDir()
	explicit := withEnv(os.Environ(), map[string]string{
		"GOCACHE":    filepath.Join(base, "cache"),
		"GOPATH":     filepath.Join(base, "gopath"),
		"GOMODCACHE": "",
		"GOENV":      filepath.Join(base, "goenv"),
	})
	got, err := GoDirs(context.Background(), goBin, explicit)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{
		"GOCACHE=" + filepath.Join(base, "cache"),
		"GOENV=" + filepath.Join(base, "goenv"),
		"GOMODCACHE=" + filepath.Join(base, "gopath", "pkg", "mod"),
		"GOPATH=" + filepath.Join(base, "gopath"),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("GoDirs = %q; want %q", got, want)
	}

	home, err := New("scratchhome-godirs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Remove(home) })
	derived := withEnv(os.Environ(), map[string]string{
		"HOME": home, "XDG_CACHE_HOME": "", "XDG_CONFIG_HOME": "",
		"GOCACHE": "", "GOPATH": "", "GOMODCACHE": "", "GOENV": "",
	})
	got, err = GoDirs(context.Background(), goBin, derived)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("GoDirs = %q; want four locations", got)
	}
	for _, assignment := range got {
		key, value, _ := strings.Cut(assignment, "=")
		if !within(home, value) {
			t.Fatalf("%s = %q; want it derived from HOME %s", key, value, home)
		}
	}
}
