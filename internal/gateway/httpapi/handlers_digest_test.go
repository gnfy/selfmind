package httpapi

// GET /v1/digest (attach digest, G0-c): reopening the CLI must get one
// bounded person-scoped summary — finished/disrupted tasks, pending
// approvals, unconfirmed pushes, and the mid-flight run — anchored on the CLI
// account's last presence, and an empty world must produce an empty digest.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/control/controltest"
	"selfmind/internal/gateway/api"
)

func newDigestTestServer(t *testing.T) (*Server, *control.Store, *control.IdentityContext) {
	t.Helper()
	t.Setenv("SELF_GATEWAY_TOKEN", "")
	t.Setenv("SELF_DAEMON_TOKEN", "")
	store := controltest.NewStore(t)
	t.Cleanup(func() { _ = store.Close() })
	identity, err := store.ResolveOrCreateAccount(context.Background(), "default", "cli", "local", "Local User")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Control: store, DefaultTenantID: "default"}, store, identity
}

func fetchDigest(t *testing.T, daemon *Server) api.DigestResponse {
	t.Helper()
	srv := httptest.NewServer(daemon.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/digest?platform=cli&platform_user_id=local")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("digest status = %d", resp.StatusCode)
	}
	var digest api.DigestResponse
	if err := json.NewDecoder(resp.Body).Decode(&digest); err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestDigestReportsAwayStateAndActiveRun(t *testing.T) {
	daemon, store, identity := newDigestTestServer(t)
	ctx := context.Background()

	mkTask := func(title, status string) *control.Task {
		t.Helper()
		task, err := store.CreateTask(ctx, control.TaskCreate{
			TenantID: identity.TenantID,
			PersonID: identity.PersonID,
			Title:    title,
			Channel:  "cli",
		})
		if err != nil {
			t.Fatal(err)
		}
		switch status {
		case "done", "completed", "failed", "interrupted", api.RunStatusVerificationPartial:
			run, err := store.StartRun(ctx, task, "cli", title)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.MaterializeRunFinalization(ctx, control.RunFinalization{
				Identity: *identity, RunID: run.ID, RunStatus: status,
				TaskID: task.ID, TaskStatus: status, Summary: "outcome of " + title,
			}); err != nil {
				t.Fatal(err)
			}
		default:
			if err := store.UpdateThreadSummary(ctx, identity.TenantID, task.ID, "outcome of "+title, nil); err != nil {
				t.Fatal(err)
			}
		}
		return task
	}
	finished := mkTask("Ship the weekly report", "completed")
	interrupted := mkTask("Refactor the parser", "interrupted")

	payload, _ := json.Marshal(map[string]interface{}{
		"tool":   "terminal",
		"reason": "destructive command",
		"args":   map[string]interface{}{"command": "rm -rf build"},
		"decisions": []map[string]interface{}{
			{"id": "once", "label": "Yes, proceed", "decision": "approved", "key": "y"},
			{"id": "deny", "label": "No", "decision": "rejected", "key": "n"},
		},
	})
	approval, err := store.CreateApprovalRequest(ctx, control.ApprovalRequest{
		TenantID:   identity.TenantID,
		PersonID:   identity.PersonID,
		TaskID:     finished.ID,
		ActionType: "tool_call",
		Payload:    payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.ParkApprovalRequest(ctx, identity.TenantID, approval.ID, "resource budget elapsed"); err != nil || !changed {
		t.Fatalf("park approval changed=%v err=%v", changed, err)
	}

	push, err := store.EnqueueDelivery(ctx, control.Delivery{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Platform: "weixin",
		Channel:  "weixin",
		Content:  "build finished — line one\nline two",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeliverySentUnconfirmed(ctx, push.ID); err != nil {
		t.Fatal(err)
	}

	running := mkTask("Long migration", "running")
	daemon.coordinator().beginActive(identity.PersonID, &activeRun{
		TenantID:  identity.TenantID,
		PersonID:  identity.PersonID,
		TaskID:    running.ID,
		RunID:     "run_1",
		Channel:   "cli",
		Summary:   "migrate the database",
		StartedAt: time.Now().Add(-12 * time.Minute),
	})

	digest := fetchDigest(t, daemon)
	if digest.Empty() {
		t.Fatal("digest must not be empty")
	}
	if len(digest.FinishedTasks) != 1 || digest.FinishedTasks[0].ID != finished.ID ||
		digest.FinishedTasks[0].Title != "Ship the weekly report" ||
		digest.FinishedTasks[0].Summary != "outcome of Ship the weekly report" {
		t.Fatalf("finished tasks = %+v", digest.FinishedTasks)
	}
	if len(digest.DisruptedTasks) != 1 || digest.DisruptedTasks[0].ID != interrupted.ID ||
		digest.DisruptedTasks[0].Status != "interrupted" {
		t.Fatalf("disrupted tasks = %+v", digest.DisruptedTasks)
	}
	if len(digest.PendingApprovals) != 1 || digest.PendingApprovals[0].ID == "" ||
		!strings.Contains(digest.PendingApprovals[0].Line, "[terminal]") {
		t.Fatalf("pending approvals = %+v", digest.PendingApprovals)
	}
	if digest.PendingApprovals[0].WaiterState != "parked" || digest.PendingApprovals[0].Tool != "terminal" ||
		len(digest.PendingApprovals[0].Decisions) != 2 || digest.PendingApprovals[0].Decisions[0].Label != "Yes, proceed" {
		t.Fatalf("interactive approval digest = %+v", digest.PendingApprovals[0])
	}
	if len(digest.UnconfirmedPushes) != 1 || digest.UnconfirmedPushes[0].Platform != "weixin" ||
		digest.UnconfirmedPushes[0].Status != "sent_unconfirmed" {
		t.Fatalf("unconfirmed pushes = %+v", digest.UnconfirmedPushes)
	}
	if strings.Contains(digest.UnconfirmedPushes[0].Preview, "\n") {
		t.Fatalf("push preview must be one line: %q", digest.UnconfirmedPushes[0].Preview)
	}
	if digest.ActiveRun == nil || digest.ActiveRun.RunID != "run_1" ||
		digest.ActiveRun.TaskID != running.ID ||
		digest.ActiveRun.Title != "Long migration" || digest.ActiveRun.ElapsedSeconds < 11*60 {
		t.Fatalf("active run = %+v", digest.ActiveRun)
	}
	// Never-seen account: the anchor is the 24h fallback window, not zero.
	if got := time.Unix(digest.SinceUnix, 0); time.Since(got) > 25*time.Hour || time.Since(got) < 23*time.Hour {
		t.Fatalf("fallback anchor not ~24h ago: %v", got)
	}
}

func TestDigestEmptyWorldStaysEmpty(t *testing.T) {
	daemon, _, _ := newDigestTestServer(t)
	digest := fetchDigest(t, daemon)
	if !digest.Empty() {
		t.Fatalf("fresh world must produce an empty digest: %+v", digest)
	}
}

// TestDigestReportsApprovalMode: the digest carries the person's effective
// approval mode so a client can show it in the status bar from startup. It is
// person state, not a "while you were away" item, so it never makes the digest
// non-empty.
func TestDigestReportsApprovalMode(t *testing.T) {
	daemon, store, identity := newDigestTestServer(t)

	// Unset → on-request default, and the digest still reads as empty.
	digest := fetchDigest(t, daemon)
	if digest.ApprovalMode != "smart" {
		t.Fatalf("default approval mode = %q, want smart", digest.ApprovalMode)
	}
	if !digest.Empty() {
		t.Fatalf("approval mode must not make the digest non-empty: %+v", digest)
	}

	// Persisted mode is reflected.
	if err := store.SetPersonSetting(context.Background(), identity.TenantID, identity.PersonID, personSettingApprovalMode, "smart"); err != nil {
		t.Fatal(err)
	}
	digest = fetchDigest(t, daemon)
	if digest.ApprovalMode != "smart" {
		t.Fatalf("approval mode = %q, want smart", digest.ApprovalMode)
	}
	if !digest.Empty() {
		t.Fatalf("approval mode must not make the digest non-empty: %+v", digest)
	}
}

// TestDigestAnchorsOnAccountLastSeen: once a presence beat has stamped
// accounts.last_seen_at, the digest window starts there — tasks that finished
// while the user was watching are not re-reported on the next attach.
func TestDigestAnchorsOnAccountLastSeen(t *testing.T) {
	daemon, store, identity := newDigestTestServer(t)
	ctx := context.Background()

	// Simulate the presence beat (what the TUI's ping loop does while open).
	daemon.touchPresence(ctx, identity)
	accounts, err := store.ListAccountsByPerson(ctx, identity.TenantID, identity.PersonID)
	if err != nil || len(accounts) != 1 || accounts[0].LastSeenAt == 0 {
		t.Fatalf("presence beat did not stamp last_seen_at: %+v, %v", accounts, err)
	}

	digest := fetchDigest(t, daemon)
	if digest.SinceUnix != accounts[0].LastSeenAt {
		t.Fatalf("digest anchor = %d, want the account's last_seen_at %d", digest.SinceUnix, accounts[0].LastSeenAt)
	}

	// A task finishing after the anchor shows up.
	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "After the beat",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "finish after the beat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaterializeRunFinalization(ctx, control.RunFinalization{
		Identity: *identity, RunID: run.ID, RunStatus: "completed",
		TaskID: task.ID, TaskStatus: "completed", Summary: "finished after the beat",
	}); err != nil {
		t.Fatal(err)
	}
	digest = fetchDigest(t, daemon)
	if len(digest.FinishedTasks) != 1 || digest.FinishedTasks[0].ID != task.ID {
		t.Fatalf("task finished after the anchor missing: %+v", digest.FinishedTasks)
	}
}

// TestDigestDoesNotRedateAnOldInterruptionDuringLifecycleReconcile covers the
// startup false positive seen in the daily driver: post-run maintenance may
// reconcile a task card long after its interrupted run. That bookkeeping must
// not make the old run look as though it stopped while the CLI was away.
func TestDigestDoesNotRedateAnOldInterruptionDuringLifecycleReconcile(t *testing.T) {
	daemon, store, identity := newDigestTestServer(t)
	ctx := context.Background()

	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Implement binary search in Go",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.StartRun(ctx, task, "cli", "implement binary search")
	if err != nil {
		t.Fatal(err)
	}
	// The interruption left work evidence (next steps), which is what keeps an
	// interrupted Run resumable Attention rather than settled history.
	if _, err := store.MaterializeRunFinalization(ctx, control.RunFinalization{
		Identity:   *identity,
		RunID:      run.ID,
		RunStatus:  "interrupted",
		TaskID:     task.ID,
		TaskStatus: "interrupted",
		Summary:    "stopped before completion",
		NextSteps:  []string{"finish the binary search implementation"},
	}); err != nil {
		t.Fatal(err)
	}
	storedRun, err := store.GetRun(ctx, identity.TenantID, run.ID)
	if err != nil || storedRun == nil || storedRun.FinishedAt == nil {
		t.Fatalf("terminal run missing: %+v, %v", storedRun, err)
	}

	// Account presence and run timestamps have second precision. Move into the
	// next second so the old terminal event is unambiguously before the anchor.
	for time.Now().Unix() <= storedRun.FinishedAt.Unix() {
		time.Sleep(10 * time.Millisecond)
	}
	daemon.touchPresence(ctx, identity)

	// This is the bookkeeping operation maintenance runs after a KEEP/MOVE
	// decision. The old open blocker honestly keeps the task interrupted, but
	// there was no new interrupted run after the presence anchor.
	if err := store.ReconcileTaskAfterRun(ctx, identity.TenantID, task.ID, run.ID, "done"); err != nil {
		t.Fatal(err)
	}
	digest := fetchDigest(t, daemon)
	if len(digest.DisruptedTasks) != 0 {
		t.Fatalf("old interruption was falsely redated by reconcile: %+v", digest.DisruptedTasks)
	}
	if len(digest.UnresolvedTasks) != 1 || digest.UnresolvedTasks[0].ID != task.ID {
		t.Fatalf("old unresolved task must remain visible without a false away event: %+v", digest.UnresolvedTasks)
	}
}

// TestDigestActiveRunShowsProgress (owner request 2026-07-05): the digest's
// active-run entry must answer "where does it stand?" — the same plan source
// /status uses (latest plan.updated event) rendered as bounded checklist
// lines with the current step marked, plus a one-line latest activity note.
func TestDigestActiveRunShowsProgress(t *testing.T) {
	daemon, store, identity := newDigestTestServer(t)
	ctx := context.Background()

	task, err := store.CreateTask(ctx, control.TaskCreate{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		Title:    "Migrate the database",
		Channel:  "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	planPayload, _ := json.Marshal(map[string]interface{}{
		"plan": []map[string]string{
			{"step": "Dump the schema", "status": "completed"},
			{"step": "Rewrite the migrations", "status": "in_progress"},
			{"step": "Replay onto staging", "status": "pending"},
		},
	})
	if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, Type: "plan.updated", Visibility: "task", Payload: planPayload}); err != nil {
		t.Fatal(err)
	}
	thinkingPayload, _ := json.Marshal(map[string]string{"message": "rewriting migration 007"})
	if _, err := store.AppendEvent(ctx, control.Event{TaskID: task.ID, Type: "agent.thinking", Visibility: "task", Payload: thinkingPayload}); err != nil {
		t.Fatal(err)
	}

	daemon.coordinator().beginActive(identity.PersonID, &activeRun{
		TenantID:  identity.TenantID,
		PersonID:  identity.PersonID,
		TaskID:    task.ID,
		RunID:     "run_progress",
		Channel:   "cli",
		Summary:   "migrate the database",
		StartedAt: time.Now().Add(-3 * time.Minute),
	})

	digest := fetchDigest(t, daemon)
	if digest.ActiveRun == nil {
		t.Fatal("active run missing from the digest")
	}
	wantPlan := []string{
		"✓ Dump the schema",
		"→ Rewrite the migrations",
		"○ Replay onto staging",
	}
	if len(digest.ActiveRun.PlanSteps) != len(wantPlan) {
		t.Fatalf("plan steps = %v", digest.ActiveRun.PlanSteps)
	}
	for i, want := range wantPlan {
		if digest.ActiveRun.PlanSteps[i] != want {
			t.Fatalf("plan step %d = %q, want %q", i, digest.ActiveRun.PlanSteps[i], want)
		}
	}
	if digest.ActiveRun.LatestActivity != "rewriting migration 007" {
		t.Fatalf("latest activity = %q", digest.ActiveRun.LatestActivity)
	}
}

// TestDigestPlanLinesBounded: a long plan never turns the digest into a
// scrolling checklist — completed leading steps collapse, the current step
// stays visible, and the tail truncates with a "… N more steps" line, all
// within the digestPlanMaxLines budget.
func TestDigestPlanLinesBounded(t *testing.T) {
	mkPlan := func(n, current int) []taskPlanStep {
		plan := make([]taskPlanStep, 0, n)
		for i := 0; i < n; i++ {
			status := "pending"
			if i < current {
				status = "completed"
			} else if i == current {
				status = "in_progress"
			}
			plan = append(plan, taskPlanStep{Step: fmt.Sprintf("step %02d", i+1), Status: status})
		}
		return plan
	}

	// 20 pending steps, current at the front: head shown, tail collapsed.
	lines := digestPlanLines(mkPlan(20, 0))
	if len(lines) > digestPlanMaxLines {
		t.Fatalf("plan lines exceed the budget: %v", lines)
	}
	if lines[0] != "→ step 01" {
		t.Fatalf("first line = %q", lines[0])
	}
	if last := lines[len(lines)-1]; !strings.Contains(last, "more steps") {
		t.Fatalf("long plan must end with a more-steps marker: %v", lines)
	}

	// Current step deep in a 20-step plan: earlier done work collapses so the
	// current step is still visible.
	lines = digestPlanLines(mkPlan(20, 10))
	if len(lines) > digestPlanMaxLines {
		t.Fatalf("plan lines exceed the budget: %v", lines)
	}
	if !strings.Contains(lines[0], "10 earlier steps done") {
		t.Fatalf("completed prefix must collapse: %v", lines)
	}
	foundCurrent := false
	for _, line := range lines {
		if strings.HasPrefix(line, "→ step 11") {
			foundCurrent = true
		}
	}
	if !foundCurrent {
		t.Fatalf("current step must stay visible: %v", lines)
	}
	if last := lines[len(lines)-1]; !strings.Contains(last, "more steps") {
		t.Fatalf("tail must truncate with a marker: %v", lines)
	}

	// A short plan renders whole; an absent plan renders nothing.
	if lines = digestPlanLines(mkPlan(3, 1)); len(lines) != 3 {
		t.Fatalf("short plan must render whole: %v", lines)
	}
	if lines = digestPlanLines(nil); lines != nil {
		t.Fatalf("no plan must render nothing: %v", lines)
	}
}
