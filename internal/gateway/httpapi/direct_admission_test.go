package httpapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/router"
	"selfmind/internal/kernel"
)

func TestDirectAdmissionQueuesConflictingRoots(t *testing.T) {
	for _, target := range []string{"same", "child", "symlink", "independent"} {
		t.Run(target, func(t *testing.T) {
			provider := newSlowLLMProvider("done")
			provider.releaseNow()
			daemon, store, _ := newDetachedRunServer(t, provider)
			ctx := context.Background()
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			requested := root
			switch target {
			case "child":
				requested = filepath.Join(root, "child")
				if err := os.Mkdir(requested, 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				requested = filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(root, requested); err != nil {
					t.Fatal(err)
				}
			case "independent":
				requested = t.TempDir()
			}
			workspace, err := store.EnsureWorkspace(ctx, control.Workspace{TenantID: identity.TenantID,
				OwnerPersonID: identity.PersonID, Name: "requested", LocalPath: requested})
			if err != nil {
				t.Fatal(err)
			}
			coord := daemon.coordinator()
			coord.activeLimit = 2
			held := &activeRun{PersonID: identity.PersonID, Platform: "cli", Channel: "session-a", RunID: "held",
				ExecutionRoots: []executionenv.RootBinding{{Path: root, AccessCap: executionenv.RootAccessWrite}}}
			if !coord.beginActive(identity.PersonID, held) {
				t.Fatal("cannot register held root")
			}
			defer coord.endActiveRun(identity.PersonID, held)
			response, status := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
				Channel: "session-b", WorkspaceID: workspace.ID, Content: "start separate work", ForceNew: true})
			if status != 200 {
				t.Fatalf("status=%d response=%+v", status, response)
			}
			if target == "independent" {
				if response.Run == nil || response.Turn.QueueID != "" {
					t.Fatalf("independent work did not start: %+v", response)
				}
				return
			}
			if !response.Accepted || response.Turn == nil || response.Turn.QueueID == "" {
				t.Fatalf("conflicting direct work was not durably queued: %+v", response)
			}
			queued, err := store.GetQueued(ctx, identity.TenantID, response.Turn.QueueID)
			if err != nil || queued == nil || queued.Status != control.QueueStatusQueued || queued.WorkspaceID != workspace.ID || len(queued.ExecutionRoots) != 1 {
				t.Fatalf("queued scope=%+v err=%v", queued, err)
			}
			select {
			case <-provider.started:
				t.Fatal("blocked work entered the model")
			default:
			}
		})
	}
}

func TestDirectAdmissionPreservesQueuedSourceFIFO(t *testing.T) {
	for _, state := range []string{"queued", "claimed", "other source"} {
		t.Run(state, func(t *testing.T) {
			sameSource := state != "other source"
			provider := newSlowLLMProvider("done")
			provider.releaseNow()
			daemon, store, _ := newDetachedRunServer(t, provider)
			ctx := context.Background()
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			workspace, err := store.EnsureWorkspace(ctx, control.Workspace{TenantID: identity.TenantID,
				OwnerPersonID: identity.PersonID, Name: "independent", LocalPath: root})
			if err != nil {
				t.Fatal(err)
			}
			channel := "session-b"
			if !sameSource {
				channel = "session-c"
			}
			headRoot := t.TempDir()
			head, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID,
				Platform: "cli", PlatformUserID: "local", Channel: channel, Content: "earlier work",
				ExecutionRoots: []executionenv.RootBinding{{Path: headRoot, AccessCap: executionenv.RootAccessWrite}}})
			if err != nil {
				t.Fatal(err)
			}
			if state == "claimed" {
				if _, claimed, err := store.ClaimQueued(ctx, identity.TenantID, head.ID, time.Minute); err != nil || !claimed {
					t.Fatalf("claim head: %t %v", claimed, err)
				}
			}
			daemon.coordinator().activeLimit = 2
			held := &activeRun{PersonID: identity.PersonID, Platform: "cli", Channel: "session-a", RunID: "held",
				ExecutionRoots: []executionenv.RootBinding{{Path: headRoot, AccessCap: executionenv.RootAccessWrite}}}
			if !daemon.coordinator().beginActive(identity.PersonID, held) {
				t.Fatal("cannot hold queue head's root")
			}
			defer daemon.coordinator().endActiveRun(identity.PersonID, held)
			response, status := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
				Channel: "session-b", WorkspaceID: workspace.ID, Content: "later work", ForceNew: true})
			if status != 200 {
				t.Fatalf("status=%d response=%+v", status, response)
			}
			if !sameSource {
				if response.Run == nil || response.Turn.QueueID != "" {
					t.Fatalf("another source unnecessarily waited: %+v", response)
				}
				return
			}
			if !response.Accepted || response.Turn == nil || response.Turn.QueueID == "" {
				t.Fatalf("later source input passed queue head: %+v", response)
			}
			rows, err := store.ListQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued)
			if state == "claimed" {
				if err != nil || len(rows) != 1 || rows[0].ID != response.Turn.QueueID {
					t.Fatalf("claimed source lost its FIFO place: %+v %v", rows, err)
				}
				return
			}
			if err != nil || len(rows) != 2 || rows[0].ID != head.ID || rows[1].ID != response.Turn.QueueID {
				t.Fatalf("FIFO queue=%+v err=%v", rows, err)
			}
		})
	}
}

// Blocking context propagation exposes the registry publication boundary
// deterministically, before an async worker can begin execution.
type admissionBarrierContext struct {
	context.Context
	entered, release chan struct{}
	once             sync.Once
}

func (c *admissionBarrierContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return c.Context.Done()
}

func TestAsyncRegistryPublishesInitializedControls(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	barrier := &admissionBarrierContext{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
	daemon.BackgroundRunContext = barrier
	response := make(chan api.MessageResponse, 1)
	go func() {
		resp, _ := daemon.coordinator().startAsyncRun(identity, api.MessageRequest{
			Platform: "cli", PlatformUserID: "local", Channel: "session-a", Content: "work"}, router.IntentResult{})
		response <- resp
	}()
	select {
	case <-barrier.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("context propagation did not reach barrier")
	}
	active := daemon.coordinator().activeRunsForPerson(identity.PersonID)
	close(barrier.release)
	for _, run := range active {
		if run.Cancel == nil || run.Interrupt == nil || run.Steer == nil {
			t.Errorf("published partial async controls: cancel=%t interrupt=%t steer=%t", run.Cancel != nil, run.Interrupt != nil, run.Steer != nil)
		}
	}
	select {
	case <-response:
	case <-time.After(5 * time.Second):
		t.Fatal("async admission did not return")
	}
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool { return daemon.coordinator().activeCount(identity.PersonID) == 0 }, "async run did not finalize")
}

func TestAsyncAcceptedHasDurableRun(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	response, status := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local",
		Channel: "session-a", Content: "work", Async: true})
	if status != 200 || !response.Accepted || response.Turn == nil || response.Turn.RunID == "" {
		t.Errorf("async acceptance has no durable Run: status=%d response=%+v", status, response)
	} else if run, err := store.GetRun(ctx, identity.TenantID, response.Turn.RunID); err != nil || run == nil {
		t.Errorf("accepted Run missing: %+v err=%v", run, err)
	}
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool { return daemon.coordinator().activeCount(identity.PersonID) == 0 }, "async run did not finalize")
}

func TestAsyncPreparationFailureDoesNotAccept(t *testing.T) {
	provider := newSlowLLMProvider("unused")
	daemon, store, sender := newDetachedRunServer(t, provider)
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(context.Background(), identity.TenantID, identity.PersonID, "weixin", "wx-local", "IM"); err != nil {
		t.Fatal(err)
	}
	daemon.coordinator().approvalModeRead = func(context.Context, string, string) (string, int64, error) {
		return "", 0, errors.New("injected mode read failure")
	}
	response, _ := daemon.coordinator().startAsyncRun(identity, api.MessageRequest{Platform: "weixin",
		PlatformUserID: "wx-local", Channel: "wx-local", Content: "work"}, router.IntentResult{})
	if response.Accepted || response.Error == "" {
		t.Errorf("preparation failure was acknowledged as work: %+v", response)
	}
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool { return daemon.coordinator().activeCount(identity.PersonID) == 0 }, "refused async run did not finalize")
	if messages := sender.snapshot(); len(messages) != 0 {
		t.Fatalf("inline preparation refusal was also pushed as an async result: %+v", messages)
	}
}

func TestPhysicalCaseAliasBlocksQueueAndViewMove(t *testing.T) {
	root := filepath.Join(t.TempDir(), "MixedCaseView")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(root), "mixedcaseview")
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(rootInfo, aliasInfo) {
		t.Skip("filesystem does not expose case aliases")
	}
	daemon, _, identity, _, _ := newApprovalTestServer(t)
	held := &activeRun{PersonID: identity.PersonID, ExecutionRoots: []executionenv.RootBinding{{Path: alias, AccessCap: executionenv.RootAccessWrite}}}
	if !daemon.coordinator().beginActive(identity.PersonID, held) {
		t.Fatal("cannot register case alias")
	}
	defer daemon.coordinator().endActiveRun(identity.PersonID, held)
	if queuedResourcesReady(control.QueuedTask{ExecutionRoots: []executionenv.RootBinding{{Path: root, AccessCap: executionenv.RootAccessWrite}}}, []*activeRun{held}) {
		t.Error("case alias bypassed queue resource check")
	}
	if !daemon.coordinator().executionViewAdmitted(root) {
		t.Error("case alias bypassed execution-view retention guard")
	}
}

func TestBlockedCLISessionRemainsUnclaimedAcrossDrains(t *testing.T) {
	provider := newSlowLLMProvider("unused")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	coord := daemon.coordinator()
	coord.activeLimit = 2
	held := &activeRun{PersonID: identity.PersonID, Platform: "cli", Channel: "session-a", RunID: "held",
		ExecutionRoots: []executionenv.RootBinding{{Path: t.TempDir(), AccessCap: executionenv.RootAccessWrite}}}
	if !coord.beginActive(identity.PersonID, held) {
		t.Fatal("cannot register foreground lane")
	}
	defer coord.endActiveRun(identity.PersonID, held)
	queued, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID,
		Platform: "cli", PlatformUserID: "local", Channel: "session-a", Content: "later input",
		ExecutionRoots: []executionenv.RootBinding{{Path: t.TempDir(), AccessCap: executionenv.RootAccessWrite}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		coord.drainQueue(identity)
	}
	got, err := store.GetQueued(ctx, identity.TenantID, queued.ID)
	if err != nil || got == nil || got.Status != control.QueueStatusQueued || got.AttemptGeneration != 0 || got.ClaimToken != "" {
		t.Fatalf("blocked foreground input was repeatedly claimed: %+v %v", got, err)
	}
	select {
	case <-provider.started:
		t.Fatal("blocked foreground input entered model")
	default:
	}
}

func TestDeclaredNewExternalWorkUsesFreeSlot(t *testing.T) {
	for _, source := range []string{"im", "cron", "http"} {
		t.Run(source, func(t *testing.T) {
			provider := newSlowLLMProvider("done")
			daemon, store, _ := newDetachedRunServer(t, provider)
			ctx := context.Background()
			identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.BindAccount(ctx, identity.TenantID, identity.PersonID, "weixin", "wx-local", "IM"); err != nil {
				t.Fatal(err)
			}
			workspace, err := store.EnsureWorkspace(ctx, control.Workspace{TenantID: identity.TenantID,
				OwnerPersonID: identity.PersonID, Name: "separate", LocalPath: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			coord := daemon.coordinator()
			coord.activeLimit = 2
			held := &activeRun{PersonID: identity.PersonID, Platform: "cli", Channel: "session-a", RunID: "held",
				ExecutionRoots: []executionenv.RootBinding{{Path: t.TempDir(), AccessCap: executionenv.RootAccessWrite}}}
			if !coord.beginActive(identity.PersonID, held) {
				t.Fatal("cannot hold first slot")
			}
			defer coord.endActiveRun(identity.PersonID, held)
			req := api.MessageRequest{Platform: "weixin", PlatformUserID: "wx-local", Channel: "chat",
				WorkspaceID: workspace.ID, Content: "independent work", ForceNew: true, Async: true}
			if source == "cron" {
				req.ForceNew = false
				req.Origin = runOriginCron
				req.OriginID = "schedule-test"
			}
			if source == "http" {
				req.Platform = "http"
				req.PlatformUserID = "web-local"
			}
			response, status, _ := daemon.dispatchAdmitted(ctx, identity, req)
			if status != 200 || !response.Accepted || response.Turn == nil || response.Turn.RunID == "" || response.Turn.QueueID != "" {
				t.Errorf("declared independent %s queued despite a free slot: %d %+v", source, status, response)
			}
			provider.releaseNow()
			waitUntil(t, 5*time.Second, func() bool { return coord.activeCount(identity.PersonID) == 1 }, "independent source did not finalize")
		})
	}
}

func TestUnboundActiveRunDoesNotAcceptStartupSteering(t *testing.T) {
	daemon, _, identity, _, _ := newApprovalTestServer(t)
	active := &activeRun{PersonID: identity.PersonID, Platform: "cli", Channel: "session-a",
		Steer: make(chan kernel.SteeringInput, 1)}
	response, accepted := daemon.steerActiveRun(context.Background(), identity, active, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "session-a", Content: "guidance before a Run exists"})
	if accepted || response.Accepted || len(active.Steer) != 0 {
		t.Fatalf("unbound guidance was accepted for startup replay: %+v accepted=%t buffered=%d", response, accepted, len(active.Steer))
	}
}

func TestCoordinationRunDoesNotOccupyQueueWorkSlot(t *testing.T) {
	provider := newSlowLLMProvider("done")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	coordination, err := store.StartRunForOwner(ctx, control.RunOwner{TenantID: identity.TenantID, PersonID: identity.PersonID}, "chat", "choose a target", control.StartRunOptions{ExecutionClass: "coordination"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.FinishRun(ctx, identity.TenantID, coordination.ID, "done")
	queued, err := store.EnqueueQueued(ctx, control.QueuedTask{TenantID: identity.TenantID, PersonID: identity.PersonID,
		Platform: "cli", PlatformUserID: "local", Channel: "session-b", Content: "ordinary work",
		ExecutionRoots: []executionenv.RootBinding{{Path: t.TempDir(), AccessCap: executionenv.RootAccessWrite}}})
	if err != nil {
		t.Fatal(err)
	}
	daemon.coordinator().drainQueue(identity)
	started, err := store.GetQueued(ctx, identity.TenantID, queued.ID)
	if err != nil || started == nil || started.RunID == "" {
		t.Errorf("coordination Run occupied the sole work slot: %+v %v", started, err)
	}
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool { return daemon.coordinator().activeCount(identity.PersonID) == 0 }, "queue work did not finalize")
}

func TestGitViewExceptionRequiresBoundDirectWriter(t *testing.T) {
	root := t.TempDir()
	bindings := []executionenv.RootBinding{{Path: root, Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite,
		Source: executionenv.RootSourceWorkspace, GitBaseline: &executionenv.GitBaseline{Root: root, CommonDir: filepath.Join(root, ".git"), Commit: "frozen"}}}
	queued := control.QueuedTask{ExecutionRoots: bindings}
	writer := &activeRun{ExecutionRoots: bindings}
	if queuedResourcesReady(queued, []*activeRun{writer}) {
		t.Fatal("unbound writer granted a view exception before materialization can select it")
	}
	writer.RunID = "bound-direct-writer"
	if !queuedResourcesReady(queued, []*activeRun{writer}) {
		t.Fatal("bound clean writer lost its independent-view admission")
	}
}

func TestGitViewMaterializationFailureQueuesDirectWork(t *testing.T) {
	provider := newSlowLLMProvider("unused")
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	root := canonicalStoredDirectory(t.TempDir())
	workspace, err := store.EnsureWorkspace(ctx, control.Workspace{TenantID: identity.TenantID, OwnerPersonID: identity.PersonID, Name: "unavailable repo", LocalPath: root})
	if err != nil {
		t.Fatal(err)
	}
	bindings := []executionenv.RootBinding{{Path: root, Role: executionenv.RootRolePrimary, AccessCap: executionenv.RootAccessWrite,
		Source: executionenv.RootSourceWorkspace, GitBaseline: &executionenv.GitBaseline{Root: root, CommonDir: filepath.Join(root, ".git"), Commit: "missing"}}}
	coord := daemon.coordinator()
	coord.activeLimit = 2
	held := &activeRun{PersonID: identity.PersonID, Platform: "cli", Channel: "session-a", RunID: "bound-writer", ExecutionRoots: bindings}
	if !coord.beginActive(identity.PersonID, held) {
		t.Fatal("cannot register frozen direct writer")
	}
	defer coord.endActiveRun(identity.PersonID, held)
	response, status := daemon.ProcessMessage(ctx, api.MessageRequest{Platform: "cli", PlatformUserID: "local", Channel: "session-b",
		WorkspaceID: workspace.ID, Content: "independent work", ForceNew: true, Async: true})
	if status != 200 || !response.Accepted || response.Turn == nil || response.Turn.QueueID == "" {
		t.Errorf("failed view fell back to occupying the original root: %d %+v", status, response)
	}
	provider.releaseNow()
	waitUntil(t, 5*time.Second, func() bool { return coord.activeCount(identity.PersonID) == 1 }, "failed view startup did not release registry slot")
	for i := 0; i < 3; i++ {
		coord.drainQueue(identity)
		waitUntil(t, 5*time.Second, func() bool { return coord.activeCount(identity.PersonID) == 1 }, "failed queued view kept an admission slot")
	}
	counts, err := store.CountChannelMessagesByChannel(ctx, identity.TenantID, identity.PersonID)
	if err != nil || len(counts) != 0 {
		t.Fatalf("blocked view retries duplicated channel input: %+v %v", counts, err)
	}
}
