package cliapp

import (
	"bytes"
	"context"
	"io/fs"
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

// freshUserHome points HOME at an empty directory, clears SELF_CONFIG, and
// registers restoration of the variables the root command exports from a
// loaded config, so a Run inside a test leaves the process environment as it
// found it.
func freshUserHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SELF_CONFIG", "")
	for _, key := range []string{
		"SELF_GATEWAY_ADDR", "SELF_GATEWAY_URL", "SELF_GATEWAY_TOKEN", "SELF_GATEWAY_DRAIN_TIMEOUT",
		"SELFMIND_FLIGHT_RECORDER", "SELFMIND_FLIGHT_DIR", "SELFMIND_FLIGHT_KEEP",
	} {
		t.Setenv(key, "")
	}
	return home
}

// userState lists, relative to home, what a command left there, apart from
// the Go tool's own per-user directory: selfcheck runs `go`, which keeps its
// telemetry there as it does for any developer.
func userState(t *testing.T, home string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(home, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(home, path)
		switch rel = filepath.ToSlash(rel); rel {
		case ".", "Library", "Library/Application Support", ".config":
			return nil
		case "Library/Application Support/go", ".config/go":
			return fs.SkipDir
		}
		found = append(found, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// Developer-only commands inspect the source checkout. On a machine where the
// person has no SelfMind state yet, they must not create any: no first-run
// config, under the default path or an explicit one, and nothing else under
// HOME — including from selfcheck's replay of the eval corpus.
func TestDeveloperCommandsCreateNoUserState(t *testing.T) {
	evalDir := t.TempDir()
	providerless := `id: no_user_state
title: developer command creates no user state
suite: selfcheck-test
workspace: isolated
channel: cli
model_required: false
turns:
  - input: "/new no user state"
expect:
  status: completed
`
	if err := os.WriteFile(filepath.Join(evalDir, "case.yaml"), []byte(providerless), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SELFMIND_EVAL_VCR_DIR", filepath.Join(t.TempDir(), "vcr"))
	t.Setenv("SELFMIND_EVAL_MIN_CASES", "")
	for _, command := range [][]string{
		{"docs", "check"},
		{"selfcheck", "--skip-go", "--skip-eval"},
		{"selfcheck", "--skip-go", "--eval-dir", evalDir},
	} {
		for _, explicit := range []bool{false, true} {
			home := freshUserHome(t)
			missing := filepath.Join(t.TempDir(), "absent", "config.yaml")
			args := []string{"selfmind"}
			if explicit {
				args = append(args, "--config", missing)
			}
			args = append(args, command...)
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), args, strings.NewReader(""), &stdout, &stderr)
			if names := userState(t, home); len(names) != 0 {
				t.Fatalf("%v created user state under HOME: %v\ncode=%d stdout:\n%s\nstderr:\n%s", args[1:], names, code, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(missing); !os.IsNotExist(err) {
				t.Fatalf("%v created the missing config %s (err=%v)", args[1:], missing, err)
			}
			if code != 0 {
				t.Fatalf("%v exit %d\nstdout:\n%s\nstderr:\n%s", args[1:], code, stdout.String(), stderr.String())
			}
		}
	}
}

// Ordinary product commands keep their first-run behavior: the first one a
// person runs creates the default config.
func TestOrdinaryCommandStillCreatesFirstRunConfig(t *testing.T) {
	home := freshUserHome(t)
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"selfmind", "config", "doctor"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("config doctor exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".selfmind", "config.yaml")); err != nil {
		t.Fatalf("first run must create the default config: %v", err)
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
