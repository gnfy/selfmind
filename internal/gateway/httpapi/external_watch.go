package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"selfmind/internal/control"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

const (
	externalWatchSweepInterval = 5 * time.Second
	externalWatchPassLimit     = 20
	externalWatchOutputRunes   = 8000
	// externalWatchExtension is the single bounded deadline grant for a watch
	// whose external operation is demonstrably still producing state at
	// expiry. The store CAS on extensions=0 keeps it to exactly one grant.
	externalWatchExtension = 30 * time.Minute
	// externalWatchRecoveryLookback bounds the startup verdict-recovery scan:
	// only recently timed_out watches are re-examined against their recorded
	// output, so a matcher fix can heal fresh misjudgments without churning
	// ancient history.
	externalWatchRecoveryLookback = 14 * 24 * time.Hour
	// A successful watcher verdict may need an agent run to backfill release
	// records and close the task. Reconciliation may reopen that durable system
	// row only a few times; after that the task becomes visibly blocked instead
	// of looping forever.
	externalWatchFinalizationRetries = 3
)

// StartExternalWatchWorker executes durable external-state checks without
// occupying an agent run. Each watch was approved when registered; this worker
// only repeats that frozen, read-only check until one declared terminal pattern
// matches or its deadline expires.
func (d *Server) StartExternalWatchWorker(ctx context.Context) func() {
	return d.startExternalWatchWorker(ctx, externalWatchSweepInterval)
}

func (d *Server) startExternalWatchWorker(ctx context.Context, interval time.Duration) func() {
	if d == nil || d.Control == nil || interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		d.recoverExternalWatchVerdicts(ctx)
		d.runExternalWatchPass(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				d.runExternalWatchPass(ctx)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

func (d *Server) runExternalWatchPass(ctx context.Context) {
	defer func() {
		d.reconcileExternalWatchFinalizations(ctx)
		d.runExternalWatchNotificationPass(ctx)
	}()
	watches, err := d.Control.ListDueExternalWatches(ctx, externalWatchPassLimit)
	if err != nil {
		log.Warn("external watch scan failed", "error", err)
		return
	}
	for i := range watches {
		watch := watches[i]
		claimed, err := d.Control.ClaimExternalWatch(ctx, watch)
		if err != nil {
			log.Warn("external watch claim failed", "watch_id", watch.ID, "error", err)
			continue
		}
		if !claimed {
			continue
		}
		d.executeExternalWatch(ctx, watch)
	}
}

func (d *Server) executeExternalWatch(ctx context.Context, watch control.ExternalWatch) {
	deadlinePassed := !watch.TimeoutAt.After(time.Now())
	if deadlinePassed {
		// A terminal state recorded on the last checkpoint wins over the
		// deadline: the external operation finished, we just noticed late.
		if status := classifyExternalWatchOutput(watch, watch.LastOutput); status != "" {
			d.completeExternalWatch(ctx, watch, status, watch.LastOutput, "")
			return
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(watch.CommandTimeoutSeconds)*time.Second)
	defer cancel()
	output, commandErr := runExternalWatchCommand(checkCtx, watch.CWD, watch.Command)
	output = truncateExternalWatchOutput(tools.RedactSensitive(output))
	errText := ""
	if commandErr != nil {
		errText = tools.RedactSensitive(commandErr.Error())
	}

	if status := classifyExternalWatchOutput(watch, output); status != "" {
		d.completeExternalWatch(ctx, watch, status, output, statusErrText(status, errText))
		return
	}
	if reason := classifyExternalWatchCommandDefect(output, errText); reason != "" {
		d.completeExternalWatch(ctx, watch, control.ExternalWatchFailed, output, reason)
		return
	}
	if deadlinePassed || !watch.TimeoutAt.After(time.Now()) {
		// The operation is demonstrably still reporting state (the check ran
		// and produced non-terminal output): grant the single bounded
		// extension instead of declaring a timeout that may be a lie.
		if errText == "" && strings.TrimSpace(output) != "" && watch.Extensions == 0 {
			until := time.Now().Add(externalWatchExtension)
			if extended, err := d.Control.ExtendExternalWatchDeadline(ctx, watch.TenantID, watch.ID, until, output); err == nil && extended {
				payload, _ := json.Marshal(map[string]interface{}{
					"watch_id":    watch.ID,
					"description": watch.Description,
					"extended_to": until.Format(time.RFC3339),
					"last_output": truncate(toOneLine(strings.TrimSpace(output)), 240),
				})
				_, _ = d.Control.AppendEvent(ctx, control.Event{
					TaskID:     watch.TaskID,
					RunID:      watch.RunID,
					Type:       "external_watch.extended",
					Visibility: "task",
					Channel:    watch.Channel,
					Payload:    payload,
				})
				return
			}
		}
		d.completeExternalWatch(ctx, watch, control.ExternalWatchTimedOut, output, firstNonEmpty(errText, "watch deadline reached"))
		return
	}
	if err := d.Control.RecordExternalWatchCheck(ctx, watch.TenantID, watch.ID, output, errText); err != nil {
		log.Warn("external watch checkpoint failed", "watch_id", watch.ID, "error", err)
	}
}

func statusErrText(status, errText string) string {
	if status == control.ExternalWatchSucceeded {
		return ""
	}
	return errText
}

func runExternalWatchCommand(ctx context.Context, cwd, command string) (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", command)
	} else {
		cmd = exec.CommandContext(ctx, "bash", "-c", command)
	}
	cmd.Dir = cwd
	bytes, err := cmd.CombinedOutput()
	return string(bytes), err
}

// classifyExternalWatchCommandDefect identifies deterministic failures in the
// frozen check itself. Retrying these cannot reveal a new external state, so a
// watch should fail immediately instead of reporting a misleading timeout.
// Ordinary non-zero exits remain retryable because many status CLIs use them
// while an external operation is still converging.
func classifyExternalWatchCommandDefect(output, errText string) string {
	combined := strings.ToLower(strings.TrimSpace(output + "\n" + errText))
	if combined == "" {
		return ""
	}
	markers := []string{
		"illegal option",
		"invalid option",
		"syntax error",
		"unexpected token",
		"command not found",
		"no such file or directory",
		"permission denied",
		"cannot execute",
		"bad substitution",
	}
	for _, marker := range markers {
		if strings.Contains(combined, marker) {
			detail := strings.TrimSpace(output)
			if detail == "" {
				detail = strings.TrimSpace(errText)
			}
			return "watch check is invalid: " + truncate(toOneLine(detail), 240)
		}
	}
	return ""
}

// matchesExternalWatchPattern normalizes command output before matching so
// anchored patterns like ^SUCCESS$ still hit real CLI output, which almost
// always carries a trailing newline (gcloud, aws, kubectl). Matching runs
// against the trimmed whole output and then each trimmed line.
func matchesExternalWatchPattern(pattern, output string) bool {
	if strings.TrimSpace(pattern) == "" {
		return false
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	normalized := strings.TrimSpace(strings.ReplaceAll(output, "\r\n", "\n"))
	if re.MatchString(normalized) {
		return true
	}
	for _, line := range strings.Split(normalized, "\n") {
		if line = strings.TrimSpace(line); line != "" && re.MatchString(line) {
			return true
		}
	}
	return false
}

// classifyExternalWatchOutput maps an output to the watch's declared terminal
// states. Empty string means non-terminal.
func classifyExternalWatchOutput(watch control.ExternalWatch, output string) string {
	if matchesExternalWatchPattern(watch.SuccessPattern, output) {
		return control.ExternalWatchSucceeded
	}
	if watch.FailurePattern != "" && matchesExternalWatchPattern(watch.FailurePattern, output) {
		return control.ExternalWatchFailed
	}
	return ""
}

func truncateExternalWatchOutput(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if utf8.RuneCountInString(value) <= externalWatchOutputRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:externalWatchOutputRunes]) + "\n... output truncated"
}

func (d *Server) completeExternalWatch(ctx context.Context, watch control.ExternalWatch, status, output, lastError string) {
	finished, err := d.Control.FinishExternalWatch(ctx, watch.TenantID, watch.ID, status, output, lastError)
	if err != nil {
		log.Warn("external watch finalization failed", "watch_id", watch.ID, "error", err)
		return
	}
	if !finished {
		return
	}
	d.finalizeExternalWatch(ctx, watch, status, output, lastError)
}

// recoverExternalWatchVerdicts runs the two boot compensation passes:
// (1) re-examine recently timed_out watches against their recorded output —
// a watch whose last checkpoint already showed a declared terminal state was
// misjudged (e.g. by the pre-normalization pattern matcher) and is revised to
// its true verdict; (2) replay completion side effects for terminal watches
// whose finalize step never ran (daemon exit between the terminal-status CAS
// and the side effects). Side effects are at-least-once by design: losing a
// finalization forever is worse than a rare duplicate notice after a crash.
func (d *Server) recoverExternalWatchVerdicts(ctx context.Context) {
	defer d.runExternalWatchNotificationPass(ctx)
	since := time.Now().Add(-externalWatchRecoveryLookback)
	watches, err := d.Control.ListExternalWatchesFinishedSince(ctx, control.ExternalWatchTimedOut, since, 200)
	if err != nil {
		log.Warn("external watch verdict recovery scan failed", "error", err)
		return
	}
	for i := range watches {
		watch := watches[i]
		status := classifyExternalWatchOutput(watch, watch.LastOutput)
		if status == "" {
			continue
		}
		revised, err := d.Control.ReviseExternalWatchVerdict(ctx, watch.TenantID, watch.ID, control.ExternalWatchTimedOut, status)
		if err != nil {
			log.Warn("external watch verdict revision failed", "watch_id", watch.ID, "error", err)
			continue
		}
		if !revised {
			continue
		}
		log.Info("external watch verdict revised", "watch_id", watch.ID, "status", status)
	}
	// Revision reset finalized=0, so revised watches are compensated below
	// together with genuinely interrupted finalizations. Loop until the scan
	// drains (each finalize marks its watch, so progress is guaranteed), with
	// a hard pass cap so a pathological backlog cannot block startup.
	for pass := 0; pass < 20; pass++ {
		unfinalized, err := d.Control.ListUnfinalizedExternalWatches(ctx, since, 100)
		if err != nil {
			log.Warn("external watch finalization compensation scan failed", "error", err)
			return
		}
		if len(unfinalized) == 0 {
			return
		}
		for i := range unfinalized {
			watch := unfinalized[i]
			log.Info("external watch finalization compensated", "watch_id", watch.ID, "status", watch.Status)
			d.finalizeExternalWatch(ctx, watch, watch.Status, watch.LastOutput, watch.LastError)
		}
		if len(unfinalized) < 100 {
			return
		}
	}
}

// finalizeExternalWatch materializes the durable completion products for a
// terminal watch. Each product is independently idempotent, and finalized is
// set only after every required durable write succeeds. Endpoint delivery is
// tracked separately so a channel outage cannot replay these core products.
func (d *Server) finalizeExternalWatch(ctx context.Context, watch control.ExternalWatch, status, output, lastError string) {
	// Materialize from the terminal snapshot, not the pre-claim watch value.
	// The latter may still carry pending status and an empty output buffer.
	watch.Status = status
	watch.LastOutput = output
	watch.LastError = lastError
	summary, nextSteps := externalWatchOutcome(watch, status, output, lastError)
	if err := d.Control.UpdateTaskStatus(ctx, watch.TenantID, watch.TaskID, "waiting_finalization", summary, nextSteps); err != nil {
		log.Warn("external watch task update failed", "watch_id", watch.ID, "error", err)
		return
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"watch_id":    watch.ID,
		"revision":    watch.VerdictRevision,
		"description": watch.Description,
		"status":      status,
		"summary":     summary,
		"attempts":    watch.Attempts + 1,
	})
	if _, err := d.Control.AppendEvent(ctx, control.Event{
		TaskID:         watch.TaskID,
		RunID:          watch.RunID,
		Type:           "external_watch.completed",
		Visibility:     "task",
		Channel:        watch.Channel,
		Payload:        payload,
		IdempotencyKey: fmt.Sprintf("external-watch:%s:r%d:completed", watch.ID, watch.VerdictRevision),
	}); err != nil {
		log.Warn("external watch event append failed", "watch_id", watch.ID, "error", err)
		return
	}

	origin := d.externalWatchOriginIdentity(ctx, watch)
	if err := d.enqueueExternalWatchFinalization(ctx, watch, origin, summary); err != nil {
		log.Warn("external watch finalization enqueue failed", "watch_id", watch.ID, "error", err)
		return
	}
	if marked, err := d.Control.MarkExternalWatchFinalized(ctx, watch.TenantID, watch.ID); err != nil {
		log.Warn("external watch finalized mark failed", "watch_id", watch.ID, "error", err)
		return
	} else if !marked {
		log.Warn("external watch finalized twice (benign after crash recovery)", "watch_id", watch.ID)
	}
	d.notifyExternalWatchCompletion(ctx, watch)
}

func (d *Server) runExternalWatchNotificationPass(ctx context.Context) {
	watches, err := d.Control.ListUnnotifiedExternalWatches(ctx, time.Now().Add(-externalWatchRecoveryLookback), externalWatchPassLimit)
	if err != nil {
		log.Warn("external watch notification scan failed", "error", err)
		return
	}
	for i := range watches {
		d.notifyExternalWatchCompletion(ctx, watches[i])
	}
}

func (d *Server) notifyExternalWatchCompletion(ctx context.Context, watch control.ExternalWatch) {
	if watch.Notified {
		return
	}
	summary, _ := externalWatchOutcome(watch, watch.Status, watch.LastOutput, watch.LastError)
	notice := summary + "\n\nReply continue to resume the task."
	if watch.Status == control.ExternalWatchSucceeded {
		notice = summary + "\n\nA finalization run is queued to record the result and close out the task."
	} else {
		notice = summary + "\n\nA finalization run is queued to diagnose the result and leave the task in the correct state."
	}
	origin := d.externalWatchOriginIdentity(ctx, watch)
	handled := d.coordinator().routePendingNotification(ctx, origin, watch.Channel, delivery.Message{
		TenantID: watch.TenantID,
		PersonID: watch.PersonID,
		TaskID:   watch.TaskID,
		RunID:    watch.RunID,
		Content:  notice,
		Kind:     "external_watch",
	})
	if !handled {
		// The finalization queue is itself a durable user-visible intent. An
		// attached CLI is not delivery evidence, but a persisted follow-up
		// prevents this terminal watch from disappearing across restarts.
		queued, err := d.Control.GetQueuedByIdempotencyKey(ctx, externalWatchFinalizationKey(watch))
		handled = err == nil && queued != nil
	}
	if !handled {
		return
	}
	if _, err := d.Control.MarkExternalWatchNotified(ctx, watch.TenantID, watch.ID); err != nil {
		log.Warn("external watch notified mark failed", "watch_id", watch.ID, "error", err)
	}
}

// externalWatchOriginIdentity resolves the account behind the watch's channel
// (IM channels are keyed by platform user id). Unmatched channels — CLI
// session UUIDs — fall back to the cli identity, whose notification path
// already routes to the person's preferred IM when the CLI is detached.
func (d *Server) externalWatchOriginIdentity(ctx context.Context, watch control.ExternalWatch) *control.IdentityContext {
	return d.routeIdentityForPerson(ctx, watch.TenantID, watch.PersonID, watch.Channel, "cli", nil)
}

// enqueueExternalWatchFinalization queues one durable follow-up run that
// records the completed external operation and closes out the task (backfill
// records, accurate final status). The daemon watcher verdict is authoritative
// evidence; this follow-up must not poll the external service again. It rides
// the ordinary task_queue path, so
// it respects the one-active-run rule and boot recovery. Its stable
// idempotency key makes crash-recovery replays converge on the same queue row.
func (d *Server) enqueueExternalWatchFinalization(ctx context.Context, watch control.ExternalWatch, origin *control.IdentityContext, summary string) error {
	content := externalWatchFinalizationContent(watch, summary)
	_, err := d.Control.EnqueueQueued(ctx, control.QueuedTask{
		TenantID:       watch.TenantID,
		PersonID:       watch.PersonID,
		Channel:        watch.Channel,
		Platform:       origin.Platform,
		PlatformUserID: origin.PlatformUserID,
		Content:        content,
		TaskID:         watch.TaskID,
		// Stable per-verdict key: a crash-recovery replay of the SAME verdict
		// is one row; a revised verdict earns one fresh finalization run.
		IdempotencyKey: externalWatchFinalizationKey(watch),
	})
	if err != nil {
		return err
	}
	d.coordinator().drainQueue(origin)
	return nil
}

func externalWatchFinalizationContent(watch control.ExternalWatch, summary string) string {
	evidence := truncateExternalWatchOutput(strings.TrimSpace(watch.LastOutput))
	if utf8.RuneCountInString(evidence) > 1200 {
		runes := []rune(evidence)
		evidence = string(runes[:1200]) + "\n... evidence truncated"
	}
	verdictInstruction := "The daemon's durable external watcher verified that this operation completed successfully."
	finalInstruction := "Backfill any pending records this task promised to update, summarize the final state, and finish the task with an accurate status."
	if watch.Status != control.ExternalWatchSucceeded {
		verdictInstruction = fmt.Sprintf(
			"The daemon's durable external watcher recorded terminal status %s for this operation.",
			firstNonEmpty(strings.TrimSpace(watch.Status), "failed"),
		)
		finalInstruction = "Diagnose the recorded terminal evidence, preserve any completed work, and finish as failed, blocked, waiting_user, or waiting_external as the evidence requires."
	}
	return fmt.Sprintf(
		"%s "+
			"Treat the recorded watcher result as authoritative evidence and do not rerun the external status check unless other durable evidence directly contradicts it. "+
			"This is an unattended finalization run: use file tools only; do not invoke terminal, shell, or network tools. "+
			"If the recorded evidence is insufficient or a privileged operation is required, finish with waiting_user instead of requesting approval or retrying variants.\n\n"+
			"Result: %s\nRecorded output:\n%s\n\n"+
			"Finalize the task now: %s",
		verdictInstruction, summary, firstNonEmpty(evidence, "(no output recorded)"), finalInstruction)
}

func externalWatchFinalizationKey(watch control.ExternalWatch) string {
	return fmt.Sprintf("external-watch:%s:r%d:finalization", watch.ID, watch.VerdictRevision)
}

// reconcileExternalWatchFinalizations closes the crash and partial-run gaps
// after a watcher has established any terminal verdict. The watcher is the fact
// source; the queue row is only a recoverable materialization job. A queued,
// started, done, or failed job that left its task non-terminal is retried with
// a hard budget, while live runs and terminal tasks are never duplicated.
func (d *Server) reconcileExternalWatchFinalizations(ctx context.Context) {
	if d == nil || d.Control == nil {
		return
	}
	var watches []control.ExternalWatch
	for _, status := range []string{
		control.ExternalWatchSucceeded,
		control.ExternalWatchFailed,
		control.ExternalWatchTimedOut,
	} {
		found, err := d.Control.ListExternalWatchesFinishedSince(ctx, status, time.Now().Add(-externalWatchRecoveryLookback), 200)
		if err != nil {
			log.Warn("external watch finalization reconciliation scan failed", "status", status, "error", err)
			return
		}
		watches = append(watches, found...)
	}
	for i := range watches {
		watch := watches[i]
		task, err := d.Control.GetTask(ctx, watch.TenantID, watch.TaskID)
		if err != nil || task == nil {
			if err != nil {
				log.Warn("external watch finalization task lookup failed", "watch_id", watch.ID, "error", err)
			}
			continue
		}
		recoverableShutdownCancel := d.recoverableGatewayShutdownCancellation(ctx, task)
		if terminalTaskStatus(task.Status) && !recoverableShutdownCancel {
			continue
		}
		if active := d.coordinator().currentActive(watch.PersonID); active != nil && active.TaskID == watch.TaskID {
			continue
		}
		if task.ActiveRunID != "" {
			run, err := d.Control.GetRun(ctx, watch.TenantID, task.ActiveRunID)
			if err != nil {
				log.Warn("external watch finalization active run lookup failed", "watch_id", watch.ID, "error", err)
				continue
			}
			if run != nil && strings.EqualFold(run.Status, "running") {
				continue
			}
		}

		key := externalWatchFinalizationKey(watch)
		queued, err := d.Control.GetQueuedByIdempotencyKey(ctx, key)
		if err != nil {
			log.Warn("external watch finalization queue lookup failed", "watch_id", watch.ID, "error", err)
			continue
		}
		summary, nextSteps := externalWatchOutcome(watch, watch.Status, watch.LastOutput, watch.LastError)
		origin := d.externalWatchOriginIdentity(ctx, watch)
		if queued == nil {
			if task.Status != "waiting_finalization" {
				_ = d.Control.UpdateTaskStatus(ctx, watch.TenantID, watch.TaskID, "waiting_finalization", summary, nextSteps)
			}
			if err := d.enqueueExternalWatchFinalization(ctx, watch, origin, summary); err != nil {
				log.Warn("external watch missing finalization enqueue failed", "watch_id", watch.ID, "error", err)
			}
			continue
		}
		if queued.Status != control.QueueStatusCancelled {
			updated, err := d.Control.UpdateSystemQueuedContent(ctx, watch.TenantID, queued.ID, externalWatchFinalizationContent(watch, summary))
			if err != nil {
				log.Warn("external watch finalization prompt refresh failed", "watch_id", watch.ID, "error", err)
				continue
			}
			if !updated {
				log.Warn("external watch finalization prompt was not refreshed", "watch_id", watch.ID, "queue_id", queued.ID)
				continue
			}
		}

		switch queued.Status {
		case control.QueueStatusQueued:
			if task.Status != "waiting_finalization" {
				_ = d.Control.UpdateTaskStatus(ctx, watch.TenantID, watch.TaskID, "waiting_finalization", summary, nextSteps)
			}
			d.coordinator().drainQueue(origin)
		case control.QueueStatusStarted:
			materialized, err := d.Control.RunHasSuccessfulTerminalEvent(ctx, queued.RunID)
			if err != nil {
				log.Warn("external watch started finalization terminal lookup failed", "watch_id", watch.ID, "run_id", queued.RunID, "error", err)
				continue
			}
			if materialized {
				if _, err := d.Control.MarkQueuedIfStatus(ctx, watch.TenantID, queued.ID, control.QueueStatusStarted, control.QueueStatusDone); err != nil {
					log.Warn("external watch completed finalization settlement failed", "watch_id", watch.ID, "queue_id", queued.ID, "error", err)
				}
				continue
			}
			fallthrough
		case control.QueueStatusFailed:
			requeued, err := d.Control.RequeueSystemQueued(ctx, watch.TenantID, queued.ID, externalWatchFinalizationRetries)
			if err != nil {
				log.Warn("external watch finalization requeue failed", "watch_id", watch.ID, "error", err)
				continue
			}
			if requeued {
				_ = d.Control.UpdateTaskStatus(ctx, watch.TenantID, watch.TaskID, "waiting_finalization", summary, nextSteps)
				d.coordinator().drainQueue(origin)
				continue
			}
			d.blockExternalWatchFinalization(ctx, watch, queued)
		case control.QueueStatusDone:
			materialized, err := d.Control.RunHasSuccessfulTerminalEvent(ctx, queued.RunID)
			if err != nil {
				log.Warn("external watch finalization terminal lookup failed", "watch_id", watch.ID, "run_id", queued.RunID, "error", err)
				continue
			}
			if materialized {
				continue
			}
			requeued, err := d.Control.RequeueDoneSystemQueuedIfUnmaterialized(ctx, watch.TenantID, queued.ID, externalWatchFinalizationRetries)
			if err != nil {
				log.Warn("external watch incomplete finalization requeue failed", "watch_id", watch.ID, "run_id", queued.RunID, "error", err)
				continue
			}
			if requeued {
				_ = d.Control.UpdateTaskStatus(ctx, watch.TenantID, watch.TaskID, "waiting_finalization", summary, nextSteps)
				d.coordinator().drainQueue(origin)
				continue
			}
			d.blockExternalWatchFinalization(ctx, watch, queued)
		case control.QueueStatusCancelled:
			d.blockExternalWatchFinalization(ctx, watch, queued)
		}
	}
}

// recoverableGatewayShutdownCancellation recognizes both the current durable
// shutdown summary and the legacy two-write shape produced by older daemons:
// stopAllActive first appended reason=gateway shutdown, then the unwinding run
// overwrote the task summary with a generic cancellation. Only the newest
// cancelled run is considered, so an older restart can never reopen a later
// explicit user cancellation.
func (d *Server) recoverableGatewayShutdownCancellation(ctx context.Context, task *control.Task) bool {
	if task == nil || !strings.EqualFold(strings.TrimSpace(task.Status), "cancelled") {
		return false
	}
	summary := strings.TrimSpace(task.CurrentSummary)
	if strings.EqualFold(summary, "gateway shutdown") || strings.EqualFold(summary, "interrupted by gateway shutdown.") {
		return true
	}
	events, err := d.Control.ListTaskEvents(ctx, task.ID, 80)
	if err != nil {
		log.Warn("external watch shutdown history lookup failed", "task_id", task.ID, "error", err)
		return false
	}
	latestCancelledRun := ""
	for _, event := range events {
		if event.Type == "run.cancelled" && strings.TrimSpace(event.RunID) != "" {
			latestCancelledRun = event.RunID
			break
		}
	}
	if latestCancelledRun == "" {
		return false
	}
	for _, event := range events {
		if event.Type != "run.cancelled" || event.RunID != latestCancelledRun {
			continue
		}
		var payload struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && strings.EqualFold(strings.TrimSpace(payload.Reason), "gateway shutdown") {
			return true
		}
	}
	return false
}

func (d *Server) blockExternalWatchFinalization(ctx context.Context, watch control.ExternalWatch, queued *control.QueuedTask) {
	reason := fmt.Sprintf("The external watch reached %s, but its finalization run did not complete after %d recovery attempts.", watch.Status, queued.Restarts)
	next := []string{"Review the recorded watcher evidence and resume this task to finish its release record."}
	if queued.Status == control.QueueStatusCancelled {
		reason = fmt.Sprintf("The external watch reached %s, but its finalization run was cancelled.", watch.Status)
	}
	_ = d.Control.UpdateTaskStatus(ctx, watch.TenantID, watch.TaskID, "blocked", reason, next)
	payload, _ := json.Marshal(map[string]interface{}{
		"watch_id": watch.ID,
		"queue_id": queued.ID,
		"status":   queued.Status,
		"restarts": queued.Restarts,
	})
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:         watch.TaskID,
		RunID:          watch.RunID,
		Type:           "external_watch.finalization_blocked",
		Visibility:     "task",
		Channel:        watch.Channel,
		Payload:        payload,
		IdempotencyKey: externalWatchFinalizationKey(watch) + ":blocked",
	})
}

func externalWatchOutcome(watch control.ExternalWatch, status, output, lastError string) (string, []string) {
	description := strings.TrimSpace(watch.Description)
	if description == "" {
		description = "External operation"
	}
	preview := truncate(toOneLine(strings.TrimSpace(output)), 240)
	switch status {
	case control.ExternalWatchSucceeded:
		message := fmt.Sprintf("%s completed.", description)
		if preview != "" {
			message += " " + preview
		}
		return message, []string{"Review the completed external operation and continue the task."}
	case control.ExternalWatchTimedOut:
		message := fmt.Sprintf("%s did not reach a terminal state before the watch timed out; the external operation may still be running.", description)
		if preview != "" {
			message += " Last observed output: " + preview
		}
		return message, []string{"Inspect the external service status, then continue or register a new watch."}
	default:
		reason := truncate(toOneLine(strings.TrimSpace(lastError)), 180)
		if reason == "" {
			reason = preview
		}
		message := fmt.Sprintf("%s reported a failure.", description)
		if reason != "" {
			message += " " + reason
		}
		return message, []string{"Inspect the failure evidence and continue with a corrected action."}
	}
}
