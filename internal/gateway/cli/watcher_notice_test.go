package cli

import (
	"strings"
	"testing"
)

func startNoticeFinalizer(m *uiModel) {
	m.Update(MsgWatcherCompleted{WatchID: "watch_old", Status: "succeeded", Event: uiEventRef{Source: eventSourceDaemon, RunID: "run_origin", Cursor: 10}})
	m.Update(MsgDaemonRunStarted{RunID: "run_finalizer", WatchID: "watch_old", Event: uiEventRef{Source: eventSourceDaemon, RunID: "run_finalizer", Cursor: 11}})
}

func finishNoticeFinalizer(m *uiModel) {
	m.Update(MsgDaemonRunFinished{RunID: "run_finalizer", Status: "done", Summary: "Previous release recorded", Event: uiEventRef{Source: eventSourceDaemon, RunID: "run_finalizer", Cursor: 20}})
}

func TestWatcherNoticeRejectsObsoleteObservationAfterNewTask(t *testing.T) {
	m := NewController("", "", nil, "").model
	startNoticeFinalizer(m)
	finishNoticeFinalizer(m)
	m.addMessage("user", "new release")
	m.Update(MsgDaemonRunStarted{RunID: "run_new", Input: "new release", Event: uiEventRef{Source: eventSourceDaemon, RunID: "run_new", Cursor: 30}})
	// A different event identity bypasses ordinary event-id deduplication.
	m.Update(MsgWatcherCompleted{WatchID: "watch_old", Status: "succeeded", Event: uiEventRef{Source: eventSourceDaemon, EventID: "late_observation", Cursor: 10}})
	if m.statusMsg != "" {
		t.Fatalf("obsolete watcher resurfaced under new work: %q", m.statusMsg)
	}
	// A revised verdict for the same watch is fresh evidence, not a replay.
	m.Update(MsgWatcherCompleted{WatchID: "watch_old", Status: "failed", Event: uiEventRef{Source: eventSourceDaemon, Cursor: 40}})
	if !strings.Contains(m.statusMsg, "failed") {
		t.Fatalf("fresh verdict was lost: %q", m.statusMsg)
	}
}

func TestWatcherFinalizationPreservesNewerNotice(t *testing.T) {
	for _, replacement := range []string{"other_watch", "other_notice", "revised_verdict"} {
		t.Run(replacement, func(t *testing.T) {
			m := NewController("", "", nil, "").model
			startNoticeFinalizer(m)
			if replacement == "other_watch" {
				m.Update(MsgWatcherCompleted{WatchID: "watch_new", Status: "succeeded", Event: uiEventRef{Source: eventSourceDaemon, Cursor: 15}})
			} else if replacement == "revised_verdict" {
				m.Update(MsgWatcherCompleted{WatchID: "watch_old", Status: "failed", Event: uiEventRef{Source: eventSourceDaemon, Cursor: 40}})
			} else {
				// Even identical display text is not the old notice's ownership.
				m.setStatusNotice(noticeWarning, m.statusMsg)
			}
			before := m.statusMsg
			finishNoticeFinalizer(m)
			if m.statusMsg != before {
				t.Fatalf("unrelated notice was erased: before=%q after=%q", before, m.statusMsg)
			}
		})
	}
}

func TestLateWatcherFinalizationDoesNotChangeForegroundRun(t *testing.T) {
	m := NewController("", "", nil, "").model
	startNoticeFinalizer(m)
	m.Update(MsgDaemonRunStarted{RunID: "run_new", Input: "new release", Event: uiEventRef{Source: eventSourceDaemon, Cursor: 30}})
	finishNoticeFinalizer(m)
	if m.statusMsg != "" || !m.daemonRunActive || m.daemonRunID != "run_new" || m.runStatus != "working" {
		t.Fatalf("late result left stale notice or changed foreground: notice=%q active=%v run=%s status=%s", m.statusMsg, m.daemonRunActive, m.daemonRunID, m.runStatus)
	}
	if len(m.messages) != 1 || !strings.Contains(m.messages[0].Content, "Previous release recorded") {
		t.Fatalf("late final result was lost: %+v", m.messages)
	}
}
