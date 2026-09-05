package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
)

func TestGatewayShutdownInterruptsAndRequeuesInsteadOfCancelling(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "gnfy", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "finalize release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "finalize release")
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.EnqueueQueued(ctx, control.QueuedTask{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		Platform: "cli", PlatformUserID: "gnfy", Channel: "cli", Content: "finalize release",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkQueued(ctx, identity.TenantID, queued.ID, control.QueueStatusStarted); err != nil {
		t.Fatal(err)
	}

	// The run was mid-work when the gateway stopped: a durable plan is the
	// evidence that keeps its interruption visible as resumable Attention.
	if _, err := store.SyncRunPlan(ctx, identity.TenantID, run.ID, "finalize release", []control.RunPlanStepInput{{Step: "stage the release", Status: "completed"}, {Step: "finalize the release", Status: "in_progress"}}); err != nil {
		t.Fatal(err)
	}
	runCtx, interrupt := context.WithCancelCause(context.Background())
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{
		TenantID: identity.TenantID, PersonID: identity.PersonID, TaskID: task.ID,
		RunID: run.ID, QueueID: queued.ID, Channel: "cli", StartedAt: time.Now(),
		Cancel: func() { interrupt(context.Canceled) }, Interrupt: interrupt,
	}) {
		t.Fatal("active run registration failed")
	}

	daemon.coordinator().stopAllActive("gateway shutdown")
	if !errors.Is(context.Cause(runCtx), errGatewayShutdown) {
		t.Fatalf("cancel cause = %v; want gateway shutdown", context.Cause(runCtx))
	}
	gotQueue, err := store.GetQueued(ctx, identity.TenantID, queued.ID)
	if err != nil || gotQueue == nil || gotQueue.Status != control.QueueStatusQueued {
		t.Fatalf("queue = %+v, %v; want queued", gotQueue, err)
	}
	gotRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || gotRun == nil || gotRun.Status != "interrupted" {
		t.Fatalf("run = %+v, %v; want interrupted", gotRun, err)
	}
	gotTask, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil || gotTask == nil || gotTask.Status != "interrupted" || !strings.Contains(strings.ToLower(gotTask.CurrentSummary), "gateway shutdown") {
		t.Fatalf("task = %+v, %v; want resumable gateway interruption", gotTask, err)
	}
	requested, err := store.RunCancelRequested(ctx, identity.TenantID, run.ID)
	if err != nil || requested {
		t.Fatalf("cancel_requested = %v, %v; infrastructure restart is not user cancellation", requested, err)
	}
}

func TestModelChangeDrainLeavesNewWorkQueuedForNextDaemon(t *testing.T) {
	ctx := context.Background()
	store := controltest.NewStore(t)
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	daemon.beginDraining("model_change:model_123")
	response := daemon.enqueueDuringModelChange(ctx, identity, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "use the new route",
	})
	if !response.Accepted || response.Turn == nil || response.Turn.Status != "queued" {
		t.Fatalf("response = %+v", response)
	}
	daemon.coordinator().drainQueue(identity)
	if count, err := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); err != nil || count != 1 {
		t.Fatalf("queued count = %d, err=%v; old daemon must not launch it", count, err)
	}
}

func TestIncompleteModelReadinessQueuesNewWorkInsteadOfStartingIt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := control.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	configPath := root + "/config.yaml"
	if _, err := config.LoadConfig(config.Options{Path: configPath, CreateIfMissing: true}); err != nil {
		t.Fatal(err)
	}
	daemon := &Server{
		Control: store, DefaultTenantID: "default",
		ModelChanges: &modelchange.Service{ConfigPath: configPath},
	}

	response, statusCode := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "inspect this workspace",
	})
	if statusCode != http.StatusOK || !response.Accepted || response.Turn == nil || response.Turn.Status != "queued" {
		t.Fatalf("response = status:%d body:%+v", statusCode, response)
	}
	if !strings.Contains(response.Content, "selfmind model") {
		t.Fatalf("content = %q, want Model Manager recovery hint", response.Content)
	}
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "")
	if err != nil {
		t.Fatal(err)
	}
	if count, err := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); err != nil || count != 1 {
		t.Fatalf("queued count = %d, err=%v", count, err)
	}
	daemon.coordinator().drainQueue(identity)
	if count, err := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); err != nil || count != 1 {
		t.Fatalf("queued count after drain = %d, err=%v; readiness boundary must park it", count, err)
	}
}

func TestIncompleteModelReadinessStillSteersActiveContinuation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := control.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	configPath := root + "/config.yaml"
	if _, err := config.LoadConfig(config.Options{Path: configPath, CreateIfMissing: true}); err != nil {
		t.Fatal(err)
	}
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "")
	if err != nil {
		t.Fatal(err)
	}
	steer := make(chan kernel.SteeringInput, 1)
	daemon := &Server{
		Control: store, DefaultTenantID: "default",
		ModelChanges: &modelchange.Service{ConfigPath: configPath},
	}
	if !daemon.coordinator().beginActive(identity.PersonID, &activeRun{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		RunID: "run_active", TaskID: "task_active", Steer: steer, StartedAt: time.Now(),
	}) {
		t.Fatal("active run registration failed")
	}
	defer daemon.coordinator().endActive(identity.PersonID)

	response, statusCode := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "继续",
	})
	if statusCode != http.StatusOK || !response.Accepted || response.Turn == nil || response.Turn.Status != "accepted" {
		t.Fatalf("response = status:%d body:%+v", statusCode, response)
	}
	select {
	case input := <-steer:
		if input.Content != "继续" {
			t.Fatalf("steer content = %q", input.Content)
		}
	default:
		t.Fatal("continuation was queued instead of steering the active run")
	}
	if count, err := store.CountQueued(ctx, identity.TenantID, identity.PersonID, control.QueueStatusQueued); err != nil || count != 0 {
		t.Fatalf("queued count = %d, err=%v", count, err)
	}
}

func TestModelChangeShutdownUsesUnboundedSafeBoundaryWait(t *testing.T) {
	if got := shutdownTimeoutForReason(30*time.Second, "model_change:model_123"); got != 0 {
		t.Fatalf("timeout = %v", got)
	}
	if got := shutdownTimeoutForReason(30*time.Second, "api shutdown"); got != 30*time.Second {
		t.Fatalf("ordinary timeout = %v", got)
	}
}

func TestRecoveryRetryCanCrossTheShutdownSafeBoundary(t *testing.T) {
	service, _ := testModelChangeService(t)
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := status.Running
	candidate.Primary.Model = "gpt-retry"
	prepared, err := service.Prepare(context.Background(), modelchange.PrepareRequest{
		Candidate: candidate, Source: "test", RequireConfirmation: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.BeginDraining(prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.MarkRestarting(prepared.Change.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.FailStarting(errors.New("listener unavailable")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RetryRecovery(prepared.Change.ID); err != nil {
		t.Fatal(err)
	}

	shutdown := make(chan struct{}, 1)
	server := &Server{ModelChanges: service, ShutdownFunc: func() { shutdown <- struct{}{} }}
	if !server.RequestGatewayShutdown(0, "model_change:"+prepared.Change.ID) {
		t.Fatal("retry shutdown request was not accepted")
	}
	select {
	case <-shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("retry never crossed the safe shutdown boundary")
	}
}

func TestModelChangeShutdownNeverInterruptsActiveRun(t *testing.T) {
	service, path := testModelChangeService(t)
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := status.Running
	candidate.Primary.Model = "gpt-after-active-run"
	prepared, err := service.Prepare(context.Background(), modelchange.PrepareRequest{
		Candidate: candidate, Source: "test", RequireConfirmation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(context.Background(), prepared.Change.ID); err != nil {
		t.Fatal(err)
	}

	var cancelled atomic.Bool
	shutdown := make(chan struct{}, 1)
	server := &Server{ModelChanges: service, ShutdownFunc: func() { shutdown <- struct{}{} }}
	if !server.coordinator().beginActive("person_active", &activeRun{
		PersonID: "person_active", RunID: "run_active", StartedAt: time.Now(),
		Cancel: func() { cancelled.Store(true) },
	}) {
		t.Fatal("active run registration failed")
	}
	if !server.RequestGatewayShutdown(time.Millisecond, "model_change:"+prepared.Change.ID) {
		t.Fatal("shutdown request was not accepted")
	}
	time.Sleep(50 * time.Millisecond)
	if cancelled.Load() {
		t.Fatal("model change interrupted the active run")
	}
	select {
	case <-shutdown:
		t.Fatal("gateway stopped before the active run reached a safe boundary")
	default:
	}
	before, err := config.LoadConfig(config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if before.EffectivePrimary().Model == "gpt-after-active-run" {
		t.Fatal("candidate was committed while an active run was still executing")
	}

	server.coordinator().endActive("person_active")
	select {
	case <-shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not stop after the active run completed")
	}
	after, err := config.LoadConfig(config.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if after.EffectivePrimary().Model != "gpt-after-active-run" {
		t.Fatalf("configured model = %q; want committed candidate", after.EffectivePrimary().Model)
	}
}

func TestServiceReconcileTimeoutDefersWithoutInterruptingActiveRun(t *testing.T) {
	var cancelled atomic.Bool
	var shutdown atomic.Bool
	server := &Server{ShutdownFunc: func() { shutdown.Store(true) }}
	if !server.coordinator().beginActive("person_active", &activeRun{
		PersonID: "person_active", RunID: "run_active", StartedAt: time.Now(),
		Cancel: func() { cancelled.Store(true) },
	}) {
		t.Fatal("active run registration failed")
	}
	if !server.RequestGatewayShutdown(time.Millisecond, api.ShutdownReasonServiceReconcile) {
		t.Fatal("service reconciliation shutdown request was not accepted")
	}
	time.Sleep(50 * time.Millisecond)
	if cancelled.Load() {
		t.Fatal("service reconciliation interrupted active work")
	}
	if shutdown.Load() {
		t.Fatal("service reconciliation shut down before active work completed")
	}
	if server.IsDraining() {
		t.Fatal("timed-out service reconciliation left the Gateway draining")
	}
}

func TestModelChangeShutdownCommitsCandidateOnlyAtDrainBoundary(t *testing.T) {
	service, path := testModelChangeService(t)
	status, err := service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate := status.Running
	candidate.Primary.Model = "gpt-after-drain"
	prepared, err := service.Prepare(context.Background(), modelchange.PrepareRequest{Candidate: candidate, Source: "test", RequireConfirmation: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(context.Background(), prepared.Change.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := config.LoadConfig(config.Options{Path: path})
	if before.EffectivePrimary().Model == "gpt-after-drain" {
		t.Fatal("candidate reached config before shutdown drain")
	}
	shutdown := make(chan struct{}, 1)
	server := &Server{ModelChanges: service, ShutdownFunc: func() { shutdown <- struct{}{} }}
	body, _ := json.Marshal(map[string]string{"reason": "model_change:" + prepared.Change.ID})
	req := httptest.NewRequest(http.MethodPost, "/v1/gateway/shutdown", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	server.handleGatewayShutdown(recorder, req)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	select {
	case <-shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("model change never reached the safe shutdown boundary")
	}
	after, _ := config.LoadConfig(config.Options{Path: path})
	status, err = service.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if after.EffectivePrimary().Model != "gpt-after-drain" || status.Pending == nil || status.Pending.Status != modelchange.StatusDraining {
		t.Fatalf("config=%+v status=%+v", after.EffectivePrimary(), status)
	}
}
