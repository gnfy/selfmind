package cli

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"selfmind/internal/platform/textutil"
)

// Session audience. Every daemon event names the session it concerns; a run
// belongs to the session that started it. This terminal renders its own
// session's runs in full, a run it explicitly attached to, and only a one-line
// status for work running or waiting in other sessions. The daemon already
// filters the stream the same way; the checks here are the second line.

// otherSessionNoticeTTL bounds a finished other-session run's status line.
const otherSessionNoticeTTL = 6 * time.Second

// otherSession reports whether an event names a session other than this
// terminal's. The daemon marks every run event with its session; an event that
// names none concerns the person as a whole.
func (m *uiModel) otherSession(channel string) bool {
	return otherSessionChannel(channel, m.channel)
}

func otherSessionChannel(channel, session string) bool {
	channel = strings.TrimSpace(channel)
	return channel != "" && channel != session
}

// foreignDetail reports whether a detail event (text, tools, plan, usage)
// belongs to another session's run that this terminal does not observe. The
// drop is counted: the daemon should never have sent it.
func (m *uiModel) foreignDetail(ref uiEventRef) bool {
	if !ref.Detail || !m.otherSession(ref.Channel) {
		return false
	}
	if m.watchingRun && m.watchedRunID != "" && ref.RunID == m.watchedRunID {
		return false
	}
	m.foreignDetailEvents.Add(1)
	return true
}

// TakeForeignSessionEvents returns and resets the number of other-session
// detail events this terminal dropped, for the daemon's isolation metric.
func (c *Controller) TakeForeignSessionEvents() int64 {
	if c == nil || c.model == nil {
		return 0
	}
	return c.model.foreignDetailEvents.Swap(0)
}

// otherSessionRunStarted records that another session started work. It leaves
// this terminal's run state, plan and tool cells alone.
func (m *uiModel) otherSessionRunStarted(msg MsgDaemonRunStarted) {
	m.otherRunID = strings.TrimSpace(msg.RunID)
	m.otherRunTitle = otherSessionTitle(msg.Input)
	if origin := strings.TrimSpace(msg.Origin); origin != "" {
		// The daemon started it, so its input is the daemon's own instruction.
		m.otherRunTitle = "background run (" + origin + ")"
	}
	m.refreshOtherSessionNotice()
}

// otherSessionRunFinished clears another session's finished run and says so
// briefly. It reports the command that expires that line.
func (m *uiModel) otherSessionRunFinished(msg MsgDaemonRunFinished) tea.Cmd {
	if m.otherRunID == "" || strings.TrimSpace(msg.RunID) != m.otherRunID {
		return nil
	}
	title := m.otherRunTitle
	m.otherRunID, m.otherRunTitle = "", ""
	m.otherClarify = ""
	if len(m.otherApprovals) > 0 {
		m.refreshOtherSessionNotice()
		return nil
	}
	if !m.ownsStatusLine() {
		return nil
	}
	status := strings.TrimSpace(msg.Status)
	if status == "" {
		status = "done"
	}
	id := m.setStatusNotice(noticeInfo, "Other session: “"+title+"” ended ("+status+").")
	m.otherNoticeID = id
	return clearStatusNoticeAfter(id, otherSessionNoticeTTL)
}

// otherSessionApproval records an approval another session's run waits on.
// Only the requesting session arms the panel; this terminal says where it is
// and how to answer it from here.
func (m *uiModel) otherSessionApproval(msg MsgApprovalRequest) {
	if m.otherApprovals == nil {
		m.otherApprovals = map[string]string{}
	}
	target := strings.TrimSpace(msg.Tool)
	if t := strings.TrimSpace(msg.Target); t != "" {
		target += " → " + t
	}
	m.otherApprovals[msg.ID] = textutil.Truncate(target, 80)
	m.refreshOtherSessionNotice()
}

// otherSessionApprovalResolved withdraws the notice for an approval answered
// anywhere.
func (m *uiModel) otherSessionApprovalResolved(id string) bool {
	if _, ok := m.otherApprovals[id]; !ok {
		return false
	}
	delete(m.otherApprovals, id)
	m.refreshOtherSessionNotice()
	return true
}

// otherSessionClarify records a question another session's run waits on. Any
// plain reply from the person answers the one pending question, so the notice
// says that a message typed here would answer it.
func (m *uiModel) otherSessionClarify(question string) {
	m.otherClarify = textutil.Truncate(strings.TrimSpace(question), 80)
	m.refreshOtherSessionNotice()
}

// refreshOtherSessionNotice shows the most pressing fact about other sessions
// — a waiting approval, then a waiting question, then running work — in the
// status line, without replacing a notice this terminal set for its own work.
func (m *uiModel) refreshOtherSessionNotice() {
	if !m.ownsStatusLine() {
		return
	}
	var kind noticeKind
	var text string
	switch {
	case len(m.otherApprovals) == 1:
		for _, target := range m.otherApprovals {
			kind, text = noticeWarning, "Other session: approval waiting for "+target+" — /approvals lists it, /approve answers it here."
		}
	case len(m.otherApprovals) > 1:
		kind, text = noticeWarning, "Other session: approvals waiting — /approvals lists them, /approve answers them here."
	case m.otherClarify != "":
		kind, text = noticeWarning, "Other session: question waiting — “"+m.otherClarify+"”. A plain message here answers it."
	case m.otherRunID != "":
		kind, text = noticeInfo, "Other session: working on “"+m.otherRunTitle+"” — /attach to watch it here."
	}
	if text == "" {
		if m.otherNoticeID != 0 && m.statusNoticeID == m.otherNoticeID {
			m.clearStatusNotice()
		}
		m.otherNoticeID = 0
		return
	}
	m.otherNoticeID = m.setStatusNotice(kind, text)
}

// ownsStatusLine reports whether the status line is free or already shows an
// other-session notice, so updating it cannot erase this terminal's own.
func (m *uiModel) ownsStatusLine() bool {
	return m.statusNoticeID == 0 || m.statusNoticeID == m.otherNoticeID
}

func otherSessionTitle(input string) string {
	title := strings.TrimSpace(strings.Join(strings.Fields(input), " "))
	if title == "" {
		return "a task"
	}
	return textutil.Truncate(title, 60)
}

// otherSessionReceipt tells the person their message went to another
// session's running task, and what happens to it there.
func otherSessionReceipt(title string) string {
	title = otherSessionTitle(title)
	return "Sent to “" + title + "”, which is running in another session. It will use your message at its next safe step or queue it as separate work; that task's progress stays in its own session (/attach to watch it here)."
}

// handleAttach watches another session's running task in this terminal. It
// only observes: the run keeps its session, which still receives its answer.
func (m *uiModel) handleAttach(args []string) tea.Cmd {
	runID, title := m.otherRunID, m.otherRunTitle
	if len(args) > 0 {
		if runID = strings.TrimSpace(args[0]); runID != m.otherRunID {
			title = ""
		}
	}
	if runID == "" {
		m.addMessage("assistant", "No task is running in another session. /status shows what is running.")
		return nil
	}
	if !m.clientMode || m.runWatcher == nil {
		m.addMessage("assistant", "Watching another session's task needs the SelfMind daemon.")
		return nil
	}
	if m.watchingRun && m.watchedRunID == runID {
		m.addMessage("assistant", "Already watching that task here.")
		return nil
	}
	m.detachWatchedRunForNewTurn()
	m.watchedRunID = runID
	m.watchedTaskTitle = firstNonEmptyText(title, "the other session's task")
	m.otherNoticeID = 0
	return m.attachToActiveRun()
}
