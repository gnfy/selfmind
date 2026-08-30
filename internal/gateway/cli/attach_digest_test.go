package cli

// Attach digest + re-attach (G0-c/G0-d): the startup digest renders once as a
// compact transcript block, an empty digest renders nothing, and an active
// run in the digest flips the composer into the run-active state so the
// existing steering / spinner / exit-prompt paths apply to the watched run.

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
)

func TestStartupDigestRendersOnceOnFirstSizedFrame(t *testing.T) {
	c := NewController(nil, nil, nil, "")
	c.SetClientMode(true)
	c.SetStartupDigest(&api.DigestResponse{
		FinishedTasks:  []api.DigestTask{{ID: "t1", Title: "Ship the report", Status: "completed"}, {ID: "t2", Title: "Fix the build", Status: "done"}},
		DisruptedTasks: []api.DigestTask{{ID: "t3", Title: "Refactor parser", Status: "interrupted"}},
		PendingApprovals: []api.DigestApproval{
			{
				ID: "apr_1", Line: "[terminal] command=rm -rf build — destructive command",
				Tool: "terminal", Target: "rm -rf build", Reason: "destructive command", WaiterState: "parked",
				Decisions: []api.ApprovalDecision{
					{ID: "once", Label: "Yes, proceed", Decision: "approved", Key: "y"},
					{ID: "deny", Label: "No", Decision: "rejected", Key: "n"},
				},
			},
		},
		UnconfirmedPushes: []api.DigestPush{{Platform: "weixin", Status: "sent_unconfirmed", Preview: "build finished"}},
	})
	model := c.model

	cmd := model.maybeShowStartupDigest(100)
	if cmd != nil {
		t.Fatal("no active run: digest must not start a watcher")
	}
	if len(model.messages) != 2 || model.messages[0].Role != "digest" {
		t.Fatalf("digest should render a digest plus restored approval prompt: %+v", model.messages)
	}
	text := model.messages[0].Content
	for _, want := range []string{
		"While you were away:",
		"✔ 2 tasks finished: Ship the report, Fix the build",
		"✖ 1 task stopped early: Refactor parser (use /resume to continue)",
		"Still needs attention:",
		"⚠ 1 approval waiting: [terminal] command=rm -rf build — destructive command — interactive choices restored below",
		"⚠ 1 push may not have reached weixin (see /status)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("digest missing %q:\n%s", want, text)
		}
	}
	if model.thinking {
		t.Fatal("digest without an active run must leave the composer idle")
	}
	if model.approvalPrompt == nil || model.pendingApprovalID != "apr_1" || !strings.Contains(model.approvalPrompt.View(100), "Yes, proceed") {
		t.Fatalf("approval panel was not restored from digest: pending=%q prompt=%+v", model.pendingApprovalID, model.approvalPrompt)
	}

	// Rendered once: a second sized frame must not repeat it.
	if cmd := model.maybeShowStartupDigest(120); cmd != nil || len(model.messages) != 2 {
		t.Fatalf("digest must render exactly once: %d messages", len(model.messages))
	}
}

func TestStartupDigestLabelsOlderUnresolvedWorkWithoutAwayClaim(t *testing.T) {
	text := formatStartupDigest(&api.DigestResponse{
		UnresolvedTasks: []api.DigestTask{{
			ID: "t-old", Title: "Implement binary search in Go", Status: "interrupted",
		}},
	})
	if strings.Contains(text, "While you were away:") || strings.Contains(text, "stopped early") {
		t.Fatalf("older unresolved work must not be presented as a new away event:\n%s", text)
	}
	for _, want := range []string{
		"Still needs attention:",
		"↻ 1 earlier task still needs attention: Implement binary search in Go (use /resume to continue)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("unresolved digest missing %q:\n%s", want, text)
		}
	}
}

func TestStartupDigestEmptyRendersNothing(t *testing.T) {
	c := NewController(nil, nil, nil, "")
	c.SetClientMode(true)
	c.SetStartupDigest(&api.DigestResponse{SinceUnix: 1751600000})
	model := c.model

	model.updateInner(tea.WindowSizeMsg{Width: 100, Height: 30})
	if len(model.messages) != 0 {
		t.Fatalf("empty digest must keep a fresh session clean: %+v", model.messages)
	}
	if !model.digestShown {
		t.Fatal("empty digest is still consumed (no later re-check)")
	}
}

// TestStartupDigestAttachesToActiveRun: an active run in the digest must (1)
// announce the attach, (2) flip the running-state flags so the composer
// behaves as if a run is active, and (3) start the run watcher.
func TestStartupDigestAttachesToActiveRun(t *testing.T) {
	c := NewController(nil, nil, nil, "")
	c.SetClientMode(true)
	watcherStarted := make(chan struct{})
	var watchedRunID string
	c.SetRunWatcher(func(ctx context.Context, runID string, observer httpapi.StreamObserver) string {
		watchedRunID = runID
		close(watcherStarted)
		<-ctx.Done()
		return ""
	})
	c.SetStartupDigest(&api.DigestResponse{
		ActiveRun: &api.DigestActiveRun{RunID: "r9", TaskID: "t9", Title: "Long migration", ElapsedSeconds: 720},
	})
	model := c.model

	_, cmd := model.updateInner(tea.WindowSizeMsg{Width: 100, Height: 30})
	if cmd == nil {
		t.Fatal("active run must produce the watcher command")
	}
	// Passive spectator: watchingRun is set but the composer is NOT captured
	// (m.thinking stays false) — the user can type a new task freely.
	if !model.watchingRun {
		t.Fatalf("attach must enter passive watch mode: watchingRun=%v", model.watchingRun)
	}
	if model.thinking {
		t.Fatal("passive watch must NOT capture the composer (thinking must stay false)")
	}
	if model.watchCancel == nil {
		t.Fatal("attach must install a watch-detach cancel")
	}
	if model.watchedRunID != "r9" {
		t.Fatalf("attached run id = %q, want r9", model.watchedRunID)
	}
	if !strings.Contains(model.statusMsg, "Watching Long migration") {
		t.Fatalf("status line should name the watched task: %q", model.statusMsg)
	}
	if !strings.Contains(model.messages[0].Content, "Long migration") {
		t.Fatalf("digest missing the running-task line:\n%s", model.messages[0].Content)
	}

	// The watcher runs; execute the returned command and confirm it starts,
	// then detach.
	go func() { _ = cmd() }()
	select {
	case <-watcherStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("run watcher was not started")
	}
	if watchedRunID != "r9" {
		t.Fatalf("watcher received run id %q, want r9", watchedRunID)
	}
	model.watchCancel()

	updated, _ := model.updateInner(MsgAttachedRunDone{RunID: "r9", Cancelled: true})
	model = updated.(*uiModel)
	if model.watchingRun {
		t.Fatal("detach must clear watch state")
	}
}

func TestAttachedRunDoneReportsOutcome(t *testing.T) {
	c := NewController(nil, nil, nil, "")
	model := c.model
	model.watchingRun = true
	model.watchedRunID = "r9"

	updated, _ := model.updateInner(MsgAttachedRunDone{RunID: "r9", Summary: "migration complete"})
	model = updated.(*uiModel)
	if model.watchingRun {
		t.Fatalf("state after watched run end: watchingRun=%v", model.watchingRun)
	}
	last := model.messages[len(model.messages)-1]
	if last.Role != "notice" || !strings.Contains(last.Content, "The running task finished: migration complete") {
		t.Fatalf("outcome not reported: %+v", last)
	}
}

func TestStaleAttachedRunDoneDoesNotStopCurrentWatcher(t *testing.T) {
	c := NewController(nil, nil, nil, "")
	model := c.model
	model.watchingRun = true
	model.watchedRunID = "r2"

	updated, _ := model.updateInner(MsgAttachedRunDone{RunID: "r1", Summary: "old run done"})
	model = updated.(*uiModel)

	if !model.watchingRun || model.watchedRunID != "r2" {
		t.Fatalf("stale completion changed current watcher: watching=%v run=%q", model.watchingRun, model.watchedRunID)
	}
	if len(model.messages) != 0 {
		t.Fatalf("stale completion should not render: %+v", model.messages)
	}
}

func TestFormatElapsedShort(t *testing.T) {
	cases := map[int64]string{45: "45s", 720: "12m", 3900: "1h05m", -3: "0s"}
	for in, want := range cases {
		if got := formatElapsedShort(in); got != want {
			t.Fatalf("formatElapsedShort(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestStartupDigestRendersActiveRunProgress: when the digest's active run
// carries plan lines and a latest-activity note (server-bounded), the startup
// block renders them under the "running now" line so re-attaching shows where
// the task stands; without them the line stays as before.
func TestStartupDigestRendersActiveRunProgress(t *testing.T) {
	text := formatStartupDigest(&api.DigestResponse{
		ActiveRun: &api.DigestActiveRun{
			TaskID:         "t9",
			Title:          "Long migration",
			ElapsedSeconds: 720,
			PlanSteps: []string{
				"✓ Dump the schema",
				"→ Rewrite the migrations",
				"○ Replay onto staging",
				"… 4 more steps",
			},
			LatestActivity: "running terminal",
		},
	})
	for _, want := range []string{
		"▶ A task is running now: Long migration (12m)",
		"    ✓ Dump the schema",
		"    → Rewrite the migrations",
		"    ○ Replay onto staging",
		"    … 4 more steps",
		"    now: running terminal",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("digest missing %q:\n%s", want, text)
		}
	}

	// No plan/activity: no indented progress lines appear.
	bare := formatStartupDigest(&api.DigestResponse{
		ActiveRun: &api.DigestActiveRun{TaskID: "t9", Title: "Long migration", ElapsedSeconds: 720},
	})
	if strings.Contains(bare, "\n    ") {
		t.Fatalf("bare active run must not render progress lines:\n%s", bare)
	}

	// The empty-digest contract is untouched.
	if got := formatStartupDigest(&api.DigestResponse{}); got != "" {
		t.Fatalf("empty digest must render nothing, got %q", got)
	}
}
