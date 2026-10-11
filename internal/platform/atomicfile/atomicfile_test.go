package atomicfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range list {
		names = append(names, entry.Name())
	}
	return names
}

func TestWriteReplacesTheFileAtomicallyWithTheRequestedMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("new-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new-secret" {
		t.Fatalf("content = %q, err = %v", got, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	if names := entries(t, dir); len(names) != 1 {
		t.Fatalf("no temporary file may be left behind: %v", names)
	}
}

// The attack: a command that can write the directory prepares the daemon's
// predictable temporary name before the daemon writes a refresh token.
func TestWriteNeverWritesThroughAPlantedSymlinkAtTheOldTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	attacker := filepath.Join(dir, "attacker-owned")
	if err := os.WriteFile(attacker, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, planted := range []string{path + ".tmp", path + ".lock", path + ".1.tmp"} {
		if err := os.Symlink(attacker, planted); err != nil {
			t.Fatal(err)
		}
	}
	if err := Write(path, []byte("refresh-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(attacker); string(got) != "untouched" {
		t.Fatalf("the secret was written through a planted symlink: %q", got)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the destination must be a regular file, not a link to the attacker's: %v %v", info, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "refresh-token" {
		t.Fatalf("destination = %q", got)
	}
}

// The other half: a file the command already holds open at the old temporary
// name would become the destination, readable through its descriptor forever.
func TestWriteNeverAdoptsAFilePlantedAtTheOldTempName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	planted := path + ".tmp"
	if err := os.WriteFile(planted, []byte("attacker-held"), 0o666); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(planted)
	if err := Write(path, []byte("secrets"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("the destination is the file the command planted")
	}
	if got, _ := os.ReadFile(planted); string(got) != "attacker-held" {
		t.Fatalf("the planted file was written to: %q", got)
	}
}

func TestWriteReplacesASymlinkDestinationInsteadOfWritingThroughIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles-auth.json")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "auth.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(path); info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("a symlink destination is replaced by the new file")
	}
	if got, _ := os.ReadFile(target); string(got) != "old" {
		t.Fatalf("the link's old target must be left alone: %q", got)
	}
}

func TestWriteLeavesNothingBehindWhenItFails(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory cannot be replaced by a file.
	blocked := filepath.Join(dir, "auth.json")
	if err := os.MkdirAll(filepath.Join(blocked, "inside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(blocked, []byte("x"), 0o600); err == nil {
		t.Fatal("replacing a non-empty directory must fail")
	}
	if names := entries(t, dir); len(names) != 1 || names[0] != "auth.json" {
		t.Fatalf("a failed write left files behind: %v", names)
	}
	if err := Write(filepath.Join(dir, "missing", "auth.json"), []byte("x"), 0o600); err == nil {
		t.Fatal("a missing directory must fail")
	}
	if err := Write("", []byte("x"), 0o600); err == nil {
		t.Fatal("an empty path must fail")
	}
}

func TestTemporaryFilesBeginWithTheDestinationSoAPrivatePrefixCoversThem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	temp, err := newTemp(path)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(temp.Name())
	temp.Close()
	if !strings.HasPrefix(temp.Name(), TempName(path)) || !strings.HasSuffix(temp.Name(), ".tmp") {
		t.Fatalf("temporary name %q must begin with %q", temp.Name(), TempName(path))
	}
	// A bare relative path works too.
	t.Chdir(dir)
	if err := Write("relative.json", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}
