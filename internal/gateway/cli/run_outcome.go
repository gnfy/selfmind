package cli

import (
	"strings"

	"selfmind/internal/gateway/api"
)

// replyOutsideLiveRun separates request completion from the exact execution
// being presented. A control acknowledgement has no Run; it cannot close one.
func (m *uiModel) replyOutsideLiveRun(msg MsgAgentDone) bool {
	if queuedTurn(msg.Turn) || acceptedTurn(msg.Turn) {
		return false
	}
	id := ""
	if msg.Turn != nil {
		id = strings.TrimSpace(msg.Turn.RunID)
	}
	if m.daemonRunActive {
		return id == ""
	}
	if m.watchingRun {
		return id == "" || id != m.watchedRunID
	}
	return id == "" && strings.HasPrefix(strings.TrimSpace(msg.Input), "/")
}

func replyOutcomeStatus(turn *api.TurnStatus) string {
	if turn == nil {
		// Compatibility for an older daemon's final answer. New daemon replies
		// carry the Run's outcome; control replies never reach this fallback.
		return "done"
	}
	if turn.RunID != "" && turn.TaskStatus != "" {
		return uiStatusForDaemonOutcome(turn.TaskStatus)
	}
	return uiStatusForDaemonOutcome(turn.Status)
}
