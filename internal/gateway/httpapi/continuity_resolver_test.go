package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

type continuityResolverStub struct {
	calls    int
	request  ContinuityResolveRequest
	decision func(ContinuityResolveRequest) ContinuityDecision
	err      error
}

func (s *continuityResolverStub) ResolveContinuity(_ context.Context, req ContinuityResolveRequest) (ContinuityResolution, error) {
	s.calls++
	s.request = req
	resolution := ContinuityResolution{Provider: "stub", Model: "fast"}
	if s.decision != nil {
		resolution.Decision = s.decision(req)
	}
	return resolution, s.err
}

func seedContinuityHistory(t *testing.T, store *control.Store, identity *control.IdentityContext, status string) (*control.Task, *control.Run) {
	t.Helper()
	ctx := context.Background()
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID,
		Title: "RUQX-767 production release", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "release PHP services for RUQX-767")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishRun(ctx, identity.TenantID, run.ID, status); err != nil {
		t.Fatal(err)
	}
	return task, run
}

func TestNaturalProgressResolvesAcrossBoundEndpointsWithoutStartingRun(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := httptest.NewRequest(http.MethodPost, "/", nil).Context()
	cliIdentity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindAccount(ctx, cliIdentity.TenantID, cliIdentity.PersonID, "weixin", "wx-user", "WX"); err != nil {
		t.Fatal(err)
	}
	_, run := seedContinuityHistory(t, store, cliIdentity, "completed")
	resolver := &continuityResolverStub{decision: func(req ContinuityResolveRequest) ContinuityDecision {
		if len(req.Candidates) == 0 {
			t.Fatal("resolver received no candidates")
		}
		return ContinuityDecision{
			Action: ContinuityObserve, Certainty: ContinuityClear,
			TargetTaskID: req.Candidates[0].TaskID, TargetRunID: req.Candidates[0].RunID,
			ObserveKind: "progress", Evidence: []string{"matching release key"},
		}
	}}
	daemon := &Server{Control: store, DefaultTenantID: "default", ContinuityResolver: resolver, ContinuityMode: "safe"}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "weixin", PlatformUserID: "wx-user", Channel: "weixin",
		Content: "刚才 RUQX-767 发布任务进展怎么样？",
	})
	if status != http.StatusOK || resp.Run == nil || resp.Run.ID != run.ID {
		t.Fatalf("status=%d response=%+v", status, resp)
	}
	if resolver.calls != 1 || resolver.request.PersonID != cliIdentity.PersonID || resolver.request.Channel != "weixin" {
		t.Fatalf("resolver calls=%d request=%+v", resolver.calls, resolver.request)
	}
	runs, err := store.ListTaskRuns(ctx, cliIdentity.TenantID, run.TaskID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("observe must not create a run: count=%d err=%v", len(runs), err)
	}
}

func TestSafeModeHistoricalResumeRequiresDurableChoice(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	_, run := seedContinuityHistory(t, store, identity, "interrupted")
	resolver := &continuityResolverStub{decision: func(req ContinuityResolveRequest) ContinuityDecision {
		return ContinuityDecision{Action: ContinuityResume, Certainty: ContinuityClear,
			TargetTaskID: run.TaskID, TargetRunID: run.ID, Reason: "The release reference matches."}
	}}
	daemon := &Server{Control: store, DefaultTenantID: "default", ContinuityResolver: resolver, ContinuityMode: "safe"}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "继续处理 RUQX-767 的发布",
	})
	if status != http.StatusOK || resp.Choice == nil || resp.Turn == nil || resp.Turn.Status != "waiting_user" {
		t.Fatalf("safe resume must clarify: status=%d response=%+v", status, resp)
	}
	runs, _ := store.ListTaskRuns(ctx, identity.TenantID, run.TaskID, 10)
	if len(runs) != 1 {
		t.Fatalf("safe clarification started a child run: %d", len(runs))
	}
}

func TestContinuityResolverFailureFailsClosedWithoutStartingRun(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	_, run := seedContinuityHistory(t, store, identity, "interrupted")
	resolver := &continuityResolverStub{err: errors.New("provider timeout")}
	daemon := &Server{Control: store, DefaultTenantID: "default", ContinuityResolver: resolver, ContinuityMode: "safe"}
	resp, _ := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "这个发布任务怎么样了",
	})
	if resp.Choice == nil || !strings.Contains(resp.Content, "could not safely match") {
		t.Fatalf("resolver failure did not fail closed: %+v", resp)
	}
	runs, _ := store.ListTaskRuns(ctx, identity.TenantID, run.TaskID, 10)
	if len(runs) != 1 {
		t.Fatalf("resolver failure started a run: %d", len(runs))
	}
}

func TestClaimedObserveChoiceIsReadOnly(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	_, run := seedContinuityHistory(t, store, identity, "completed")
	daemon := &Server{Control: store, DefaultTenantID: "default"}
	choice, err := daemon.createTurnChoice(ctx, identity, api.MessageRequest{Channel: "cli", Content: "发布任务进展怎么样", ContinuityResolutionID: "turnres_source"}, []control.TurnChoiceOption{
		{Key: "1", Label: "release", Action: "observe", TaskID: run.TaskID, RunID: run.ID},
		{Key: "2", Label: "new", Action: "new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "1", ChoiceID: choice.ID,
	})
	if status != http.StatusOK || resp.Run == nil || resp.Run.ID != run.ID {
		t.Fatalf("status=%d response=%+v", status, resp)
	}
	runs, _ := store.ListTaskRuns(ctx, identity.TenantID, run.TaskID, 10)
	if len(runs) != 1 {
		t.Fatalf("observe choice started a run: %d", len(runs))
	}
	corrections, err := store.CountTurnResolutionCorrections(ctx, identity.TenantID, identity.PersonID, "turnres_source")
	if err != nil {
		t.Fatal(err)
	}
	if corrections != 1 {
		t.Fatalf("corrections=%d", corrections)
	}
}

func TestStandaloneContinueSkipsModelContinuityResolver(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	seedContinuityHistory(t, store, identity, "interrupted")
	resolver := &continuityResolverStub{}
	daemon := &Server{Control: store, DefaultTenantID: "default", ContinuityResolver: resolver, ContinuityMode: "safe"}
	result := daemon.resolveNaturalContinuity(ctx, identity, api.MessageRequest{Platform: "cli", Channel: "cli", Content: "继续"})
	if result.Resolved || result.Response != nil || resolver.calls != 0 {
		t.Fatalf("standalone continue must stay deterministic: result=%+v calls=%d", result, resolver.calls)
	}
}

func TestObservePlusNewCarriesBoundedStatusIntoFreshTurn(t *testing.T) {
	store, err := control.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity, _ := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Local")
	_, run := seedContinuityHistory(t, store, identity, "completed")
	resolver := &continuityResolverStub{decision: func(req ContinuityResolveRequest) ContinuityDecision {
		return ContinuityDecision{
			Action: ContinuityObserve, SecondaryAction: ContinuityNew, Certainty: ContinuityClear,
			TargetTaskID: run.TaskID, TargetRunID: run.ID, ObserveKind: "result",
		}
	}}
	daemon := &Server{Control: store, DefaultTenantID: "default", ContinuityResolver: resolver, ContinuityMode: "safe"}
	result := daemon.resolveNaturalContinuity(ctx, identity, api.MessageRequest{
		Platform: "cli", Channel: "cli", Content: "发布结果怎么样？另外新建一份发布复盘。",
	})
	if !result.Resolved || !result.Request.ForceNew || result.Response != nil ||
		!strings.Contains(result.Request.ContinuityContext, "RUQX-767") {
		t.Fatalf("result=%+v", result)
	}
}
