package httpapi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
	"selfmind/internal/kernel/memory"
)

func gitAdmissionFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "file.txt")
	git("commit", "-qm", "base")
	return root
}

func TestTwoCLIRunsInOneCleanRepoUseIndependentPhysicalViews(t *testing.T) {
	provider := &countedSlowProvider{slowLLMProvider: newSlowLLMProvider("done"), startedCalls: make(chan struct{}, 4)}
	defer provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider.slowLLMProvider)
	firstAgent := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test", 1, 1, nil)
	secondAgent := kernel.NewAgent(memory.NewMemoryManager(nil), stubToolBackend{}, provider, "test", 1, 1, nil)
	daemon.Gateway = router.NewGateway(firstAgent, nil)
	daemon.Gateway.EnableWorkerPool([]*kernel.Agent{secondAgent})
	daemon.coordinator().activeLimit = 2
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	root := gitAdmissionFixture(t)
	workspace, err := store.EnsureWorkspace(ctx, control.Workspace{TenantID: identity.TenantID, OwnerPersonID: identity.PersonID, Name: "repo", LocalPath: root})
	if err != nil {
		t.Fatal(err)
	}
	request := func(channel string) {
		t.Helper()
		response, status := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: channel,
			WorkspaceID: workspace.ID, Content: "inspect this repository", Async: true})
		if status != 200 || !response.Accepted {
			t.Fatalf("%s was not accepted: status=%d response=%+v", channel, status, response)
		}
		select {
		case <-provider.startedCalls:
		case <-time.After(8 * time.Second):
			t.Fatalf("%s did not enter the model while the other run was active", channel)
		}
	}
	request("session-a")
	request("session-b")
	active := daemon.coordinator().activeRunsForPerson(identity.PersonID)
	if len(active) != 2 {
		t.Fatalf("active runs=%+v", active)
	}
	var direct, isolated string
	for _, run := range active {
		if len(run.ExecutionRoots) != 1 {
			t.Fatalf("run has unexpected roots: %+v", run)
		}
		root := run.ExecutionRoots[0]
		switch root.Source {
		case executionenv.RootSourceWorkspace:
			direct = root.Path
		case executionenv.RootSourceExecutionView:
			isolated = root.Path
		default:
			t.Fatalf("unexpected run source: %+v", root)
		}
		stored, err := store.GetRun(ctx, identity.TenantID, run.RunID)
		if err != nil || !executionenv.EqualRootBindings(stored.ExecutionRoots, run.ExecutionRoots) {
			t.Fatalf("in-memory scope disagrees with durable Run: %+v, %v", stored, err)
		}
	}
	if direct == "" || isolated == "" || direct == isolated {
		t.Fatalf("runs did not split views: direct=%q isolated=%q", direct, isolated)
	}
	if _, err := os.Stat(filepath.Join(isolated, "file.txt")); err != nil {
		t.Fatalf("isolated worktree has no project contents: %v", err)
	}
	provider.releaseNow()
	waitUntil(t, 8*time.Second, func() bool { return daemon.coordinator().activeCount(identity.PersonID) == 0 }, "same-repository runs did not finish")
}

func TestIsolatedGitViewAdmissionUsesFrozenBaseAndExactRunRoot(t *testing.T) {
	ctx := context.Background()
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	root := gitAdmissionFixture(t)
	workspace := &control.Workspace{ID: "ws", OwnerPersonID: identity.PersonID, LocalPath: root}
	task, err := store.CreateTask(ctx, control.TaskCreate{TenantID: identity.TenantID, PersonID: identity.PersonID, WorkspaceID: workspace.ID, Title: "second", Channel: "session-b"})
	if err != nil {
		t.Fatal(err)
	}
	c := &RunCoordinator{srv: &Server{Control: store}, activeLimit: 2, active: make(map[string]map[*activeRun]struct{})}
	first := api.MessageRequest{}
	if err := c.prepareRequestExecutionRoots(ctx, workspace, &first); err != nil {
		t.Fatal(err)
	}
	if first.ExecutionRoots[0].GitBaseline == nil {
		t.Fatal("first direct run did not freeze a baseline")
	}
	firstActive := &activeRun{PersonID: identity.PersonID, RunID: "run-first", ExecutionRoots: first.ExecutionRoots}
	c.active[identity.PersonID] = map[*activeRun]struct{}{firstActive: {}}
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("first run changed direct checkout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := api.MessageRequest{Channel: "session-b", Content: "independent work"}
	if err := c.prepareRequestExecutionRoots(ctx, workspace, &second); err != nil {
		t.Fatal(err)
	}
	if second.ExecutionRoots[0].GitBaseline == nil {
		t.Fatal("second request did not inherit the first run's baseline")
	}
	c.maybeAssignIsolatedGitView(ctx, identity, task, &second, false)
	if len(second.ExecutionRoots) != 1 || second.ExecutionRoots[0].Source != executionenv.RootSourceExecutionView || second.ExecutionRoots[0].Path == canonicalStoredDirectory(root) {
		t.Fatalf("second request did not get its own view: %+v", second.ExecutionRoots)
	}
	if !strings.Contains(c.withGatewayContext("work", identity, task, workspace, second.ExecutionRoots, nil), "workspace_root: "+second.ExecutionRoots[0].Path) {
		t.Fatal("agent prompt still pointed at the original checkout")
	}
	contents, err := os.ReadFile(filepath.Join(second.ExecutionRoots[0].Path, "file.txt"))
	if err != nil || string(contents) != "base\n" {
		t.Fatalf("isolated view did not use frozen commit: %q, %v", contents, err)
	}
	if err := c.prepareRequestExecutionRoots(ctx, workspace, &second); err != nil {
		t.Fatalf("recovery lost its bound view: %v", err)
	}
	if second.ExecutionRoots[0].Source != executionenv.RootSourceExecutionView {
		t.Fatalf("recovery reintroduced the direct root: %+v", second.ExecutionRoots)
	}
	if !queuedResourcesReady(control.QueuedTask{ExecutionRoots: first.ExecutionRoots}, c.activeRunsForPerson(identity.PersonID)) {
		t.Fatal("clean, equal baseline could not enter independent view admission")
	}
}
