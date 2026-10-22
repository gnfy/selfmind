package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file prefix hides a file together with the copies made beside it. These
// tests pin how each backend renders it; the real enforcement under
// sandbox-exec is TestSeatbeltHidesConfigurationBackupsAndTemporaryCopies.

func TestSeatbeltProfileDeniesPrivateFilePrefixes(t *testing.T) {
	policy := Policy{
		WritableRoots: []string{"/home/u"},
		Private: PrivateState{
			Files:        []string{"/home/u/.selfmind/config.yaml"},
			FilePrefixes: []string{"/home/u/.selfmind/config.yaml."},
		},
	}
	profile, params, ok := SeatbeltProfile(policy)
	if !ok {
		t.Fatal("a policy with a file prefix must render")
	}
	if want := `(deny file-read* file-write* (prefix (param "PRIVATE_PREFIX_0")))`; !strings.Contains(profile, want) {
		t.Fatalf("missing %s in:\n%s", want, profile)
	}
	if !containsParam(params, "-DPRIVATE_PREFIX_0=/home/u/.selfmind/config.yaml.") {
		t.Fatalf("the prefix must travel as a parameter: %v", params)
	}
	if strings.Contains(profile, "config.yaml") {
		t.Fatalf("private paths must never be interpolated into the policy text:\n%s", profile)
	}
	if strings.Index(profile, "PRIVATE_PREFIX_0") < strings.LastIndex(profile, "(allow ") {
		t.Fatal("the prefix denial must follow every allow rule")
	}
	// The configuration's directory is pinned like any directory above state.
	if !containsParam(params, "-DPRIVATE_ANCESTOR_0=/home/u/.selfmind") {
		t.Fatalf("a prefix's directory must be pinned: %v", params)
	}
}

func TestSeatbeltProfileRefusesUnsoundFilePrefixes(t *testing.T) {
	for name, prefix := range map[string]string{
		"relative":                    ".selfmind/config.yaml.",
		"unclean":                     "/home/u/../u/.selfmind/config.yaml.",
		"a directory, not a name":     "/home/u/.selfmind/",
		"names every sibling at once": "/home/u/.selfmind/.",
	} {
		t.Run(name, func(t *testing.T) {
			// Dropping the entry would run the command with a copy of the
			// configuration in reach; refusing degrades to an approval-gated run.
			if _, _, ok := SeatbeltProfile(Policy{Private: PrivateState{FilePrefixes: []string{prefix}}}); ok {
				t.Fatalf("prefix %q must not render", prefix)
			}
		})
	}
}

func TestWrapArgvMasksTheSiblingsOfAFilePrefixThatExist(t *testing.T) {
	// Not under /tmp: nothing there is visible inside the sandbox, so none of it
	// is masked.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	dir, err := os.MkdirTemp(home, "selfmind-sandbox-prefix-")
	if err != nil {
		t.Skip("cannot create a test directory under the home directory")
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if strings.HasPrefix(dir, "/tmp/") {
		t.Skip("the home directory lies under /tmp")
	}
	for _, name := range []string{"config.yaml", "config.yaml.tmp", "config.yaml.bak-1", "config.yaml-unrelated", "config.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "config.yaml.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	policy := Policy{
		WritableRoots: []string{dir},
		Private:       PrivateState{FilePrefixes: []string{filepath.Join(dir, "config.yaml.")}},
	}
	join := func() string { return strings.Join(WrapArgv("/usr/bin/bwrap", policy, []string{"true"}), " ") }
	got := join()
	for _, want := range []string{
		"--ro-bind /dev/null " + filepath.Join(dir, "config.yaml.tmp"),
		"--ro-bind /dev/null " + filepath.Join(dir, "config.yaml.bak-1"),
		"--tmpfs " + filepath.Join(dir, "config.yaml.d"),
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	// Neither the file itself (a prefix names its siblings; the file is listed
	// separately) nor a name that merely resembles it.
	for _, forbidden := range []string{
		filepath.Join(dir, "config.yaml "), filepath.Join(dir, "config.yaml-unrelated"), filepath.Join(dir, "config.json"),
	} {
		if strings.Contains(got+" ", "--ro-bind /dev/null "+forbidden) {
			t.Fatalf("%q must not be masked: %s", forbidden, got)
		}
	}
	// The expansion happens per command, so a copy made later is masked from the
	// next command on.
	if err := os.WriteFile(filepath.Join(dir, "config.yaml.bak-2"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if later := join(); !strings.Contains(later, "--ro-bind /dev/null "+filepath.Join(dir, "config.yaml.bak-2")) {
		t.Fatalf("a copy written after the first command must be masked by the next: %s", later)
	}
}
