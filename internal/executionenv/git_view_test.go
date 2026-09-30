package executionenv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureGitViewIsIdempotentAndDoesNotWriteOriginal(t *testing.T) {
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	first, err := EnsureGitView(context.Background(), views, "run-A", baseline)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureGitView(context.Background(), views, "run-A", baseline)
	if err != nil || second != first {
		t.Fatalf("idempotent view: %+v, %v", second, err)
	}
	if err := os.WriteFile(filepath.Join(first.Path, "file.txt"), []byte("view only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, first.Path, "add", "file.txt")
	baselineGit(t, first.Path, "commit", "-qm", "view commit")
	original, err := os.ReadFile(filepath.Join(root, "file.txt"))
	if err != nil || string(original) != "base\n" {
		t.Fatalf("original checkout changed: %q, %v", original, err)
	}
	if resumed, err := EnsureGitView(context.Background(), views, "run-A", baseline); err != nil || resumed != first {
		t.Fatalf("run work must survive idempotent registration: %+v, %v", resumed, err)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatalf("failed inspection removed user work: %v", err)
	}
}

func TestNewGitViewKeepsReachableBaselineWithoutSourceAlternates(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	view, err := EnsureGitView(ctx, filepath.Join(t.TempDir(), "views"), "independent", baseline)
	if err != nil {
		t.Fatal(err)
	}
	alternates := filepath.Join(view.Path, ".git", "objects", "info", "alternates")
	hidden := alternates + ".test-hidden"
	if err := os.Rename(alternates, hidden); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(hidden, alternates) })
	if _, err := gitViewCommand(ctx, view.Path, "cat-file", "-e", baseline.Commit+"^{commit}"); err != nil {
		t.Fatalf("view still depended on source objects: %v", err)
	}
	if _, err := gitViewCommand(ctx, view.Path, "fsck", "--no-reflogs", "--connectivity-only"); err != nil {
		t.Fatalf("view baseline graph was incomplete: %v", err)
	}
}

func TestEnsureGitViewDoesNotRunCheckoutHookOrFilter(t *testing.T) {
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(root, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureGitView(context.Background(), filepath.Join(t.TempDir(), "views"), "hook-test", baseline); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Git checkout hook executed while creating a view: %v", err)
	}
	baselineGit(t, root, "config", "filter.sideeffect.smudge", "touch "+marker)
	if _, err := InspectCleanGitBaseline(context.Background(), root); !errors.Is(err, ErrGitViewUnavailable) {
		t.Fatalf("executable filter admitted as clean baseline: %v", err)
	}
	if _, err := EnsureGitView(context.Background(), filepath.Join(t.TempDir(), "views"), "filter-test", baseline); !errors.Is(err, ErrGitViewUnavailable) {
		t.Fatalf("executable filter admitted at materialization: %v", err)
	}
}

func TestEnsureGitViewRejectsCollisionAndInvalidIdentity(t *testing.T) {
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	for _, id := range []string{"", "../escape", "a/b", ".", "run A"} {
		if _, err := EnsureGitView(context.Background(), views, id, baseline); !errors.Is(err, ErrGitViewUnavailable) {
			t.Fatalf("ID %q accepted: %v", id, err)
		}
	}
	path := filepath.Join(views, "collision")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "user-file"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureGitView(context.Background(), views, "collision", baseline); !errors.Is(err, ErrGitViewUnavailable) {
		t.Fatalf("occupied path accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "user-file")); err != nil {
		t.Fatalf("collision destroyed existing work: %v", err)
	}
}

func TestInspectGitViewParksMissingOrChangedViewWithoutRecreatingIt(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "run-A", baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, "uncommitted.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if inspected, err := InspectGitView(ctx, views, view.ID, baseline); err != nil || inspected != view {
		t.Fatalf("unchanged view failed inspection: %+v, %v", inspected, err)
	}
	alternates := filepath.Join(view.Path, ".git", "objects", "info", "alternates")
	if err := os.WriteFile(alternates, []byte("/other/repository/objects\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectGitView(ctx, views, view.ID, baseline); !errors.Is(err, ErrGitViewUnavailable) {
		t.Fatalf("changed source repository binding accepted: %v", err)
	}
	if contents, err := os.ReadFile(filepath.Join(view.Path, "uncommitted.txt")); err != nil || string(contents) != "keep" {
		t.Fatalf("failed validation changed the user's work: %q, %v", contents, err)
	}
	if err := os.RemoveAll(view.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectGitView(ctx, views, view.ID, baseline); !errors.Is(err, ErrGitViewUnavailable) {
		t.Fatalf("missing view was silently recreated: %v", err)
	}
	if _, err := os.Stat(view.Path); !os.IsNotExist(err) {
		t.Fatalf("inspection recreated a missing checkout: %v", err)
	}
}

func TestEnsureGitViewRejectsStorageInsideSourceWithoutTouchingIt(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(root, "managed-views")
	if _, err := EnsureGitView(ctx, views, "run-A", baseline); !errors.Is(err, ErrGitViewUnavailable) {
		t.Fatalf("nested view storage accepted: %v", err)
	}
	if _, err := os.Stat(views); !os.IsNotExist(err) {
		t.Fatalf("rejected view storage changed the original checkout: %v", err)
	}
}
