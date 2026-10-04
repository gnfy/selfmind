package httpapi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
)

func TestExecutionViewCommandsAreExactOwnerScopedAndNonDestructive(t *testing.T) {
	daemon, store, owner, task, _ := newApprovalTestServer(t)
	ctx := context.Background()
	root := gitAdmissionFixture(t)
	baseline, err := executionenv.InspectCleanGitBaseline(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	view, err := executionenv.EnsureGitView(ctx, store.ExecutionViewsDir(), "run-demo", baseline)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRunWithOptions(ctx, task, "cli", "edit in view", control.StartRunOptions{
		ExecutionRoots: []executionenv.RootBinding{{Path: view.Path, Role: executionenv.RootRolePrimary,
			AccessCap: executionenv.RootAccessWrite, Source: executionenv.RootSourceExecutionView,
			ContextRoot: true, GitBaseline: &baseline}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(view.Path, "file.txt"), []byte("delivered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", view.Path}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("add", "file.txt")
	git("commit", "-qm", "from isolated run")
	command := func(identity *control.IdentityContext, input string) (string, error) {
		t.Helper()
		handled, reply, _, err := daemon.tryHandleControlCommand(ctx, identity, api.MessageRequest{Content: input})
		if !handled {
			t.Fatalf("%s escaped control path", input)
		}
		return reply, err
	}
	if _, err := command(owner, "/apply "+run.ID); err == nil {
		t.Fatal("running view was delivered while it could still change")
	}
	if _, err := command(owner, "/views archive "+run.ID); err == nil {
		t.Fatal("running view was retired while it could still change")
	}
	if err := store.FinishRun(ctx, owner.TenantID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	stranger, err := store.ResolveOrCreateAccount(ctx, owner.TenantID, "cli", "stranger", "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := command(stranger, "/views "+run.ID); err == nil {
		t.Fatal("stranger read another person's view")
	}
	if _, err := command(stranger, "/apply "+run.ID); err == nil {
		t.Fatal("stranger delivered another person's view")
	}
	if _, err := command(stranger, "/views archive "+run.ID); err == nil {
		t.Fatal("stranger retired another person's view")
	}
	if list, err := command(owner, "/views"); err != nil || !strings.Contains(list, run.ID) {
		t.Fatalf("owner could not find view: %q %v", list, err)
	}
	if detail, err := command(owner, "/views "+run.ID); err != nil || !strings.Contains(detail, "Committed files: 1") {
		t.Fatalf("owner could not inspect view: %q %v", detail, err)
	}
	if reply, err := command(owner, "/apply "+run.ID); err != nil || !strings.Contains(reply, "selfmind/run-demo") || !strings.Contains(reply, "checkout was not changed") {
		t.Fatalf("safe delivery failed: %q %v", reply, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "file.txt")); err != nil || string(got) != "base\n" {
		t.Fatalf("original checkout changed: %q %v", got, err)
	}
	queued, err := store.EnqueueQueued(ctx, control.QueuedTask{
		TenantID: owner.TenantID, PersonID: owner.PersonID, Platform: "cli", Channel: "other-session",
		Content: "inspect retained work", ExecutionRoots: run.ExecutionRoots,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := command(owner, "/views archive "+run.ID); err == nil {
		t.Fatal("view was retired while durable queued work still referred to it")
	}
	if err := store.MarkQueued(ctx, owner.TenantID, queued.ID, control.QueueStatusCancelled); err != nil {
		t.Fatal(err)
	}
	if reply, err := command(owner, "/views archive "+run.ID); err != nil || !strings.Contains(reply, "All files and Git metadata remain") {
		t.Fatalf("retirement failed: %q %v", reply, err)
	}
	if _, err := os.Lstat(view.Path); !os.IsNotExist(err) {
		t.Fatalf("retired view still occupies run root: %v", err)
	}
	if detail, err := command(owner, "/views "+run.ID); err != nil || !strings.Contains(detail, "Retired: true") {
		t.Fatalf("owner could not inspect retired view: %q %v", detail, err)
	}
	if _, err := command(owner, "/apply "+run.ID); err == nil {
		t.Fatal("retired view was delivered without restoring it")
	}
	if reply, err := command(owner, "/views restore "+run.ID); err != nil || !strings.Contains(reply, "Restored execution view") {
		t.Fatalf("restoration failed: %q %v", reply, err)
	}
	if _, err := command(owner, "/views restore "+run.ID); err != nil {
		t.Fatalf("repeated restoration failed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(view.Path, "file.txt")); err != nil || string(data) != "delivered\n" {
		t.Fatalf("restored view lost work: %q %v", data, err)
	}
	if _, err := command(owner, "/apply "+run.ID); err != nil {
		t.Fatalf("delivered view changed after restoration: %v", err)
	}
	if _, err := command(owner, "/views archive "+run.ID); err != nil {
		t.Fatalf("second retirement failed: %v", err)
	}
	if _, err := command(stranger, "/views prune "+run.ID); err == nil {
		t.Fatal("stranger pruned another person's view")
	}
	if reply, err := command(owner, "/views prune "+run.ID); err != nil || !strings.Contains(reply, "cannot be restored") {
		t.Fatalf("explicit prune failed: %q %v", reply, err)
	}
	if _, err := os.Lstat(filepath.Join(store.ExecutionViewsDir(), ".retired", view.ID)); !os.IsNotExist(err) {
		t.Fatalf("pruned checkout remained on disk: %v", err)
	}
	if detail, err := command(owner, "/views "+run.ID); err != nil || !strings.Contains(detail, "refs/selfmind/retained/run-demo") {
		t.Fatalf("pruned view's retained ref is invisible: %q %v", detail, err)
	}
	if _, err := command(owner, "/views restore "+run.ID); err == nil {
		t.Fatal("pruned checkout was restored")
	}
	if _, err := command(owner, "/views prune "+run.ID); err != nil {
		t.Fatalf("prune replay failed: %v", err)
	}
}
