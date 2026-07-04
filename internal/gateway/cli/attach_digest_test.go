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
			{ID: "apr_1", Line: "[terminal] command=rm -rf build — destructive command"},
		},
		UnconfirmedPushes: []api.DigestPush{{Platform: "weixin", Status: "sent_unconfirmed", Preview: "build finished"}},
	})
	model := c.model

	cmd := model.maybeShowStartupDigest(100)
	if cmd != nil {
		t.Fatal("no active run: digest must not start a watcher")
	}
	if len(model.messages) != 1 || model.messages[0].Role != "system" {
		t.Fatalf("digest should render one system message: %+v", model.messages)
	}
	text := model.messages[0].Content
	for _, want := range []string{
		"While you were away:",
		"✔ 2 tasks finished: Ship the report, Fix the build",
		"✖ 1 task stopped early: Refactor parser (use /resume to continue)",
		"⚠ 1 approval waiting: [terminal] command=rm -rf build — destructive command — reply y or n (see /approvals)",
		"⚠ 1 push may not have reached weixin (see /status)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("digest missing %q:\n%s", want, text)
		}
	}
	if model.thinking {
		t.Fatal("digest without an active run must leave the composer idle")
	}

	// Rendered once: a second sized frame must not repeat it.
	if cmd := model.maybeShowStartupDigest(120); cmd != nil || len(model.messages) != 1 {
		t.Fatalf("digest must render exactly once: %d messages", len(model.messages))
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
	c.SetRunWatcher(func(ctx context.Context, observer httpapi.StreamObserver) string {
		close(watcherStarted)
		<-ctx.Done()
		return ""
	})
	c.SetStartupDigest(&api.DigestResponse{
		ActiveRun: &api.DigestActiveRun{TaskID: "t9", Title: "Long migration", ElapsedSeconds: 720},
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
	model.watchCancel()

	updated, _ := model.updateInner(MsgAttachedRunDone{Cancelled: true})
	model = updated.(*uiModel)
	if model.watchingRun {
		t.Fatal("detach must clear watch state")
	}
}

func TestAttachedRunDoneReportsOutcome(t *testing.T) {
	c := NewController(nil, nil, nil, "")
	model := c.model
	model.watchingRun = true

	updated, _ := model.updateInner(MsgAttachedRunDone{Summary: "migration complete"})
	model = updated.(*uiModel)
	if model.watchingRun {
		t.Fatalf("state after watched run end: watchingRun=%v", model.watchingRun)
	}
	last := model.messages[len(model.messages)-1]
	if last.Role != "system" || !strings.Contains(last.Content, "The running task finished: migration complete") {
		t.Fatalf("outcome not reported: %+v", last)
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
