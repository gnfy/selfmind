package cli

import (
	"encoding/json"
	"strings"
	"time"

	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/textutil"
)

// backgroundSummaryMaxBytes bounds the outcome summary carried by a background
// run's result notice: it is a status line, not a transcript.
const backgroundSummaryMaxBytes = 800

func eventRefFromRunEvent(event api.RunEvent) uiEventRef {
	return uiEventRef{
		Source:  eventSourceDaemon,
		RunID:   strings.TrimSpace(event.RunID),
		EventID: strings.TrimSpace(event.EventID),
		Cursor:  event.Cursor,
		LiveSeq: event.LiveSeq,
	}
}

func (m *uiModel) forwardDaemonRunEvent(event api.RunEvent) {
	if m == nil || m.program == nil {
		return
	}
	switch event.Type {
	case "background.notice":
		var payload struct {
			Message string `json:"message"`
			Kind    string `json:"kind"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		if message := strings.TrimSpace(payload.Message); message != "" {
			m.program.Send(MsgBackgroundNotice{Content: message, Success: payload.Kind == "success"})
		}
	case "run.started":
		var payload struct {
			Input      string `json:"input"`
			QueueID    string `json:"queue_id"`
			WatchID    string `json:"watch_id"`
			TaskStatus string `json:"task_status"`
			Origin     string `json:"origin"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		started := event.CreatedAt
		if started.IsZero() {
			started = time.Now()
		}
		m.program.Send(MsgDaemonRunStarted{
			RunID:      strings.TrimSpace(event.RunID),
			QueueID:    strings.TrimSpace(payload.QueueID),
			TaskID:     strings.TrimSpace(event.TaskID),
			WatchID:    strings.TrimSpace(payload.WatchID),
			TaskStatus: strings.TrimSpace(payload.TaskStatus),
			Origin:     strings.TrimSpace(payload.Origin),
			Input:      strings.TrimSpace(payload.Input),
			Started:    started,
			Event:      eventRefFromRunEvent(event),
		})
	case "run.finished", "run.cancelled", "run.interrupted", "run.failed":
		var payload struct {
			Outcome api.RunOutcome `json:"outcome"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		status := strings.TrimSpace(payload.Outcome.Status)
		if status == "" {
			switch event.Type {
			case "run.finished":
				status = "done"
			case "run.cancelled":
				status = "cancelled"
			default:
				status = "error"
			}
		}
		m.program.Send(MsgDaemonRunFinished{
			RunID:   strings.TrimSpace(event.RunID),
			Status:  status,
			Summary: strings.TrimSpace(payload.Outcome.Summary),
			Event:   eventRefFromRunEvent(event),
		})
	}
}

func sameQueuedInput(full, eventInput string) bool {
	full = strings.TrimSpace(full)
	eventInput = strings.TrimSpace(eventInput)
	if full == "" || eventInput == "" {
		return false
	}
	return full == eventInput || strings.HasPrefix(full, eventInput) || strings.HasPrefix(eventInput, full)
}

func (m *uiModel) consumeQueuedInput(eventInput string) bool {
	for i, input := range m.queuedInputs {
		if !sameQueuedInput(input, eventInput) {
			continue
		}
		m.queuedInputs = append(m.queuedInputs[:i], m.queuedInputs[i+1:]...)
		if m.queuedCount > 0 {
			m.queuedCount--
		}
		return true
	}
	return false
}

func (m *uiModel) rememberQueuedRun(queueID string) bool {
	queueID = strings.TrimSpace(queueID)
	if queueID == "" {
		return false
	}
	if m.daemonRunActive && m.daemonRunQueueID == queueID {
		m.daemonRunOwned = true
		return false
	}
	for _, existing := range m.queuedRunIDs {
		if existing == queueID {
			return false
		}
	}
	m.queuedRunIDs = append(m.queuedRunIDs, queueID)
	m.queuedCount++
	return true
}

func (m *uiModel) consumeQueuedRun(queueID string) bool {
	queueID = strings.TrimSpace(queueID)
	if queueID == "" {
		return false
	}
	for i, existing := range m.queuedRunIDs {
		if existing != queueID {
			continue
		}
		m.queuedRunIDs = append(m.queuedRunIDs[:i], m.queuedRunIDs[i+1:]...)
		if m.queuedCount > 0 {
			m.queuedCount--
		}
		return true
	}
	return false
}

func uiStatusForDaemonOutcome(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed", "succeeded", "success":
		return "done"
	case "cancelled", "canceled":
		return "cancelled"
	case "running", "waiting_user", "waiting_external", "verification_partial", "interrupted":
		return "done"
	default:
		return "error"
	}
}

// watcherStatusNotice renders a watcher's OWN terminal observation. It is the
// transient half of a watcher's report: the finalization run that follows leaves
// the single durable transcript line (backgroundResultNotice), so this text goes
// to the status bar and never becomes a second history cell.
func watcherStatusNotice(watchID, status, taskStatus string) string {
	watchID = strings.TrimSpace(watchID)
	taskStatus = strings.TrimSpace(taskStatus)
	if taskStatus == "" {
		taskStatus = "waiting_finalization"
	}
	watchStatus := "succeeded"
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed":
		watchStatus = "failed"
	case "timed_out", "timeout":
		watchStatus = "timed_out"
	case "cancelled", "canceled":
		watchStatus = "cancelled"
	case "blocked_environment":
		// A check that could not observe the external state at all. The daemon
		// notice carries this status; rendering it as the default "succeeded"
		// would report a blocked watcher as a completed operation.
		watchStatus = "blocked_environment"
	}
	return "Watcher " + watchID + " | status: " + watchStatus + " | task: " + taskStatus
}

// watcherNoticeKind types a watcher's terminal observation for the status bar.
// Only an observed success is neutral; anything else needs the person's
// attention, and a blocked check is not a business failure.
func watcherNoticeKind(status string) noticeKind {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed":
		return noticeError
	case "timed_out", "timeout", "cancelled", "canceled", "blocked_environment":
		return noticeWarning
	default:
		return noticeInfo
	}
}

// backgroundResultNotice is the single line a background run is allowed to
// leave behind: what ran, the run's recorded task status, and its outcome
// summary. The status is the raw run outcome, not the collapsed UI status —
// "waiting_user" is exactly what a blocked watcher check produces and must
// stay distinguishable from "done". A watcher keeps its durable id as the
// label so the line joins its earlier notices; any other origin names itself.
func backgroundResultNotice(watchID, origin, taskStatus, summary string) string {
	label := "Background run (" + firstNonEmptyText(origin, "background") + ")"
	if watchID = strings.TrimSpace(watchID); watchID != "" {
		label = "Watcher " + watchID + " | status: finalized"
	}
	notice := label + " | task: " + firstNonEmptyText(taskStatus, "done")
	if summary = strings.TrimSpace(textutil.Truncate(summary, backgroundSummaryMaxBytes)); summary != "" {
		notice += "\n" + summary
	}
	return notice
}

func firstNonEmptyText(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

// markBackgroundRun records a daemon run the person did not type at any
// endpoint: the daemon started it on their behalf, and `run.started` names the
// initiator (a watcher finalization, a cron fire, any future one).
//
// Moving work off the agent turn is pointless if the terminal then replays the
// whole background transcript, so its progress is deliberately dropped: at
// most a start notice and one result line are rendered, and `/diag execution`
// keeps the detail. Approvals, clarifications, watcher notices, and the run
// lifecycle itself are NOT filtered — a background run that needs a human must
// still be able to reach one, and the person must still see why their next
// message queues.
//
// A turn the person typed at another endpoint is NOT background work: they are
// doing it right now, just elsewhere, so it is left alone here.
func (m *uiModel) markBackgroundRun(runID, watchID, origin string) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return
	}
	m.backgroundRunID = runID
	m.backgroundWatchID = strings.TrimSpace(watchID)
	m.backgroundOrigin = strings.TrimSpace(origin)
	m.backgroundResultPending = true
}

func (m *uiModel) backgroundDaemonRunActive() bool {
	return m.daemonRunActive && m.daemonRunID != "" && m.daemonRunID == m.backgroundRunID
}

// backgroundRunEvent reports whether this event belongs to the background run.
// The run id is kept after that run ends and is replaced only by the next
// background run, so a trailing event (a late tool.completed, a buffered
// delta) cannot land under whatever the person does next.
func (m *uiModel) backgroundRunEvent(ref uiEventRef) bool {
	return m.backgroundRunID != "" && ref.RunID != "" && ref.RunID == m.backgroundRunID
}

// finishedBackgroundRun consumes the result line this background run is owed,
// returning its watcher id (empty for other origins) and its origin. A second
// call for the same run returns false, so a replayed finish cannot print the
// outcome twice.
func (m *uiModel) finishedBackgroundRun(runID string) (watchID, origin string, ok bool) {
	if !m.backgroundResultPending || m.backgroundRunID == "" {
		return "", "", false
	}
	if strings.TrimSpace(runID) != m.backgroundRunID {
		return "", "", false
	}
	m.backgroundResultPending = false
	return m.backgroundWatchID, m.backgroundOrigin, true
}

// passiveDaemonEvent reports whether a daemon-feed event belongs to work this
// terminal merely observes. A run the person started here — synchronously or
// through the durable queue — is owned: its activity drives the spinner and
// tool cells exactly like a local turn.
func (m *uiModel) passiveDaemonEvent(ref uiEventRef) bool {
	if ref.Source != eventSourceDaemon {
		return false
	}
	if m.daemonRunID != "" && ref.RunID != "" && ref.RunID != m.daemonRunID {
		return true
	}
	return !m.daemonRunOwned
}

func queuedTurn(turn *api.TurnStatus) bool {
	return turn != nil && strings.EqualFold(strings.TrimSpace(turn.Status), "queued")
}

// acceptedTurn is the daemon's acknowledgement that a message was steered into
// the live run (busy path). It is not that run's terminal answer, so the UI
// must keep the run's live state instead of finalizing it.
func acceptedTurn(turn *api.TurnStatus) bool {
	return turn != nil && strings.EqualFold(strings.TrimSpace(turn.Status), "accepted")
}
