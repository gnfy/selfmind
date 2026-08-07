package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"selfmind/internal/control"
	"selfmind/internal/executionenv"
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
		if status := classifyStoredExternalWatchOutput(watch); status != "" {
			d.completeExternalWatch(ctx, watch, status, watch.LastOutput, "")
			return
		}
	}
	// A watch must never silently change identity. It is checked BEFORE the
	// command runs, because the failure mode is not an error — it is a check
	// that succeeds against the wrong account, project, or cluster.
	if changed := externalWatchEnvironmentChange(watch); len(changed) > 0 {
		d.completeExternalWatchEnvironmentChanged(ctx, watch, changed)
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(watch.CommandTimeoutSeconds)*time.Second)
	defer cancel()
	result, commandErr := d.runExternalWatchCommand(checkCtx, watch)
	output := truncateExternalWatchOutput(tools.RedactSensitive(result.Output))
	errText := ""
	if commandErr != nil {
		errText = tools.RedactSensitive(commandErr.Error())
	}

	// The layer order is fixed and must stay fixed: a lower-layer failure is
	// never matched against the declared patterns. Reversing these two steps is
	// what let a check that could not query anything report a business verdict.
	verdict := classifyWatchCheck(result.FailureClass, result.ExitCode, commandErr != nil)
	if verdict.Action == watchCheckPark {
		d.parkExternalWatch(ctx, watch, verdict, result.FailureClass, output, errText)
		return
	}
	if verdict.Action == watchCheckObserve {
		if status := classifyExternalWatchOutput(watch, output, result.ExitCode); status != "" {
			d.completeExternalWatch(ctx, watch, status, output, statusErrText(status, errText))
			return
		}
	}
	// A failed check is not an observation. Its streak is recorded so an
	// identical failure cannot be repeated until the deadline.
	if commandErr != nil || strings.TrimSpace(result.FailureClass) != "" || result.ExitCode != 0 {
		signature := watchCheckSignature(result.FailureClass, output)
		streak, err := d.Control.RecordExternalWatchFailure(ctx, watch.TenantID, watch.ID,
			output, errText, result.FailureClass, signature)
		if err != nil {
			log.Warn("external watch failure checkpoint failed", "watch_id", watch.ID, "error", err)
			return
		}
		if streak >= watchRepeatedFailureLimit {
			repeated := watchCheckVerdict{
				Action: watchCheckPark,
				Layer:  verdict.Layer,
				Reason: watchReasonRepeatedFailure,
				Detail: fmt.Sprintf("the same check failure repeated %d times without observing the external state", streak),
			}
			d.parkExternalWatch(ctx, watch, repeated, result.FailureClass, output, errText)
		}
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

// runExternalWatchCommand executes one durable check with the same execution
// material a foreground command gets.
//
// This path has no host escape hatch, so an unprepared environment is fatal
// rather than recoverable: a live gcloud watch failed on all six of its targets
// with CHECK_ERROR purely because it could not write its own credential store,
// while aws watches in the same workspace succeeded (the AWS CLI reads its
// config without writing it). The scratch key is the watch id, so the state
// overlay is materialized once and reused by every poll.
func (d *Server) runExternalWatchCommand(ctx context.Context, watch control.ExternalWatch) (tools.ExecutionResult, error) {
	binding := watch.ExecutionBinding
	capabilities := []string(nil)
	networkShared := true // grandfathered rows predate explicit capability binding
	if binding.Version > 0 {
		var err error
		capabilities, err = d.validateExternalWatchCapabilities(ctx, watch, binding)
		if err != nil {
			return tools.ExecutionResult{FailureClass: "environment"}, err
		}
		networkShared = hasExecutionCapability(capabilities, executionenv.CapabilityNetworkShared)
		if !networkShared {
			return tools.ExecutionResult{FailureClass: "environment"},
				fmt.Errorf("external watch execution binding does not include network:shared")
		}
	}
	durable := tools.DurableExecutionScope{
		ScratchKey:   watch.ID,
		TenantID:     watch.TenantID,
		PersonID:     watch.PersonID,
		WorkspaceID:  watch.WorkspaceID,
		Capabilities: capabilities,
		Binding:      binding,
	}
	if binding.Version == 0 && d != nil && d.Control != nil && strings.TrimSpace(watch.WorkspaceID) != "" {
		if workspace, err := d.Control.GetWorkspace(ctx, watch.TenantID, watch.WorkspaceID); err == nil && workspace != nil {
			durable.TrustLevel = workspace.TrustLevel
		}
		if grants, err := d.Control.ListActiveExecutionCapabilities(ctx, watch.TenantID, watch.PersonID, watch.WorkspaceID); err == nil {
			for _, grant := range grants {
				durable.Capabilities = append(durable.Capabilities, grant.Capability)
			}
		}
	}
	return tools.RunDurableCheck(ctx, watch.Command, watch.CWD, networkShared, durable)
}

// validateExternalWatchCapabilities permits only capabilities present when the
// watch was registered. Later grants never widen a running watch; withdrawal
// of workspace trust or a persisted grant stops it before the next command.
func (d *Server) validateExternalWatchCapabilities(
	ctx context.Context,
	watch control.ExternalWatch,
	binding executionenv.Binding,
) ([]string, error) {
	if d == nil || d.Control == nil {
		return nil, fmt.Errorf("external watch capability store is unavailable")
	}
	workspace, err := d.Control.GetWorkspace(ctx, watch.TenantID, watch.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("read external watch workspace trust: %w", err)
	}
	if binding.TrustLevel == executionenv.TrustTrusted &&
		(workspace == nil || workspace.TrustLevel != executionenv.TrustTrusted) {
		return nil, fmt.Errorf("external watch workspace trust was revoked")
	}

	activeGrants, err := d.Control.ListActiveExecutionCapabilities(
		ctx, watch.TenantID, watch.PersonID, watch.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("read external watch capability grants: %w", err)
	}
	activeGrantIDs := map[string]bool{}
	for _, grant := range activeGrants {
		activeGrantIDs[grant.ID] = true
	}

	bindings := map[string]executionenv.CapabilityBinding{}
	for _, bound := range binding.CapabilityBindings {
		bindings[bound.Capability] = bound
	}
	out := make([]string, 0, len(binding.ExecutionCapabilities))
	for _, capability := range binding.ExecutionCapabilities {
		bound, described := bindings[capability]
		if !described {
			// Early v1 rows froze the capability names but not provenance. They
			// remain bounded (later grants are ignored); a trusted binding is
			// already covered by the trust check above.
			out = append(out, capability)
			continue
		}
		switch bound.Source {
		case executionenv.CapabilitySourceTrust:
			if workspace == nil || workspace.TrustLevel != executionenv.TrustTrusted {
				return nil, fmt.Errorf("external watch capability %s was revoked with workspace trust", capability)
			}
			if capability == executionenv.CapabilityNetworkShared && !tools.ExecSandboxAllowsNetwork() {
				return nil, fmt.Errorf("external watch network capability is disabled by current sandbox policy")
			}
		case executionenv.CapabilitySourceGrant:
			if strings.TrimSpace(bound.GrantID) == "" || !activeGrantIDs[bound.GrantID] {
				return nil, fmt.Errorf("external watch capability %s grant is expired or revoked", capability)
			}
		case executionenv.CapabilitySourceRegistration:
			if !bound.ExpiresAt.IsZero() && !bound.ExpiresAt.After(time.Now()) {
				return nil, fmt.Errorf("external watch capability %s registration approval expired", capability)
			}
		default:
			return nil, fmt.Errorf("external watch capability %s has unknown authorization source", capability)
		}
		out = append(out, capability)
	}
	return out, nil
}

func hasExecutionCapability(capabilities []string, target string) bool {
	for _, capability := range capabilities {
		if capability == target {
			return true
		}
	}
	return false
}

// parkExternalWatch stops a watch whose CHECK failed, recording the structured
// reason that keeps the failure attributable to the check rather than to the
// external operation.
//
// It reuses the failed terminal status (as the environment-change path does)
// so the existing finalization, notification, and recovery machinery applies
// unchanged; the reason prefix is what downstream surfaces read.
func (d *Server) parkExternalWatch(
	ctx context.Context,
	watch control.ExternalWatch,
	verdict watchCheckVerdict,
	failureClass, output, errText string,
) {
	evidence := strings.TrimSpace(output)
	if evidence == "" {
		evidence = strings.TrimSpace(errText)
	}
	reason := parkedWatchReason(verdict.Reason, verdict.Detail, evidence)
	if d != nil && d.Control != nil {
		_, _ = d.Control.AppendEvent(ctx, control.Event{
			TaskID:     watch.TaskID,
			RunID:      watch.RunID,
			Type:       "external_watch.blocked",
			Visibility: "task",
			Channel:    watch.Channel,
			Payload: mustJSON(map[string]interface{}{
				"watch_id":      watch.ID,
				"description":   watch.Description,
				"reason":        verdict.Reason,
				"layer":         verdict.Layer,
				"failure_class": failureClass,
				"detail":        verdict.Detail,
			}),
			IdempotencyKey: fmt.Sprintf("watch:%s:blocked:%s", watch.ID, verdict.Reason),
		})
	}
	log.Warn("external watch parked",
		"watch_id", watch.ID, "reason", verdict.Reason, "layer", verdict.Layer, "failure_class", failureClass)
	d.completeExternalWatch(ctx, watch, control.ExternalWatchBlocked, output, reason)
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

// classifyStoredExternalWatchOutput re-examines a recorded checkpoint.
//
// A row carrying a failure class recorded a FAILED CHECK, not an observation,
// so re-matching its text would resurrect the defect this file exists to
// prevent — on the recovery path, where nobody is watching. Rows written before
// the class existed carry none and keep the previous behaviour.
func classifyStoredExternalWatchOutput(watch control.ExternalWatch) string {
	if strings.TrimSpace(watch.FailureClass) != "" {
		return ""
	}
	return classifyExternalWatchOutput(watch, watch.LastOutput, 0)
}

// classifyExternalWatchOutput maps an output to the watch's declared terminal
// states. Empty string means non-terminal.
//
// Success requires a clean exit; failure does not. The asymmetry is deliberate:
// a status CLI may exit non-zero while reporting a genuine terminal failure, but
// a "success" printed by a check that itself failed is self-contradictory
// evidence, and accepting it would let a broken check close a release.
func classifyExternalWatchOutput(watch control.ExternalWatch, output string, exitCode int) string {
	if watch.SpecVersion >= 2 {
		if watch.TerminalFailurePattern != "" && matchesExternalWatchPattern(watch.TerminalFailurePattern, output) {
			return control.ExternalWatchFailed
		}
		if exitCode == 0 && watch.TerminalSuccessPattern != "" && matchesExternalWatchPattern(watch.TerminalSuccessPattern, output) {
			return control.ExternalWatchSucceeded
		}
		if exitCode == 0 && watch.TargetPattern != "" && matchesExternalWatchPattern(watch.TargetPattern, output) {
			return control.ExternalWatchSucceeded
		}
		return ""
	}
	if exitCode == 0 && matchesExternalWatchPattern(watch.SuccessPattern, output) {
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
	// The watch's state overlay (a copied credential store) is no longer needed
	// once the watch reaches a terminal state.
	if err := executionenv.CleanupLeaseScratch(watch.ID); err != nil {
		log.Debug("external watch scratch cleanup failed", "watch_id", watch.ID, "error", err)
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
		status := classifyStoredExternalWatchOutput(watch)
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
		"task_status": "waiting_finalization",
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
	origin := d.externalWatchOriginIdentity(ctx, watch)
	handled := d.coordinator().routePendingNotification(ctx, origin, watch.Channel, delivery.Message{
		TenantID: watch.TenantID,
		PersonID: watch.PersonID,
		TaskID:   watch.TaskID,
		RunID:    watch.RunID,
		Content:  externalWatchNotice(watch, "waiting_finalization"),
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

// externalWatchNotice renders the one-line endpoint notice. A parked watch says
// "blocked" and names the layer that failed, because "status: failed" on a check
// that never reached the external system reads as a failed deployment.
// Details stay out of the notice; `/diag execution` carries them.
func externalWatchNotice(watch control.ExternalWatch, taskStatus string) string {
	if reason, ok := watchCheckDefect(watch.LastError); ok {
		return "Watcher " + strings.TrimSpace(watch.ID) + " blocked: " + reason +
			" | the external state was not observed | task: " + taskStatus
	}
	return externalWatchCompletionNotice(watch.ID, watch.Status, taskStatus)
}

func externalWatchCompletionNotice(watchID, status, taskStatus string) string {
	watchID = strings.TrimSpace(watchID)
	taskStatus = strings.TrimSpace(taskStatus)
	if taskStatus == "" {
		taskStatus = "waiting_finalization"
	}
	watchStatus := control.ExternalWatchSucceeded
	switch strings.ToLower(strings.TrimSpace(status)) {
	case control.ExternalWatchFailed:
		watchStatus = control.ExternalWatchFailed
	case control.ExternalWatchTimedOut:
		watchStatus = control.ExternalWatchTimedOut
	case control.ExternalWatchCancelled:
		watchStatus = control.ExternalWatchCancelled
	case control.ExternalWatchBlocked:
		watchStatus = control.ExternalWatchBlocked
	}
	return "Watcher " + watchID + " | status: " + watchStatus + " | task: " + taskStatus
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
		Class:          control.QueueClassFinalization,
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
	// A parked watch says nothing about the external operation, and saying
	// otherwise here is how a check defect became a release record claiming a
	// build had failed. The finalization run must not convert it into a business
	// outcome in either direction.
	if reason, ok := watchCheckDefect(watch.LastError); ok {
		verdictInstruction = fmt.Sprintf(
			"The daemon's durable external watcher STOPPED because its own check could not observe the external state (%s). "+
				"This is NOT evidence about the external operation: its real state is unknown.",
			reason)
		finalInstruction = "Record that the check was blocked and why, leave every external-state claim unresolved (never write succeeded or failed), " +
			"and finish with waiting_user so a human can restore the check environment or verify the external state directly."
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

func externalWatchIDFromFinalizationKey(key string) string {
	const prefix = "external-watch:"
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, ":finalization") {
		return ""
	}
	rest := strings.TrimPrefix(key, prefix)
	revisionAt := strings.LastIndex(rest, ":r")
	if revisionAt <= 0 {
		return ""
	}
	return strings.TrimSpace(rest[:revisionAt])
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
	case control.ExternalWatchBlocked:
		reason := truncate(toOneLine(strings.TrimSpace(lastError)), 180)
		return fmt.Sprintf("The watcher for %s was stopped: %s", description, firstNonEmpty(reason, preview)),
			[]string{
				"Restore the check environment or fix the check command, then verify the external state directly.",
				"Do not record the external operation as succeeded or failed until it has actually been observed.",
			}
	default:
		reason := truncate(toOneLine(strings.TrimSpace(lastError)), 180)
		if reason == "" {
			reason = preview
		}
		// The check failed, not the operation. Naming the watcher (not the
		// operation) is the difference between "the release failed" and "we
		// could not look".
		if _, ok := watchCheckDefect(lastError); ok {
			return fmt.Sprintf("The watcher for %s was stopped: %s", description, reason),
				[]string{
					"Restore the check environment or fix the check command, then verify the external state directly.",
					"Do not record the external operation as succeeded or failed until it has actually been observed.",
				}
		}
		message := fmt.Sprintf("%s reported a failure.", description)
		if reason != "" {
			message += " " + reason
		}
		return message, []string{"Inspect the failure evidence and continue with a corrected action."}
	}
}

// externalWatchEnvironmentChange reports which non-secret dimensions of the
// execution environment moved since the watch was registered.
//
// Watches created before this identity existed carry no fingerprints; they are
// grandfathered rather than parked, because parking them would strand work the
// operator already started for a property that was never recorded.
func externalWatchEnvironmentChange(watch control.ExternalWatch) []string {
	if watch.ExecutionBinding.Version > 0 {
		if _, err := executionenv.ResolveBinding(executionenv.DefaultRegistry(), watch.ExecutionBinding); err != nil {
			var changed *executionenv.EnvironmentChangedError
			if errors.As(err, &changed) {
				return append([]string{}, changed.Changed...)
			}
			return []string{"environment unavailable"}
		}
		return nil
	}
	if strings.TrimSpace(watch.EnvironmentFingerprint) == "" &&
		strings.TrimSpace(watch.PrincipalFingerprint) == "" &&
		strings.TrimSpace(watch.CredentialSourceHash) == "" {
		return nil
	}
	current := executionenv.DefaultRegistry().Current()
	if current == nil {
		return nil
	}
	changed := make([]string, 0, 3)
	if watch.PrincipalFingerprint != "" && watch.PrincipalFingerprint != current.PrincipalFingerprint {
		changed = append(changed, "account/profile")
	}
	if watch.EnvironmentFingerprint != "" && watch.EnvironmentFingerprint != current.EnvironmentFingerprint {
		changed = append(changed, "PATH/HOME/proxy")
	}
	if watch.CredentialSourceHash != "" && watch.CredentialSourceHash != current.CredentialSourceHash {
		changed = append(changed, "credential source")
	}
	return changed
}

// completeExternalWatchEnvironmentChanged parks a watch whose environment moved.
// It is finished as failed with a structured reason so the existing notification
// and handoff path reports it, and the task is left waiting for a human decision
// rather than being retried under an identity nobody chose.
func (d *Server) completeExternalWatchEnvironmentChanged(ctx context.Context, watch control.ExternalWatch, changed []string) {
	reason := "environment_changed: " + strings.Join(changed, ", ") +
		" changed since this watch was registered; it was stopped instead of checking under a different identity. " +
		"Confirm the intended account and re-register the watch."
	if d != nil && d.Control != nil {
		_, _ = d.Control.AppendEvent(ctx, control.Event{
			TaskID:     watch.TaskID,
			RunID:      watch.RunID,
			Type:       "external_watch.environment_changed",
			Visibility: "task",
			Channel:    watch.Channel,
			Payload: mustJSON(map[string]interface{}{
				"watch_id":    watch.ID,
				"description": watch.Description,
				"changed":     changed,
			}),
			IdempotencyKey: "watch:" + watch.ID + ":environment-changed",
		})
	}
	d.completeExternalWatch(ctx, watch, control.ExternalWatchBlocked, watch.LastOutput, reason)
}
