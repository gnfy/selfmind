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

func TestPruneRetiredGitViewKeepsDeliveredCommitAndReclaimsCheckout(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "prunable", baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), []byte("delivered\n"), 0o600); err != nil {
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
	pruned, err := PruneRetiredGitView(ctx, views, view.ID, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if pruned.RetainedRef != "refs/selfmind/retained/prunable" ||
		baselineGit(t, root, "rev-parse", pruned.RetainedRef) != pruned.HeadCommit {
		t.Fatalf("delivered commit was not protected: %+v", pruned)
	}
	if _, err := os.Lstat(retired.Path); !os.IsNotExist(err) {
		t.Fatalf("retired checkout was not reclaimed: %v", err)
	}
	if _, err := RestoreGitView(ctx, views, view.ID, baseline); err == nil {
		t.Fatal("a pruned checkout was silently rebuilt")
	}
	partial := filepath.Join(filepath.Dir(retired.Path), ".pruning", view.ID)
	if err := os.Mkdir(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "interrupted-delete"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := PruneRetiredGitView(ctx, views, view.ID, baseline); err != nil || again != pruned {
		t.Fatalf("prune retry = %+v %v", again, err)
	}
	if _, err := os.Lstat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial deletion was not recovered: %v", err)
	}
}

func TestPruneRetiredGitViewRefusesHiddenGitWork(t *testing.T) {
	for _, hidden := range []string{"extra-ref", "rewound-commit"} {
		t.Run(hidden, func(t *testing.T) {
			ctx := context.Background()
			root := cleanGitFixture(t)
			baseline, err := InspectCleanGitBaseline(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			views := filepath.Join(t.TempDir(), "views")
			view, err := EnsureGitView(ctx, views, "hidden", baseline)
			if err != nil {
				t.Fatal(err)
			}
			switch hidden {
			case "extra-ref":
				baselineGit(t, view.Path, "branch", "unreviewed")
			case "rewound-commit":
				if err := os.WriteFile(filepath.Join(view.Path, "draft.txt"), []byte("saved only in Git"), 0o600); err != nil {
					t.Fatal(err)
				}
				baselineGit(t, view.Path, "add", "draft.txt")
				baselineGit(t, view.Path, "commit", "-qm", "unreviewed work")
				baselineGit(t, view.Path, "reset", "--hard", baseline.Commit)
			}
			retired, err := RetireGitView(ctx, views, view.ID, baseline)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline); err == nil {
				t.Fatal("hidden Git work was deleted")
			}
			if _, err := os.Stat(retired.Path); err != nil {
				t.Fatalf("rejected view was not preserved: %v", err)
			}
		})
	}
}

func TestPruneRetiredGitViewResumesAfterStagingCrash(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "interrupted-prune", baseline)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := RetireGitView(ctx, views, view.ID, baseline)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Dir(retired.Path)
	staging, err := privateGitViewSubdir(archive, ".pruning")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(retired.Path, filepath.Join(staging, view.ID)); err != nil {
		t.Fatal(err)
	}
	pruned, err := PruneRetiredGitView(ctx, views, view.ID, baseline)
	if err != nil || pruned.HeadCommit != baseline.Commit {
		t.Fatalf("staged prune recovery = %+v %v", pruned, err)
	}
	if _, err := os.Lstat(filepath.Join(staging, view.ID)); !os.IsNotExist(err) {
		t.Fatalf("staged view was not removed: %v", err)
	}
}

func TestPruneRetiredGitViewRefusesReappearingLiveView(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "reappearing", baseline)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := RetireGitView(ctx, views, view.ID, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(view.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline); err == nil {
		t.Fatal("prune ignored a second live path")
	}
	if _, err := os.Stat(retired.Path); err != nil {
		t.Fatalf("archive was lost: %v", err)
	}
}

func TestInspectPrunedGitViewRejectsSymlinkRecord(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "tampered-record", baseline)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RetireGitView(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneRetiredGitView(ctx, views, view.ID, baseline); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(views, ".retired", ".pruned", view.ID+".json")
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(elsewhere, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectPrunedGitView(ctx, views, view.ID, baseline); err == nil {
		t.Fatal("symlink prune record was accepted")
	}
}
