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
// client.WatchRun; it must honor ctx cancellation (local detach).
type RunWatcher func(ctx context.Context, runID string, observer httpapi.StreamObserver) string

// MsgAttachedRunDone is emitted when a watched (re-attached) run ends.
// Cancelled marks a local cancel (the user chose c on the exit prompt), which
// cancelActiveRunLocally has already reported.
type MsgAttachedRunDone struct {
	RunID     string
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
	m.addMessage("digest", text)
	if m.startupDigest.ActiveRun != nil && m.clientMode && m.runWatcher != nil {
		m.watchedRunID = strings.TrimSpace(m.startupDigest.ActiveRun.RunID)
		m.watchedTaskTitle = strings.TrimSpace(m.startupDigest.ActiveRun.Title)
		return m.attachToActiveRun()
	}
	return nil
}

// attachToActiveRun starts PASSIVE observation of the person's mid-flight
// daemon run: live events stream into the transcript, but the composer is NOT
// captured. This is a spectator view, not a local turn — the user opened the
// CLI and simply sees what is already running; they remain free to type a new
// task (the daemon queues it behind the active run per G1+G2) without it being
// misread as steering, and ctrl+c detaches the watch rather than cancelling
// someone else's task. Making this a full "run active" state (m.thinking) was
// wrong: it hijacked startup into steering-mode and, with baseline event
// suppression, showed a silent spinner that read as "stuck".
func (m *uiModel) attachToActiveRun() tea.Cmd {
	m.watchingRun = true
	name := strings.TrimSpace(m.watchedTaskTitle)
	if name == "" {
		name = "the running task"
	}
	m.statusMsg = "Watching " + name + " — live progress below; type a new task to queue it, or /stop to cancel."
	ctx, cancel := context.WithCancel(context.Background())
	m.watchCancel = cancel
	watcher := m.runWatcher
	runID := m.watchedRunID
	return func() tea.Msg {
		summary := watcher(ctx, runID, func(event llm.StreamEvent) {
			if event.EventType != "" {
				m.forwardGatewayEventFrom(event, eventSourceWatch)
			}
		})
		return MsgAttachedRunDone{RunID: runID, Summary: summary, Cancelled: ctx.Err() != nil}
	}
}

// detachWatchedRunForNewTurn closes the passive view before a new user cell is
// committed. The daemon-owned run keeps executing; only this terminal's live
// projection is detached. This makes transcript order deterministic and lets
// acceptEvent reject any watcher messages already queued in Bubble Tea.
func (m *uiModel) detachWatchedRunForNewTurn() {
	if !m.watchingRun {
		return
	}
	m.finalizeLiveStream("")
	m.watchingRun = false
	m.watchedRunID = ""
	m.watchedTaskTitle = ""
	if m.watchCancel != nil {
		m.watchCancel()
	}
	m.watchCancel = nil
	m.toolExecuting = ""
	m.activePlanJSON = ""
	m.discardOpenToolMessages()
	if strings.HasPrefix(m.statusMsg, "Watching ") {
		m.statusMsg = ""
	}
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
	switch n := len(digest.PendingClarifies); {
	case n == 1:
		lines = append(lines, fmt.Sprintf("⚠ 1 question waiting: %s — just reply with your answer", digest.PendingClarifies[0].Line))
	case n > 1:
		lines = append(lines, fmt.Sprintf("⚠ %d questions waiting — see /status", n))
	}
	if n := len(digest.UnconfirmedPushes); n > 0 {
		lines = append(lines, fmt.Sprintf("⚠ %s may not have reached %s (see /status)", countNoun(n, "push"), digestPushTargets(digest.UnconfirmedPushes)))
	}
	if active := digest.ActiveRun; active != nil {
		title := strings.TrimSpace(active.Title)
		if title == "" {
			title = "untitled task"
		}
		lines = append(lines, fmt.Sprintf("▶ A task is running now: %s (%s) — live progress below; type a new task to queue it, or /stop to cancel.", title, formatElapsedShort(active.ElapsedSeconds)))
		// Current progress: the run's plan checklist (server-bounded lines, the
		// current step marked →) and the latest activity, so re-attaching
		// answers "where is it?" without waiting for the next live event.
		for _, step := range active.PlanSteps {
			lines = append(lines, "    "+step)
		}
		if activity := strings.TrimSpace(active.LatestActivity); activity != "" {
			lines = append(lines, "    now: "+activity)
		}
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
