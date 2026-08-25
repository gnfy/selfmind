package cliapp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionCommandExitsWithoutStartingTUI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"selfmind", "--version"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "SelfMind "+Version {
		t.Fatalf("version output = %q", got)
	}
}

func TestUnknownCommandDoesNotStartTUI(t *testing.T) {
	t.Setenv("SELF_USE_GATEWAY", "")
	t.Setenv("SELF_USE_DAEMON", "")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"selfmind", "frobnicate"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `unknown command "frobnicate"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestSplitAddDirFlagsCanonicalizesAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cleaned, dirs, err := splitAddDirFlags([]string{
		"selfmind", "send", "--add-dir", root, "--add-dir=" + alias, "inspect",
	})
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0] != filepath.Clean(want) {
		t.Fatalf("dirs = %#v, want canonical %q", dirs, want)
	}
	if got := strings.Join(cleaned, " "); got != "selfmind send inspect" {
		t.Fatalf("cleaned args = %q", got)
	}
}

func TestSplitAddDirFlagsPreservesLiteralArguments(t *testing.T) {
	cleaned, dirs, err := splitAddDirFlags([]string{"selfmind", "send", "--", "--add-dir", "literal"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 0 {
		t.Fatalf("literal --add-dir was parsed: %#v", dirs)
	}
	if got := strings.Join(cleaned, " "); got != "selfmind send -- --add-dir literal" {
		t.Fatalf("cleaned args = %q", got)
	}
}

func TestSplitAddDirFlagsRejectsNonDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := splitAddDirFlags([]string{"selfmind", "--add-dir", file}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("expected non-directory error, got %v", err)
	}
}
