package executionenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func baselineGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = cleanGitProbeEnv(os.Environ())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func cleanGitFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	baselineGit(t, root, "init", "-q")
	baselineGit(t, root, "config", "user.name", "Test")
	baselineGit(t, root, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, root, "add", "file.txt")
	baselineGit(t, root, "commit", "-qm", "base")
	return root
}

func TestInspectCleanGitBaselinePinsCommitAndPhysicalIdentity(t *testing.T) {
	root := cleanGitFixture(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	baseline, err := InspectCleanGitBaseline(context.Background(), alias)
	if err != nil {
		t.Fatal(err)
	}
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Root != physicalRoot || baseline.Commit != baselineGit(t, root, "rev-parse", "HEAD") || baseline.CommonDir != filepath.Join(physicalRoot, ".git") {
		t.Fatalf("baseline = %+v", baseline)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectCleanGitBaseline(context.Background(), root); !errors.Is(err, ErrGitViewUnavailable) {
		t.Fatalf("dirty tracked file: %v", err)
	}
}

func TestInspectCleanGitBaselineFailsClosed(t *testing.T) {
	root := cleanGitFixture(t)
	for _, path := range []string{"", filepath.Join(root, "missing"), filepath.Join(root, ".git"), t.TempDir()} {
		if _, err := InspectCleanGitBaseline(context.Background(), path); !errors.Is(err, ErrGitViewUnavailable) {
			t.Fatalf("path %q: %v", path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("untracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectCleanGitBaseline(context.Background(), root); !errors.Is(err, ErrGitViewUnavailable) {
		t.Fatalf("untracked file: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "new.txt")); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectCleanGitBaseline(cancelled, root); !errors.Is(err, ErrGitViewUnavailable) {
		t.Fatalf("cancelled inspection: %v", err)
	}
}

func TestInspectCleanGitBaselineRejectsLinkedWorktreeAndSubmodule(t *testing.T) {
	root := cleanGitFixture(t)
	view := filepath.Join(t.TempDir(), "view")
	baselineGit(t, root, "worktree", "add", "-q", "--detach", view, "HEAD")
	if _, err := InspectCleanGitBaseline(context.Background(), view); !errors.Is(err, ErrGitViewUnavailable) {
		t.Fatalf("linked worktree: %v", err)
	}
	child := cleanGitFixture(t)
	baselineGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", "-q", child, "sub")
	baselineGit(t, root, "commit", "-qam", "add submodule")
	if _, err := InspectCleanGitBaseline(context.Background(), root); !errors.Is(err, ErrGitViewUnavailable) {
		t.Fatalf("submodule: %v", err)
	}
}
