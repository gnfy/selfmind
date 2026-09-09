package cli

import "strings"

// Watcher notices have their own lifecycle: observation and finalization belong
// to different Runs. Display prose and the current foreground Run cannot supply
// their ownership. Retain bounded terminal cursors even after clearing the bar.
func (m *uiModel) showWatcherNotice(msg MsgWatcherCompleted) {
	watchID := strings.TrimSpace(msg.WatchID)
	if watchID == "" {
		return
	}
	if cursor, finalized := m.finalizedWatchNotices[watchID]; finalized && msg.Event.Cursor <= cursor {
		return
	}
	m.setStatusNotice(watcherNoticeKind(msg.Status), watcherStatusNotice(watchID, msg.Status, msg.TaskStatus))
	m.watcherNoticeID = watchID
	m.watcherNoticeCursor = msg.Event.Cursor
}

func (m *uiModel) finishWatcherNotice(watchID string, cursor int64) {
	watchID = strings.TrimSpace(watchID)
	if watchID == "" {
		return
	}
	if m.finalizedWatchNotices == nil {
		m.finalizedWatchNotices = make(map[string]int64)
	}
	previous, exists := m.finalizedWatchNotices[watchID]
	if !exists {
		const maxFinalizedWatchNotices = 128
		if len(m.finalizedWatchOrder) == maxFinalizedWatchNotices {
			delete(m.finalizedWatchNotices, m.finalizedWatchOrder[0])
			m.finalizedWatchOrder = m.finalizedWatchOrder[1:]
		}
		m.finalizedWatchOrder = append(m.finalizedWatchOrder, watchID)
	}
	m.finalizedWatchNotices[watchID] = max(previous, cursor)
	// A newer notice (another watch, approval feedback, or a revised verdict)
	// must survive a delayed completion from this finalization Run.
	if m.watcherNoticeID == watchID && m.watcherNoticeCursor <= cursor {
		m.clearStatusNotice()
	}
}
