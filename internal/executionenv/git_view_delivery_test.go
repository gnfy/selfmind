package executionenv

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGitViewDeliveryCreatesBranchWithoutChangingSourceCheckout(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "run-one", baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), []byte("from view\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, view.Path, "add", "file.txt")
	baselineGit(t, view.Path, "commit", "-qm", "change in view")
	preview, err := InspectGitViewDelivery(ctx, views, view.ID, baseline)
	if err != nil || preview.CommittedFiles != 1 || preview.Uncommitted != 0 || preview.Untracked != 0 {
		t.Fatalf("view preview: %+v %v", preview, err)
	}
	delivered, err := DeliverGitViewBranch(ctx, views, view.ID, baseline)
	if err != nil || delivered.DeliveredBranch != "selfmind/run-one" || delivered.HeadCommit == baseline.Commit {
		t.Fatalf("branch delivery: %+v %v", delivered, err)
	}
	if got := baselineGit(t, root, "rev-parse", "HEAD"); got != baseline.Commit {
		t.Fatalf("source HEAD changed: %s", got)
	}
	if got, err := os.ReadFile(filepath.Join(root, "file.txt")); err != nil || string(got) != "base\n" {
		t.Fatalf("source checkout changed: %q %v", got, err)
	}
	if got := baselineGit(t, root, "rev-parse", delivered.DeliveredBranch); got != delivered.HeadCommit {
		t.Fatalf("delivered branch changed: %s", got)
	}
	if again, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err != nil || again.DeliveredBranch != delivered.DeliveredBranch {
		t.Fatalf("repeat was not idempotent: %+v %v", again, err)
	}
}

func TestGitViewDeliveryRejectsUncommittedButKeepsAdvancedSourceCheckout(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "run-two", baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, view.Path, "add", "file.txt")
	baselineGit(t, view.Path, "commit", "-qm", "change")
	if err := os.WriteFile(filepath.Join(view.Path, "draft\nline.txt"), []byte("draft\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if preview, err := InspectGitViewDelivery(ctx, views, view.ID, baseline); err != nil || preview.Untracked != 1 {
		t.Fatalf("newline-bearing path was miscounted: %+v %v", preview, err)
	}
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err == nil {
		t.Fatal("untracked file was silently omitted from a successful delivery")
	}
	if err := os.Remove(filepath.Join(view.Path, "draft\nline.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("external change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, root, "add", "file.txt")
	baselineGit(t, root, "commit", "-qm", "another run changed the source")
	advanced := baselineGit(t, root, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "new-draft.txt"), []byte("other work in progress\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	delivered, err := DeliverGitViewBranch(ctx, views, view.ID, baseline)
	if err != nil || delivered.DeliveredBranch != "selfmind/run-two" {
		t.Fatalf("second Run's branch could not be delivered after first Run advanced: %+v %v", delivered, err)
	}
	if got := baselineGit(t, root, "rev-parse", "HEAD"); got != advanced {
		t.Fatalf("delivery moved the source HEAD: %s", got)
	}
	if draft, err := os.ReadFile(filepath.Join(root, "new-draft.txt")); err != nil || string(draft) != "other work in progress\n" {
		t.Fatalf("delivery changed source's uncommitted work: %q %v", draft, err)
	}
	if _, err := os.Stat(filepath.Join(view.Path, ".git")); err != nil {
		t.Fatalf("failed delivery removed the view: %v", err)
	}
}

func TestGitViewDeliveryRejectsChangedSourceIdentity(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "run-changed-source", baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), []byte("from view\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, view.Path, "add", "file.txt")
	baselineGit(t, view.Path, "commit", "-qm", "change")
	if err := os.Rename(filepath.Join(root, ".git"), filepath.Join(root, ".git-moved")); err != nil {
		t.Fatal(err)
	}
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err == nil {
		t.Fatal("delivery accepted a source repository with changed Git identity")
	}
	if _, err := os.Stat(filepath.Join(view.Path, ".git")); err != nil {
		t.Fatalf("identity conflict removed undelivered view: %v", err)
	}
}

func TestGitViewDeliveryRefusesUnreviewedViewGitConfig(t *testing.T) {
	ctx := context.Background()
	root := cleanGitFixture(t)
	baseline, err := InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	views := filepath.Join(t.TempDir(), "views")
	view, err := EnsureGitView(ctx, views, "run-config", baseline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), []byte("from view\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineGit(t, view.Path, "add", "file.txt")
	baselineGit(t, view.Path, "commit", "-qm", "change")
	baselineGit(t, view.Path, "config", "--local", "uploadpack.packObjectsHook", "unreviewed-command")
	if _, err := DeliverGitViewBranch(ctx, views, view.ID, baseline); err == nil {
		t.Fatal("daemon imported objects from a view with executable Git configuration")
	}
	if _, err := os.Stat(filepath.Join(view.Path, ".git")); err != nil {
		t.Fatalf("rejected view was destroyed: %v", err)
	}
}
