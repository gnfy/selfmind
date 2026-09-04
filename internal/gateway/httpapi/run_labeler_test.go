package httpapi

// Post-run maintenance after simplification P2: task routing is gone — every
// root run owns its task and child runs inherit through the parent edge — so
// maintenance is memory extraction plus reference search hints only. These
// tests pin the surviving contracts: eligibility, provider-failure parking,
// frozen legacy-proposal compatibility, non-blocking application, and the
// per-message task-ownership rule that replaced pre-label stickiness.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// fakeLabeler is a scripted PostRunAnalyzer that records prompts and can block
// until released (to prove the response is not gated on maintenance).
type fakeLabeler struct {
	mu      sync.Mutex
	reply   string
	err     error
	calls   int
	prompts []string
	block   chan struct{} // when non-nil, Analyze waits for it (or ctx)
}

func (f *fakeLabeler) Analyze(ctx context.Context, req PostRunAnalysisRequest) (PostRunAnalysis, error) {
	f.mu.Lock()
	f.calls++
	f.prompts = append(f.prompts, req.Prompt)
	block := f.block
	reply, err := f.reply, f.err
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return PostRunAnalysis{}, ctx.Err()
		}
	}
	return PostRunAnalysis{TaskDecision: reply}, err
}

func (f *fakeLabeler) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// durableTurnInput is long enough to pass the language-neutral memory
// eligibility gate (user text + summary >= 160 runes) without relying on
// outcome structure.
func durableTurnInput(topic string) string {
	return strings.Repeat("Work carefully on "+topic+" and keep the durable design constraints in mind. ", 3)
}

// runOrdinaryTurn sends one ordinary sync message and advances the daemon-only
// maintenance worker past its debounce window.
func runOrdinaryTurn(t *testing.T, daemon *Server, content string) api.MessageResponse {
	t.Helper()
	resp, status := daemon.ProcessMessage(context.Background(), api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: content,
	})
	if status != 200 || resp.Task == nil {
		t.Fatalf("turn failed: status=%d resp=%+v", status, resp)
	}
	drainPostRunMaintenance(daemon)
	return resp
}

func drainPostRunMaintenance(daemon *Server) {
	daemon.PostRunMaintenance = PostRunMaintenanceOptions{Debounce: -1, MaxWait: -1, BatchMaxRuns: 10}
	daemon.runMaintenancePassAt(context.Background(), time.Now())
}

func TestPostRunMemoryEligibleIncludesShortNaturalPreference(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		outcome api.RunOutcome
		want    bool
	}{
		{
			name: "compact Chinese preference", input: "以后回答先给结论",
			outcome: api.RunOutcome{Status: "done", Summary: "明白，之后会先给出结论。"}, want: true,
		},
		{
			name: "English preference", input: "Always lead with the conclusion",
			outcome: api.RunOutcome{Status: "done", Summary: "I will follow that preference."}, want: true,
		},
		{
			name: "tiny greeting", input: "hi",
			outcome: api.RunOutcome{Status: "done", Summary: "Hello there."}, want: false,
		},
		{
			name: "failed without evidence", input: "Always lead with the conclusion",
			outcome: api.RunOutcome{Status: "failed", Summary: "The request failed before producing evidence."}, want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := postRunMemoryEligible(tc.input, tc.outcome); got != tc.want {
				t.Fatalf("postRunMemoryEligible()=%v want %v", got, tc.want)
			}
		})
	}
}

func TestShortNaturalPreferenceSchedulesPostRunMaintenance(t *testing.T) {
	provider := newSlowLLMProvider("明白，之后会先给出结论。")
	provider.releaseNow()
	daemon, _, _ := newDetachedRunServer(t, provider)
	fake := &fakeLabeler{}
	daemon.PostRunAnalyzer = fake

	runOrdinaryTurn(t, daemon, "以后回答先给结论")
	if fake.callCount() != 1 {
		t.Fatalf("maintenance calls=%d want 1", fake.callCount())
	}
}

func hasEventOfType(t *testing.T, store *control.Store, taskID, eventType string) bool {
	t.Helper()
	events, err := store.ListTaskEvents(context.Background(), taskID, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == eventType {
			return true
		}
	}
	return false
}

func TestPostRunProviderFailureBlocksWithoutRetry(t *testing.T) {
	provider := newSlowLLMProvider("completed the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	fake := &fakeLabeler{err: errors.New("provider 403: quota exhausted")}
	daemon.PostRunAnalyzer = fake

	resp := runOrdinaryTurn(t, daemon, durableTurnInput("concise Go code review preferences"))
	runs, err := store.ListTaskRuns(context.Background(), resp.Task.TenantID, resp.Task.ID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v err=%v", runs, err)
	}
	job, err := store.GetMaintenanceJob(context.Background(), resp.Task.TenantID, runs[0].ID, postRunAnalyzerVersion)
	if err != nil || job == nil {
		t.Fatalf("job = %+v err=%v", job, err)
	}
	if job.Status != control.MaintenanceJobBlockedProvider || job.Attempts != 1 {
		t.Fatalf("provider failure must block after one attempt: %+v", job)
	}

	daemon.runMaintenancePass(context.Background())
	if fake.callCount() != 1 {
		t.Fatalf("blocked provider was retried: calls=%d", fake.callCount())
	}
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	diag, err := daemon.diagReply(context.Background(), identity)
	if err != nil || !strings.Contains(diag, "Background learning: paused (1 job(s))") {
		t.Fatalf("diag = %q err=%v", diag, err)
	}
}

// TestOrdinaryMessagesOwnFreshTasks pins the P2 attach rule that replaced
// pre-label stickiness: every ordinary message creates its own root task, and
// no maintenance decision moves it afterwards. Wrong-looking grouping is a
// display concern handled by derived list priority, never execution state.
func TestOrdinaryMessagesOwnFreshTasks(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)

	first := runOrdinaryTurn(t, daemon, "build the KOF game")
	second := runOrdinaryTurn(t, daemon, "check my cloud invoices")
	if first.Task == nil || second.Task == nil || first.Task.ID == second.Task.ID {
		t.Fatalf("each ordinary message must own a fresh task: first=%+v second=%+v", first.Task, second.Task)
	}
	for _, resp := range []api.MessageResponse{first, second} {
		runs, err := store.ListTaskRuns(context.Background(), resp.Task.TenantID, resp.Task.ID, 10)
		if err != nil || len(runs) != 1 {
			t.Fatalf("task %s runs=%+v err=%v", resp.Task.ID, runs, err)
		}
	}
}

// TestLegacyFrozenTaskDecisionIsIgnored pins the analyzer-version cutover
// contract (proposal §10.2): a frozen v2 proposal migrated onto the current
// generation may still carry a KEEP/MOVE/NEW/INBOX ruling. The apply path must
// audit and ignore it — never replay routing under new semantics.
func TestLegacyFrozenTaskDecisionIsIgnored(t *testing.T) {
	provider := newSlowLLMProvider("completed the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	fake := &fakeLabeler{reply: ""}
	daemon.PostRunAnalyzer = fake

	resp, status := daemon.ProcessMessage(context.Background(), api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: durableTurnInput("the legacy migration scenario"),
	})
	if status != 200 || resp.Task == nil {
		t.Fatalf("turn failed: status=%d resp=%+v", status, resp)
	}
	runs, err := store.ListTaskRuns(context.Background(), resp.Task.TenantID, resp.Task.ID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	// Simulate the migrated frozen v2 proposal: the version migration copies
	// proposal_json onto the current-generation row before any worker pass.
	// (SaveMaintenanceProposal persists only on a running job, so claim, save,
	// and park it exactly like a crash between proposal and apply would.)
	if claimed, _, err := store.ClaimMaintenanceJobWithLimit(context.Background(), resp.Task.TenantID, runs[0].ID, postRunAnalyzerVersion, 5); err != nil || !claimed {
		t.Fatalf("claim job: claimed=%v err=%v", claimed, err)
	}
	if err := store.SaveMaintenanceProposal(context.Background(), resp.Task.TenantID, runs[0].ID,
		postRunAnalyzerVersion, `{"task_decision":"MOVE:task_does-not-exist"}`, "legacy-hash"); err != nil {
		t.Fatal(err)
	}
	if err := store.FailMaintenanceJob(context.Background(), resp.Task.TenantID, runs[0].ID,
		postRunAnalyzerVersion, "simulated crash before apply", 0); err != nil {
		t.Fatal(err)
	}
	drainPostRunMaintenance(daemon)

	if fake.callCount() != 0 {
		t.Fatalf("a frozen proposal must replay without a model call, got %d", fake.callCount())
	}
	after, err := store.GetRun(context.Background(), resp.Task.TenantID, runs[0].ID)
	if err != nil || after == nil || after.TaskID != resp.Task.ID {
		t.Fatalf("legacy MOVE must not re-point the run: %+v err=%v", after, err)
	}
	if !hasEventOfType(t, store, resp.Task.ID, "label.assigned") {
		t.Fatal("ignored legacy decision must stay auditable")
	}
	job, err := store.GetMaintenanceJob(context.Background(), resp.Task.TenantID, runs[0].ID, postRunAnalyzerVersion)
	if err != nil || job == nil || job.Status != control.MaintenanceJobSucceeded {
		t.Fatalf("job = %+v err=%v", job, err)
	}
}

// TestMaintenanceSkipsNonDurableTurn: a short chat turn with no durable
// evidence never spends a maintenance model call.
func TestMaintenanceSkipsNonDurableTurn(t *testing.T) {
	provider := newSlowLLMProvider("hi")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	fake := &fakeLabeler{}
	daemon.PostRunAnalyzer = fake

	resp := runOrdinaryTurn(t, daemon, "你好")
	if fake.callCount() != 0 {
		t.Fatalf("non-durable turn must skip the analyzer, got %d calls", fake.callCount())
	}
	runs, err := store.ListTaskRuns(context.Background(), resp.Task.TenantID, resp.Task.ID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	job, err := store.GetMaintenanceJob(context.Background(), resp.Task.TenantID, runs[0].ID, postRunAnalyzerVersion)
	if err != nil || job == nil || job.Status != control.MaintenanceJobSkipped {
		t.Fatalf("job = %+v err=%v", job, err)
	}
}

// TestNilAnalyzerNoops: no analyzer wired (eval, no cheap model) → the turn
// completes and the task reaches its derived lifecycle without maintenance.
func TestNilAnalyzerNoops(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)

	resp := runOrdinaryTurn(t, daemon, "just do the work")
	task, _ := store.GetTask(context.Background(), resp.Task.TenantID, resp.Task.ID)
	if task == nil {
		t.Fatal("task must exist with a nil analyzer")
	}
	if hasEventOfType(t, store, task.ID, "label.assigned") {
		t.Fatal("nil analyzer must not record label events")
	}
}

// TestSubstantiveTurnGetsOneMaintenanceCallWithoutRelabeling: an explicit
// attach with durable content receives exactly one combined maintenance call,
// and nothing renames or moves the task afterwards.
func TestSubstantiveTurnGetsOneMaintenanceCallWithoutRelabeling(t *testing.T) {
	provider := newSlowLLMProvider("completed a substantive implementation and verified the result")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "explicit durable target")
	fake := &fakeLabeler{reply: "TITLE:must be ignored"}
	daemon.PostRunAnalyzer = fake
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: durableTurnInput("the explicitly selected implementation"), TaskID: parked.ID,
	})
	if status != 200 || resp.Task == nil || resp.Task.ID != parked.ID {
		t.Fatalf("explicit attach failed: %+v", resp)
	}
	drainPostRunMaintenance(daemon)
	if fake.callCount() != 1 {
		t.Fatalf("substantive run should receive one combined maintenance call, got %d", fake.callCount())
	}
	after, _ := store.GetTask(ctx, parked.TenantID, parked.ID)
	if after == nil || after.Title != parked.Title {
		t.Fatalf("task was relabeled: before=%+v after=%+v", parked, after)
	}
}

// TestMaintenanceDoesNotBlockResponse: the turn's response returns while the
// analyzer is still deliberating.
func TestMaintenanceDoesNotBlockResponse(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	fake := &fakeLabeler{block: make(chan struct{})}
	daemon.PostRunAnalyzer = fake

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: durableTurnInput("the slow maintenance turn"),
	})
	if status != 200 || resp.Task == nil {
		t.Fatalf("turn failed: status=%d resp=%+v", status, resp)
	}
	// The response is here while the analyzer is still blocked.
	if before, _ := store.GetTask(ctx, resp.Task.TenantID, resp.Task.ID); before == nil {
		t.Fatal("task must exist before maintenance completes")
	}
	done := make(chan struct{})
	go func() {
		drainPostRunMaintenance(daemon)
		close(done)
	}()
	waitUntil(t, 2*time.Second, func() bool { return fake.callCount() == 1 }, "maintenance worker did not start the analyzer")
	close(fake.block)
	<-done
	runs, err := store.ListTaskRuns(ctx, resp.Task.TenantID, resp.Task.ID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	waitUntil(t, 2*time.Second, func() bool {
		job, _ := store.GetMaintenanceJob(ctx, resp.Task.TenantID, runs[0].ID, postRunAnalyzerVersion)
		return job != nil && job.Status == control.MaintenanceJobSucceeded
	}, "maintenance result was never applied after release")
}
