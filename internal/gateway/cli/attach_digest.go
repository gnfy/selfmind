package cli

// Attach digest + mid-flight re-attach (client mode, G0-c/G0-d). Reopening
// the TUI after being away renders one compact "While you were away" block
// (fed by GET /v1/digest via the client shell) and, when the digest reports a
// run still executing in the daemon, hooks that run's live events and steering
// WITHOUT starting a user turn — the run stays daemon-owned, this terminal is
// only a watcher (docs/identity-continuity.md "Runtime attachment model").

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/llm"
)

// RunWatcher follows the person's mid-flight daemon run: it streams live
// events into the observer until the run ends and returns the recorded
// outcome summary (empty when none). Client mode wires it to
// client.WatchActiveRun; it must honor ctx cancellation (local detach).
type RunWatcher func(ctx context.Context, observer httpapi.StreamObserver) string

// MsgAttachedRunDone is emitted when a watched (re-attached) run ends.
// Cancelled marks a local cancel (the user chose c on the exit prompt), which
// cancelActiveRunLocally has already reported.
type MsgAttachedRunDone struct {
	Summary   string
	Cancelled bool
}

// SetStartupDigest hands the controller the attach digest fetched by the
// client shell. Must be called before Start(); the first sized frame renders
// it once. A nil or empty digest renders nothing — a fresh session stays clean.
func (c *Controller) SetStartupDigest(digest *api.DigestResponse) {
	if c == nil || c.model == nil {
		return
	}
	c.model.startupDigest = digest
}

// SetRunWatcher installs the client-mode follower for a mid-flight daemon run
// (re-attach). Only set in client mode; without it an active run in the digest
// is reported but not watched.
func (c *Controller) SetRunWatcher(fn RunWatcher) {
	if c == nil || c.model == nil {
		return
	}
	c.model.runWatcher = fn
}

// maybeShowStartupDigest renders the attach digest exactly once, as soon as a
// real width is known, and starts the run watcher when the digest reports a
// mid-flight run. Returns nil when there is nothing to do.
func (m *uiModel) maybeShowStartupDigest(width int) tea.Cmd {
	if m.startupDigest == nil || m.digestShown || width <= 0 {
		return nil
	}
	m.digestShown = true
	text := formatStartupDigest(m.startupDigest)
	if text == "" {
		return nil
	}
	m.addMessage("system", text)
	if m.startupDigest.ActiveRun != nil && m.clientMode && m.runWatcher != nil {
		return m.attachToActiveRun()
	}
	return nil
}

// attachToActiveRun flips the composer into the same "run active" state a
// local turn uses — spinner, Enter-steers-the-run, ctrl+c exit prompt all
// reuse the existing paths keyed off m.thinking — and starts the watcher in
// the background. m.cancelFn cancels only the local watch (detach); actually
// stopping the run goes through requestDaemonStop like every client-mode
// cancel.
func (m *uiModel) attachToActiveRun() tea.Cmd {
	m.attachedRun = true
	m.thinking = true
	m.runStatus = "working"
	m.thinkingStart = time.Now()
	m.thinkingDots = 0
	m.runTokens = 0
	m.activityText = "Watching the running task"
	ctx, cancel := context.WithCancel(context.Background())
	m.cancelFn = cancel
	watcher := m.runWatcher
	return tea.Batch(func() tea.Msg {
		summary := watcher(ctx, func(event llm.StreamEvent) {
			if event.EventType != "" {
				m.forwardGatewayEvent(event)
			}
		})
		return MsgAttachedRunDone{Summary: summary, Cancelled: ctx.Err() != nil}
	}, m.spinner.Tick, workingTick())
}

// formatStartupDigest renders the digest as one compact conversational block.
// Ids are deliberately absent: approvals resolve by ordinal via /approvals on
// the gateway, tasks resume via /resume — UUID hashes carry no meaning here.
func formatStartupDigest(digest *api.DigestResponse) string {
	if digest.Empty() {
		return ""
	}
	lines := []string{"While you were away:"}
	if n := len(digest.FinishedTasks); n > 0 {
		lines = append(lines, fmt.Sprintf("✔ %s finished: %s", countNoun(n, "task"), digestTitleList(digest.FinishedTasks)))
	}
	if n := len(digest.DisruptedTasks); n > 0 {
		lines = append(lines, fmt.Sprintf("✖ %s stopped early: %s (use /resume to continue)", countNoun(n, "task"), digestTitleList(digest.DisruptedTasks)))
	}
	switch n := len(digest.PendingApprovals); {
	case n == 1:
		lines = append(lines, fmt.Sprintf("⚠ 1 approval waiting: %s — reply y or n (see /approvals)", digest.PendingApprovals[0].Line))
	case n > 1:
		lines = append(lines, fmt.Sprintf("⚠ %d approvals waiting — see /approvals", n))
	}
	if n := len(digest.UnconfirmedPushes); n > 0 {
		lines = append(lines, fmt.Sprintf("⚠ %s may not have reached %s (see /status)", countNoun(n, "push"), digestPushTargets(digest.UnconfirmedPushes)))
	}
	if active := digest.ActiveRun; active != nil {
		title := strings.TrimSpace(active.Title)
		if title == "" {
			title = "untitled task"
		}
		lines = append(lines, fmt.Sprintf("▶ A task is running now: %s (%s) — attaching to its live events…", title, formatElapsedShort(active.ElapsedSeconds)))
	}
	return strings.Join(lines, "\n")
}

// digestTitleList joins up to three task titles; the rest collapse into a
// count so the digest never turns into a scrolling report.
func digestTitleList(tasks []api.DigestTask) string {
	titles := make([]string, 0, 3)
	for _, task := range tasks {
		title := strings.TrimSpace(task.Title)
		if title == "" {
			title = "untitled task"
		}
		titles = append(titles, title)
		if len(titles) == 3 {
			break
		}
	}
	out := strings.Join(titles, ", ")
	if rest := len(tasks) - len(titles); rest > 0 {
		out += fmt.Sprintf(", and %d more", rest)
	}
	return out
}

func digestPushTargets(pushes []api.DigestPush) string {
	seen := map[string]bool{}
	var platforms []string
	for _, push := range pushes {
		platform := strings.TrimSpace(push.Platform)
		if platform == "" || seen[platform] {
			continue
		}
		seen[platform] = true
		platforms = append(platforms, platform)
	}
	if len(platforms) == 0 {
		return "its destination"
	}
	return strings.Join(platforms, ", ")
}

func countNoun(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	if noun == "push" {
		return fmt.Sprintf("%d pushes", n)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func formatElapsedShort(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
