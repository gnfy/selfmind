package httpapi

// Post-run labeler (Work Timeline P3): after a run finalizes, a cheap-model
// judge may KEEP the pre-label, MOVE the run to another open label (cleaning
// an empty auto-created placeholder), or TITLE a new placeholder once. The
// domain is harmless — every failure degrades to KEEP — and the call runs
// post-finalize without blocking the response.

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

// fakeLabeler is a scripted RunLabeler that records prompts and can block
// until released (to prove the response is not gated on labeling).
type fakeLabeler struct {
	mu      sync.Mutex
	reply   string
	err     error
	calls   int
	prompts []string
	block   chan struct{} // when non-nil, Label waits for it (or ctx)
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

	resp := runOrdinaryTurn(t, daemon, "Please remember that I prefer concise Go code reviews, with tests and specific file references.")
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

// TestLabelerMoveRepointsRunAndCleansPlaceholder: MOVE re-points the run (and
// its events) to the chosen open label, deletes the empty auto-created
// placeholder, repoints current_task, and records label.assigned on the
// target.
func TestLabelerMoveRepointsRunAndCleansPlaceholder(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	// Open MOVE target, then park a terminal current so the next ordinary
	// message creates a fresh placeholder.
	target := parkEmptyTask(t, daemon, "KOF game build")
	if err := store.UpdateTaskStatus(ctx, target.TenantID, target.ID, "in_progress", "built index.html", nil); err != nil {
		t.Fatal(err)
	}
	closer := parkEmptyTask(t, daemon, "finished thing")
	if err := store.UpdateTaskStatus(ctx, closer.TenantID, closer.ID, "done", "", nil); err != nil {
		t.Fatal(err)
	}

	daemon.PostRunAnalyzer = &fakeLabeler{reply: "MOVE:" + target.ID}
	resp := runOrdinaryTurn(t, daemon, "make the KOF characters look sharper")
	placeholderID := resp.Task.ID
	if placeholderID == target.ID {
		t.Fatalf("setup: expected a fresh placeholder, got the target itself")
	}

	// The run (and its events) now live on the target label.
	runs, err := store.ListTaskRuns(ctx, target.TenantID, target.ID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("run was not re-pointed to the target label: %d (%v)", len(runs), err)
	}
	events, _ := store.ListTaskEvents(ctx, target.ID, 50)
	sawRunEvent := false
	for _, ev := range events {
		if ev.RunID == runs[0].ID && ev.Type == "run.finished" {
			sawRunEvent = true
		}
	}
	if !sawRunEvent {
		t.Fatalf("the run's events must move with it; target events: %+v", events)
	}
	// The empty placeholder is gone; the current pointer follows the run.
	if ghost, _ := store.GetTask(ctx, target.TenantID, placeholderID); ghost != nil {
		t.Fatalf("empty auto-created placeholder should be deleted, got %+v", ghost)
	}
	if current, _ := store.CurrentTask(ctx, target.TenantID, target.PersonID); current == nil || current.ID != target.ID {
		t.Fatalf("current task should follow the moved run, got %+v", current)
	}
	if !hasEventOfType(t, store, target.ID, "label.assigned") {
		t.Fatal("label.assigned provenance event missing on the target label")
	}
}

func TestUniqueWorkKeyAttachesBeforeLabeler(t *testing.T) {
	provider := newSlowLLMProvider("completed release verification for RUQX-224")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	target := parkEmptyTask(t, daemon, "Release RUQX-224 to production")
	if err := store.UpdateTaskStatus(ctx, target.TenantID, target.ID, "in_progress", "awaiting deployment", nil); err != nil {
		t.Fatal(err)
	}
	closer := parkEmptyTask(t, daemon, "closed current")
	if err := store.UpdateTaskStatus(ctx, closer.TenantID, closer.ID, "done", "", nil); err != nil {
		t.Fatal(err)
	}

	daemon.PostRunAnalyzer = &fakeLabeler{reply: "KEEP"}
	resp := runOrdinaryTurn(t, daemon, "RUQX-224 production deployment needs verification")
	if resp.Task == nil || resp.Task.ID != target.ID {
		t.Fatalf("ingress work key must attach directly to %s: %+v", target.ID, resp.Task)
	}
	runs, err := store.ListTaskRuns(ctx, target.TenantID, target.ID, 10)
	if err != nil || len(runs) != 1 {
		tasks, _ := store.ListTasks(ctx, target.TenantID, target.PersonID, 20)
		fake := daemon.PostRunAnalyzer.(*fakeLabeler)
		fake.mu.Lock()
		prompts := append([]string(nil), fake.prompts...)
		fake.mu.Unlock()
		t.Fatalf("unique work key did not attach the run: runs=%+v err=%v tasks=%+v prompts=%q", runs, err, tasks, prompts)
	}
	if !hasEventOfType(t, store, target.ID, "label.assigned") {
		t.Fatal("deterministic ingress work-key decision must remain auditable")
	}
}

// TestLabelerTitleSetsPlaceholderTitleOnce: TITLE names a NEW placeholder
// once; an established label (pre-label reuse) is never retitled by the
// labeler.
func TestLabelerTitleSetsPlaceholderTitleOnce(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	fake := &fakeLabeler{reply: "TITLE:Build the KOF fighting game"}
	daemon.PostRunAnalyzer = fake

	// No current task yet → the first ordinary message creates a placeholder.
	resp := runOrdinaryTurn(t, daemon, "用JS写一个KOF格斗游戏,先做两个角色")
	created, _ := store.GetTask(ctx, resp.Task.TenantID, resp.Task.ID)
	if created == nil || created.Title != "Build the KOF fighting game" {
		t.Fatalf("placeholder title = %+v, want the labeler's TITLE", created)
	}
	if !hasEventOfType(t, store, created.ID, "label.assigned") {
		t.Fatal("label.assigned provenance event missing for the TITLE decision")
	}

	// Second turn pre-labels onto the SAME (now open, established) label; a
	// TITLE reply must be ignored — established labels are renamed by humans
	// only.
	fake.mu.Lock()
	fake.reply = "TITLE:Something else entirely"
	fake.mu.Unlock()
	resp2 := runOrdinaryTurn(t, daemon, "add another playable character")
	if resp2.Task.ID != created.ID {
		t.Fatalf("second turn should pre-label onto the same open label")
	}
	after, _ := store.GetTask(ctx, created.TenantID, created.ID)
	if after == nil || after.Title != "Build the KOF fighting game" {
		t.Fatalf("labeler retitled an established label: %+v", after)
	}
}

// TestLabelerKeepAndGarbageAreNoops: KEEP (and any unparsable reply) changes
// nothing and records no provenance event.
func TestLabelerKeepAndGarbageAreNoops(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	for _, reply := range []string{"KEEP", "definitely not a decision"} {
		daemon.PostRunAnalyzer = &fakeLabeler{reply: reply}
		resp := runOrdinaryTurn(t, daemon, "do a thing ("+reply+")")
		task, _ := store.GetTask(ctx, resp.Task.TenantID, resp.Task.ID)
		if task == nil {
			t.Fatalf("reply %q must keep the task, got nil", reply)
		}
		if hasEventOfType(t, store, task.ID, "label.assigned") {
			t.Fatalf("reply %q must not record a label.assigned event", reply)
		}
	}
}

// TestNilLabelerNoops: no labeler wired (eval, no cheap model) → the
// pre-label simply stands.
func TestNilLabelerNoops(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)

	resp := runOrdinaryTurn(t, daemon, "just do the work")
	task, _ := store.GetTask(context.Background(), resp.Task.TenantID, resp.Task.ID)
	if task == nil {
		t.Fatal("task must exist with a nil labeler")
	}
	if hasEventOfType(t, store, task.ID, "label.assigned") {
		t.Fatal("nil labeler must not record label events")
	}
}

func TestLabelerSkipsEstablishedSoleOpenLabel(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, _, _ := newDetachedRunServer(t, provider)
	fake := &fakeLabeler{reply: "KEEP"}
	daemon.PostRunAnalyzer = fake

	first := runOrdinaryTurn(t, daemon, "build the first version")
	if fake.callCount() != 1 {
		t.Fatalf("new placeholder should be labeled once, got %d calls", fake.callCount())
	}
	second := runOrdinaryTurn(t, daemon, "improve the same version")
	if second.Task == nil || first.Task == nil || second.Task.ID != first.Task.ID {
		t.Fatalf("expected established label reuse: first=%+v second=%+v", first.Task, second.Task)
	}
	if fake.callCount() != 1 {
		t.Fatalf("sole established open label must skip redundant judge call, got %d", fake.callCount())
	}
}

func TestLabelerInboxHidesCasualRunAndClearsPlaceholder(t *testing.T) {
	provider := newSlowLLMProvider("I am SelfMind")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	daemon.TaskGovernance = TaskGovernanceOptions{InboxEnabled: true}
	daemon.PostRunAnalyzer = &fakeLabeler{reply: "INBOX"}
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Me")
	if err != nil {
		t.Fatal(err)
	}

	resp := runOrdinaryTurn(t, daemon, "who are you?")
	if ghost, _ := store.GetTask(context.Background(), identity.TenantID, resp.Task.ID); ghost != nil {
		t.Fatalf("auto-created visible placeholder should be removed, got %+v", ghost)
	}
	inbox, err := store.EnsureInboxTask(context.Background(), identity.TenantID, identity.PersonID, "")
	if err != nil {
		t.Fatal(err)
	}
	runs, err := store.ListTaskRuns(context.Background(), identity.TenantID, inbox.ID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("casual run was not preserved in inbox: runs=%+v err=%v", runs, err)
	}
	if current, _ := store.CurrentTask(context.Background(), identity.TenantID, identity.PersonID); current != nil {
		t.Fatalf("inbox must never become current task: %+v", current)
	}
	if visible, _ := store.ListTasks(context.Background(), identity.TenantID, identity.PersonID, 20); len(visible) != 0 {
		t.Fatalf("inbox run polluted visible task list: %+v", visible)
	}
	if !hasEventOfType(t, store, inbox.ID, "label.assigned") {
		t.Fatal("inbox assignment must remain auditable")
	}
}

func TestParseRunLabelReplyInbox(t *testing.T) {
	decision, arg := parseRunLabelReply("INBOX\nignored")
	if decision != "INBOX" || arg != "" {
		t.Fatalf("decision=%q arg=%q", decision, arg)
	}
}

func TestPostRunInboxEligibilityRequiresNondurableCompletedTurn(t *testing.T) {
	tests := []struct {
		name    string
		outcome api.RunOutcome
		workKey string
		want    bool
	}{
		{name: "casual completed turn", outcome: api.RunOutcome{Status: "done"}, want: true},
		{name: "issue key", outcome: api.RunOutcome{Status: "done"}, workKey: "RUQX-248", want: false},
		{name: "changed file", outcome: api.RunOutcome{Status: "done", Files: []string{"main.go"}}, want: false},
		{name: "durable result", outcome: api.RunOutcome{Status: "done", Done: []string{"permission inventory recorded"}}, want: false},
		{name: "next step", outcome: api.RunOutcome{Status: "done", NextSteps: []string{"apply the reviewed change"}}, want: false},
		{name: "verification evidence", outcome: api.RunOutcome{Status: "done", Verification: &api.VerificationOutcome{State: "passed"}}, want: false},
		{name: "waiting user", outcome: api.RunOutcome{Status: "waiting_user"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := postRunInboxEligible(tc.outcome, tc.workKey); got != tc.want {
				t.Fatalf("postRunInboxEligible()=%v want %v for %+v", got, tc.want, tc.outcome)
			}
		})
	}
}

// TestLabelerSkippedForExplicitAttach: an explicit attach (req.TaskID) is the
// user's decision — the labeler is never consulted.
func TestLabelerSkippedForExplicitAttach(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, _, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "explicit target")
	fake := &fakeLabeler{reply: "KEEP"}
	daemon.PostRunAnalyzer = fake

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: "work on it", TaskID: parked.ID,
	})
	if status != 200 || resp.Task == nil || resp.Task.ID != parked.ID {
		t.Fatalf("explicit attach failed: %+v", resp.Task)
	}
	drainPostRunMaintenance(daemon)
	if fake.callCount() != 0 {
		t.Fatalf("labeler must not run for explicit attaches, got %d calls", fake.callCount())
	}
}

func TestPostRunAnalyzerLearnsFromSubstantiveExplicitAttachWithoutRelabeling(t *testing.T) {
	provider := newSlowLLMProvider("completed a substantive implementation and verified the result")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	parked := parkEmptyTask(t, daemon, "explicit durable target")
	fake := &fakeLabeler{reply: "TITLE:must be ignored"}
	daemon.PostRunAnalyzer = fake
	input := strings.Repeat("Continue the explicitly selected implementation with durable design constraints. ", 4)
	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli",
		Content: input, TaskID: parked.ID,
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
		t.Fatalf("explicit task was relabeled: before=%+v after=%+v", parked, after)
	}
}

// TestLabelerDoesNotBlockResponse: the turn's response returns while the
// labeler is still deliberating; the decision applies afterwards.
func TestLabelerDoesNotBlockResponse(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	fake := &fakeLabeler{reply: "TITLE:Named after release", block: make(chan struct{})}
	daemon.PostRunAnalyzer = fake

	resp, status := daemon.ProcessMessage(ctx, api.MessageRequest{
		Platform: "cli", PlatformUserID: "local", Channel: "cli", Content: "slow labeling turn",
	})
	if status != 200 || resp.Task == nil {
		t.Fatalf("turn failed: status=%d resp=%+v", status, resp)
	}
	// The response is here, the labeler is still blocked: the title is still
	// the provisional one.
	before, _ := store.GetTask(ctx, resp.Task.TenantID, resp.Task.ID)
	if before == nil || before.Title != "slow labeling turn" {
		t.Fatalf("provisional title should stand while the labeler deliberates: %+v", before)
	}
	done := make(chan struct{})
	go func() {
		drainPostRunMaintenance(daemon)
		close(done)
	}()
	waitUntil(t, 2*time.Second, func() bool { return fake.callCount() == 1 }, "maintenance worker did not start the labeler")
	close(fake.block)
	<-done
	waitUntil(t, 2*time.Second, func() bool {
		after, _ := store.GetTask(ctx, resp.Task.TenantID, resp.Task.ID)
		return after != nil && after.Title == "Named after release"
	}, "labeler decision was never applied after release")
}

// TestLabelerMoveTargetMustBeOffered: a MOVE to a task id that was not in the
// offered open-label list (e.g. hallucinated or terminal) degrades to KEEP.
func TestLabelerMoveTargetMustBeOffered(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	daemon.PostRunAnalyzer = &fakeLabeler{reply: "MOVE:task_does-not-exist"}
	resp := runOrdinaryTurn(t, daemon, "work with a lying labeler")
	task, _ := store.GetTask(ctx, resp.Task.TenantID, resp.Task.ID)
	if task == nil {
		t.Fatal("an unoffered MOVE target must degrade to KEEP")
	}
	runs, _ := store.ListTaskRuns(ctx, resp.Task.TenantID, task.ID, 10)
	if len(runs) != 1 {
		t.Fatalf("run must stay on the pre-label, got %d runs", len(runs))
	}
}

func TestRejectedLabelDecisionReconcilesWeakTaskLifecycle(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()
	identity, err := store.ResolveOrCreateAccount(ctx, "default", "cli", "local", "Me")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Established work", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID, PersonID: identity.PersonID, Title: "Other offered work", Channel: "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &control.Run{ID: "run_rejected_label"}
	daemon.applyPostRunLabel(ctx, identity, task, run, taskAttach{preLabel: true}, []control.Task{*target}, "", api.RunOutcome{Status: "done"}, "MOVE:task_not_offered")

	stored, err := store.GetTask(ctx, identity.TenantID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Status != "done" {
		t.Fatalf("rejected label decision must degrade to KEEP and reconcile lifecycle: %+v", stored)
	}
}

// TestLabelerPromptCarriesTurnAndCandidates sanity-checks the prompt contract:
// current label, open candidates, and the turn inside data delimiters.
func TestLabelerPromptCarriesTurnAndCandidates(t *testing.T) {
	provider := newSlowLLMProvider("making progress on the requested work")
	provider.releaseNow()
	daemon, store, _ := newDetachedRunServer(t, provider)
	ctx := context.Background()

	target := parkEmptyTask(t, daemon, "open candidate")
	if err := store.UpdateTaskStatus(ctx, target.TenantID, target.ID, "in_progress", "", nil); err != nil {
		t.Fatal(err)
	}
	closer := parkEmptyTask(t, daemon, "closed current")
	if err := store.UpdateTaskStatus(ctx, closer.TenantID, closer.ID, "done", "", nil); err != nil {
		t.Fatal(err)
	}

	fake := &fakeLabeler{reply: "KEEP"}
	daemon.PostRunAnalyzer = fake
	runOrdinaryTurn(t, daemon, "the unique turn text marker")

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.prompts) != 1 {
		t.Fatalf("labeler consulted %d times, want 1", len(fake.prompts))
	}
	prompt := fake.prompts[0]
	for _, want := range []string{target.ID, "<turn-data>", "the unique turn text marker", "new placeholder: true"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("labeler prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, closer.ID) {
		t.Fatalf("terminal tasks must not be offered as MOVE candidates:\n%s", prompt)
	}
}
