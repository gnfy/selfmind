package executionenv

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDeliveredGitViewRetirementIsReversibleAndBytePreserving(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "retained", baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), []byte("committed work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, view.Path, "add", "file.txt")
	baselineGit(t, view.Path, "commit", "-qm", "isolated work")
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	retired, err := RetireGitView(ctx, views, view.ID, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if retired.Path == view.Path {
		t.Fatal("retirement did not move the view")
	}
	if _, err := os.Lstat(view.Path); !os.IsNotExist(err) {
		t.Fatalf("live path was not cleared: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(retired.Path, "file.txt")); err != nil || string(data) != "committed work\n" {
		t.Fatalf("retained file changed: %q %v", data, err)
	}
	if again, err := RetireGitView(ctx, views, view.ID, baseline); err != nil || again.Path != retired.Path {
		t.Fatalf("retirement retry = %+v %v", again, err)
	}
	restored, err := RestoreGitView(ctx, views, view.ID, baseline)
	if err != nil || restored.Path != view.Path {
		t.Fatalf("restore = %+v %v", restored, err)
	}
	if _, err := InspectGitView(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
		t.Fatalf("restored delivery changed: %v", err)
	}
}

func TestGitViewRetirementKeepsUndeliveredAndIgnoredWorkAtLivePath(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "unfinished", baseline)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(view.Path, "draft.txt")
	if err := os.WriteFile(path, []byte("do not lose"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireGitView(ctx, views, view.ID, baseline); err == nil {
		t.Fatal("undelivered draft was retired")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "do not lose" {
		t.Fatalf("draft was lost: %q %v", data, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, ".gitignore"), []byte("cache/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, view.Path, "add", ".gitignore")
	baselineGit(t, view.Path, "commit", "-qm", "ignore cache")
	if err := os.Mkdir(filepath.Join(view.Path, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	ignored := filepath.Join(view.Path, "cache", "state")
	if err := os.WriteFile(ignored, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	if _, err := RetireGitView(ctx, views, view.ID, baseline); err == nil {
		t.Fatal("ignored state was retired without review")
	}
	if data, err := os.ReadFile(ignored); err != nil || string(data) != "keep" {
		t.Fatalf("ignored data lost: %q %v", data, err)
	}
}

func TestGitViewRetirementRefusesUndeliveredCommit(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "not-delivered", baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), []byte("committed but not delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, view.Path, "add", "file.txt")
	baselineGit(t, view.Path, "commit", "-qm", "isolated work")
	if _, err := RetireGitView(ctx, views, view.ID, baseline); err == nil {
		t.Fatal("undelivered commit was retired")
	}
	if data, err := os.ReadFile(filepath.Join(view.Path, "file.txt")); err != nil || string(data) != "committed but not delivered\n" {
		t.Fatalf("undelivered view was lost: %q %v", data, err)
	}
}
