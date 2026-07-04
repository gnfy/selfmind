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
	if !model.thinking || !model.attachedRun || model.runStatus != "working" {
		t.Fatalf("attach must flip run-active state: thinking=%v attached=%v status=%q", model.thinking, model.attachedRun, model.runStatus)
	}
	if model.cancelFn == nil {
		t.Fatal("attach must install a local detach cancelFn (exit prompt path)")
	}
	if !strings.Contains(model.messages[0].Content, "▶ A task is running now: Long migration (12m) — attaching to its live events…") {
		t.Fatalf("digest missing the attach line:\n%s", model.messages[0].Content)
	}

	// The steering path is usable exactly like an in-turn run: Enter-typed
	// input goes to the daemon steer function, not the local channel.
	var steered string
	model.steerFn = func(text string) error {
		steered = text
		return nil
	}
	_ = model.injectMidRunGuidance("prioritize the failing shard")
	if steered != "prioritize the failing shard" {
		t.Fatalf("steering while attached saw %q", steered)
	}

	// Execute the batch's sub-commands (watcher + ticks) like the Bubble Tea
	// runtime would, to confirm the watcher actually starts, then cancel the
	// local watch (detach).
	go func() {
		if batch, ok := cmd().(tea.BatchMsg); ok {
			for _, sub := range batch {
				if sub != nil {
					go sub()
				}
			}
		}
	}()
	select {
	case <-watcherStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("run watcher was not started")
	}
	model.cancelFn()

	updated, _ := model.updateInner(MsgAttachedRunDone{Cancelled: true})
	model = updated.(*uiModel)
	if model.thinking || model.attachedRun {
		t.Fatal("attached-run end must clear the run-active state")
	}
}

func TestAttachedRunDoneReportsOutcome(t *testing.T) {
	c := NewController(nil, nil, nil, "")
	model := c.model
	model.attachedRun = true
	model.thinking = true
	model.runStatus = "working"

	updated, _ := model.updateInner(MsgAttachedRunDone{Summary: "migration complete"})
	model = updated.(*uiModel)
	if model.thinking || model.attachedRun || model.runStatus != "done" {
		t.Fatalf("state after attached run end: thinking=%v attached=%v status=%q", model.thinking, model.attachedRun, model.runStatus)
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
