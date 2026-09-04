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
	if watch.SpecVersion >= 3 && commandErr == nil && result.ExitCode == 0 {
		state, adapterErr := tools.ClassifyExternalWatchObservation(watch.ObservationAdapter, output)
		if adapterErr != nil {
			d.parkExternalWatch(ctx, watch, watchCheckVerdict{
				Action: watchCheckPark, Layer: "observation", Reason: watchReasonInvalidCheck,
				Detail: adapterErr.Error(),
			}, "check_definition", output, adapterErr.Error())
			return
		}
		switch state {
		case tools.ExternalWatchObservationSucceeded:
			_ = d.Control.RecordExternalWatchPhases(ctx, watch.TenantID, watch.ID,
				control.WatchCheckerOK, control.WatchOperationSucceeded, control.WatchVerificationPassed)
			d.completeExternalWatch(ctx, watch, control.ExternalWatchSucceeded, output, "")
			return
		case tools.ExternalWatchObservationFailed:
			_ = d.Control.RecordExternalWatchPhases(ctx, watch.TenantID, watch.ID,
				control.WatchCheckerOK, control.WatchOperationFailed, control.WatchVerificationNotRequired)
			d.completeExternalWatch(ctx, watch, control.ExternalWatchFailed, output, "")
			return
		case tools.ExternalWatchObservationPending:
			// Continue into the ordinary deadline/checkpoint path below.
		}
	}

	// A v2 terminal operation marker is authoritative even when a LATER
	// verification command in the same script fails. Preserve that operation
	// truth, then park the verification layer; otherwise a completed deployment
	// is polled until a false timeout merely because kubectl/gcloud auth broke.
	operation := operationStatusFromOutput(watch, output)
	verdict := classifyWatchCheck(result.FailureClass, result.ExitCode, commandErr != nil)
	// A declared terminal business failure is authoritative. Authentication or
	// verifier errors printed later in the same check must not rewrite a failed
	// deployment as blocked_environment and hide the actual outcome.
	if operation == control.WatchOperationFailed {
		watch.CheckerStatus = control.WatchCheckerOK
		watch.OperationStatus = operation
		watch.VerificationStatus = control.WatchVerificationNotRequired
		_ = d.Control.RecordExternalWatchPhases(ctx, watch.TenantID, watch.ID,
			watch.CheckerStatus, watch.OperationStatus, watch.VerificationStatus)
		d.completeExternalWatch(ctx, watch, control.ExternalWatchFailed, output, statusErrText(control.ExternalWatchFailed, errText))
		return
	}
	if operation == control.WatchOperationSucceeded && verdict.Action != watchCheckObserve {
		watch.CheckerStatus = checkerStatusForVerdict(verdict)
		watch.OperationStatus = operation
		watch.VerificationStatus = control.WatchVerificationBlocked
		_ = d.Control.RecordExternalWatchPhases(ctx, watch.TenantID, watch.ID,
			watch.CheckerStatus, watch.OperationStatus, watch.VerificationStatus)
		verdict.Detail = firstNonEmpty(verdict.Detail,
			"the external operation reached its declared success state, but the verification check could not complete")
		d.parkExternalWatch(ctx, watch, verdict, result.FailureClass, output, errText)
		return
	}
	if verdict.Action == watchCheckPark {
		watch.CheckerStatus = checkerStatusForVerdict(verdict)
		watch.OperationStatus = firstNonEmpty(operation, control.WatchOperationRunning)
		watch.VerificationStatus = control.WatchVerificationPending
		_ = d.Control.RecordExternalWatchPhases(ctx, watch.TenantID, watch.ID,
			watch.CheckerStatus, watch.OperationStatus, watch.VerificationStatus)
		d.parkExternalWatch(ctx, watch, verdict, result.FailureClass, output, errText)
		return
	}
	if verdict.Action == watchCheckObserve {
		if status := classifyExternalWatchOutput(watch, output, result.ExitCode); status != "" {
			watch.CheckerStatus = control.WatchCheckerOK
			watch.OperationStatus, watch.VerificationStatus = terminalWatchPhases(watch, status)
			_ = d.Control.RecordExternalWatchPhases(ctx, watch.TenantID, watch.ID,
				watch.CheckerStatus, watch.OperationStatus, watch.VerificationStatus)
			d.completeExternalWatch(ctx, watch, status, output, statusErrText(status, errText))
			return
		}
	}
	// A failed check is not an observation. Its streak is recorded so an
	// identical failure cannot be repeated until the deadline.
	if commandErr != nil || strings.TrimSpace(result.FailureClass) != "" || result.ExitCode != 0 {
		_ = d.Control.RecordExternalWatchPhases(ctx, watch.TenantID, watch.ID,
			control.WatchCheckerCommandFailed, firstNonEmpty(operation, control.WatchOperationRunning), control.WatchVerificationPending)
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
		watch.CheckerStatus = control.WatchCheckerOK
		watch.OperationStatus = control.WatchOperationTimedOut
		watch.VerificationStatus = control.WatchVerificationPending
		_ = d.Control.RecordExternalWatchPhases(ctx, watch.TenantID, watch.ID,
			watch.CheckerStatus, watch.OperationStatus, watch.VerificationStatus)
		d.completeExternalWatch(ctx, watch, control.ExternalWatchTimedOut, output, firstNonEmpty(errText, "watch deadline reached"))
		return
	}
	_ = d.Control.RecordExternalWatchPhases(ctx, watch.TenantID, watch.ID,
		control.WatchCheckerOK, control.WatchOperationRunning, control.WatchVerificationPending)
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

func operationStatusFromOutput(watch control.ExternalWatch, output string) string {
	if watch.SpecVersion >= 2 {
		if watch.TerminalFailurePattern != "" && matchesExternalWatchPattern(watch.TerminalFailurePattern, output) {
			return control.WatchOperationFailed
		}
		if watch.TerminalSuccessPattern != "" && matchesExternalWatchPattern(watch.TerminalSuccessPattern, output) {
			return control.WatchOperationSucceeded
		}
	}
	return ""
}

func checkerStatusForVerdict(verdict watchCheckVerdict) string {
	if verdict.Reason == watchReasonBlockedEnvironment || verdict.Reason == watchReasonEnvironmentChanged {
		return control.WatchCheckerCapabilityBlocked
	}
	return control.WatchCheckerCommandFailed
}

func terminalWatchPhases(watch control.ExternalWatch, status string) (string, string) {
	switch status {
	case control.ExternalWatchSucceeded:
		if watch.SpecVersion >= 2 {
			return control.WatchOperationSucceeded, control.WatchVerificationPassed
		}
		return control.WatchOperationSucceeded, control.WatchVerificationNotRequired
	case control.ExternalWatchFailed:
		return control.WatchOperationFailed, control.WatchVerificationNotRequired
	default:
		return control.WatchOperationTimedOut, control.WatchVerificationPending
	}
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
	if watch.SpecVersion >= 3 {
		if exitCode != 0 {
			return ""
		}
		state, err := tools.ClassifyExternalWatchObservation(watch.ObservationAdapter, output)
		if err != nil {
			return ""
		}
		switch state {
		case tools.ExternalWatchObservationSucceeded:
			return control.ExternalWatchSucceeded
		case tools.ExternalWatchObservationFailed:
			return control.ExternalWatchFailed
		default:
			return ""
		}
	}
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
	if strings.TrimSpace(watch.WaitGroupID) != "" {
		group, groupErr := d.Control.ResolveExternalWatchGroup(ctx, watch.TenantID, watch.WaitGroupID, watch.ID)
		if groupErr != nil {
			log.Warn("external watch group resolution failed", "watch_id", watch.ID, "group_id", watch.WaitGroupID, "error", groupErr)
			return
		}
		if !group.Terminal || !group.Won {
			return
		}
		status = group.Status
		watch.Status = status
		watch.Description = fmt.Sprintf("wait group %s (%s)", group.Group.GroupKey, group.Group.Mode)
		_, _ = d.Control.AppendEvent(ctx, control.Event{
			TaskID: watch.TaskID, RunID: watch.RunID, Type: "external_watch.group_resolved",
			Visibility: "task", Channel: watch.Channel,
			Payload: mustJSON(map[string]interface{}{
				"group_id": group.Group.ID, "group_key": group.Group.GroupKey,
				"mode": group.Group.Mode, "status": status, "winner_watch_id": watch.ID,
			}),
			IdempotencyKey: "external-watch-group-resolved:" + group.Group.ID,
		})
		if status == control.ExternalWatchFailed && strings.TrimSpace(lastError) == "" {
			lastError = "the aggregate wait-group condition could not be satisfied"
		}
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
	if err := d.Control.UpdateThreadSummary(ctx, watch.TenantID, watch.TaskID, summary, nextSteps); err != nil {
		log.Warn("external watch thread summary update failed", "watch_id", watch.ID, "error", err)
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

	// A watcher that concluded WITHOUT observing its target leaves the Run it
	// handed off with nothing able to wake it: the watcher is terminal, so no
	// monitoring signal remains, and `waiting_external` is not a resumable
	// status, so the Run is in no Attention set either. Park it as blocked as
	// soon as the verdict is durable rather than waiting for a finalization
	// FAILURE: the finalization run queues behind the person's active work and
	// can sit there for hours, and during that window the person's own work
	// must still be visible as theirs to resolve. A successful verdict needs no
	// park — its finalization reports the observed result — and a user
	// cancellation is already re-parked as waiting_user by
	// CancelExternalWatchForPerson.
	switch status {
	case control.ExternalWatchSucceeded, control.ExternalWatchCancelled:
	default:
		if parked, err := d.Control.MarkExternalWatchRunBlocked(ctx, watch.TenantID, watch.RunID, summary); err != nil {
			log.Warn("external watch run park failed", "watch_id", watch.ID, "run_id", watch.RunID, "error", err)
		} else if parked {
			log.Info("external watch run parked as blocked", "watch_id", watch.ID, "run_id", watch.RunID, "status", status)
		}
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
	// liveSurfaceInformed=true: a watcher's own events reach the attached TUI on
	// their normal path, so the established suppression still applies here.
	handled := d.coordinator().routePendingNotification(ctx, origin, watch.Channel, delivery.Message{
		TenantID: watch.TenantID,
		PersonID: watch.PersonID,
		TaskID:   watch.TaskID,
		RunID:    watch.RunID,
		Content:  externalWatchNotice(watch, "waiting_finalization"),
		Kind:     "external_watch",
	}, true)
	if !handled && origin != nil && origin.Platform == "cli" &&
		d.presenceTracker().IsAttached(watch.PersonID, "cli") {
		// external_watch.completed was committed before this method and is
		// published through the durable event stream. An attached CLI therefore
		// has a real user-visible surface even though routePendingNotification
		// correctly suppresses a duplicate IM push.
		handled = true
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
	if watch.OperationStatus == control.WatchOperationSucceeded && watch.VerificationStatus == control.WatchVerificationBlocked {
		return fmt.Sprintf("Watcher %s | operation: succeeded | verification: blocked_environment | task: %s", shortExternalWatchID(watch.ID), taskStatus)
	}
	if reason, ok := watchCheckDefect(watch.LastError); ok {
		return "Watcher " + shortExternalWatchID(watch.ID) + " blocked: " + reason +
			" | the external state was not observed | task: " + taskStatus
	}
	return externalWatchCompletionNotice(watch.ID, watch.Status, taskStatus)
}

func externalWatchCompletionNotice(watchID, status, taskStatus string) string {
	watchID = shortExternalWatchID(watchID)
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
// The row replies to the watcher Run while that Run is still parked on its
// watcher, so the drained finalization becomes that Run's exact child.
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
		ReplyToRunID:   d.externalWatchFinalizationTarget(ctx, watch),
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

// externalWatchFinalizationTarget names the watcher Run the finalization
// continues as its exact child: the Run that registered the watch, while it is
// still parked as waiting_external for this person and thread. The drain
// claims it atomically (validateResumeClaimTx admits a waiting_external parent
// once its watchers concluded). A Run that already moved on leaves the row
// bound to the Thread only, so a stale exact binding can never make the
// finalization unroutable.
func (d *Server) externalWatchFinalizationTarget(ctx context.Context, watch control.ExternalWatch) string {
	if strings.TrimSpace(watch.RunID) == "" {
		return ""
	}
	run, err := d.Control.GetRun(ctx, watch.TenantID, watch.RunID)
	if err != nil || run == nil || run.PersonID != watch.PersonID || run.TaskID != watch.TaskID {
		return ""
	}
	if !strings.EqualFold(run.Status, "waiting_external") {
		return ""
	}
	return run.ID
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
		finalInstruction = "Diagnose and record the external result, preserve any completed work, then finish this finalization run as done. The daemon records the external failure or timeout separately and will keep the task blocked or waiting for the user."
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
		queued, err := d.Control.GetQueuedByIdempotencyKey(ctx, watch.TenantID, key)
		if err != nil {
			log.Warn("external watch finalization queue lookup failed", "watch_id", watch.ID, "error", err)
			continue
		}
		boundRunID := ""
		if queued != nil {
			boundRunID = queued.RunID
		}
		if d.externalWatchFinalizationSuppressedByUserCancellation(ctx, watch, boundRunID) {
			continue
		}
		summary, nextSteps := externalWatchOutcome(watch, watch.Status, watch.LastOutput, watch.LastError)
		origin := d.externalWatchOriginIdentity(ctx, watch)
		// The Thread summary is presentation; refresh it only when it drifted so
		// a reconciliation pass does not churn the Thread's activity clock.
		refreshSummary := func() {
			if task.CurrentSummary != summary {
				_ = d.Control.UpdateThreadSummary(ctx, watch.TenantID, watch.TaskID, summary, nextSteps)
			}
		}
		if queued == nil {
			refreshSummary()
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
			refreshSummary()
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
			// The queue claim lease is the primary worker-ownership signal. Keep
			// this direct run lookup as a migration and crash-window guard: rows
			// created before leases existed (or briefly observed before their first
			// renewal) must never be reopened while the bound run is still live.
			if queued.RunID != "" {
				boundRun, runErr := d.Control.GetRun(ctx, watch.TenantID, queued.RunID)
				if runErr != nil {
					log.Warn("external watch started finalization run lookup failed", "watch_id", watch.ID, "run_id", queued.RunID, "error", runErr)
					continue
				}
				if boundRun != nil && strings.EqualFold(boundRun.Status, "running") {
					continue
				}
			}
			fallthrough
		case control.QueueStatusFailed:
			requeued, err := d.Control.RequeueSystemQueued(ctx, watch.TenantID, queued.ID, externalWatchFinalizationRetries)
			if err != nil {
				log.Warn("external watch finalization requeue failed", "watch_id", watch.ID, "error", err)
				continue
			}
			if requeued {
				refreshSummary()
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
				refreshSummary()
				d.coordinator().drainQueue(origin)
				continue
			}
			d.blockExternalWatchFinalization(ctx, watch, queued)
		case control.QueueStatusCancelled:
			d.blockExternalWatchFinalization(ctx, watch, queued)
		}
	}
}

// externalWatchFinalizationSuppressedByUserCancellation prevents recovery from
// reopening work the person explicitly cancelled. A Thread has no aggregate
// execution status, and a cancellation of some unrelated Run in the same
// Thread says nothing about this watcher, so the decision reads only the exact
// Runs of this finalization: the watcher Run itself and the finalization Run
// bound to its queue row. A gateway-shutdown cancellation is recoverable; any
// other cancellation of those Runs is user authority and suppresses the retry.
func (d *Server) externalWatchFinalizationSuppressedByUserCancellation(ctx context.Context, watch control.ExternalWatch, boundRunID string) bool {
	for _, runID := range []string{watch.RunID, boundRunID} {
		if d.runCancelledByUser(ctx, watch, runID) {
			return true
		}
	}
	return false
}

// runCancelledByUser reads one Run's own cancellation events. A run.cancelled
// carrying the gateway-shutdown reason is a daemon restart, not a decision.
func (d *Server) runCancelledByUser(ctx context.Context, watch control.ExternalWatch, runID string) bool {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return false
	}
	events, err := d.Control.ListRunEvents(ctx, watch.TenantID, watch.PersonID, watch.TaskID, runID, 80)
	if err != nil {
		log.Warn("external watch cancellation history lookup failed", "thread_id", watch.TaskID, "run_id", runID, "error", err)
		return false
	}
	// Events arrive newest first, and the most recent cancellation that states
	// a REASON describes this Run's current state. Reading past it would let an
	// older gateway shutdown excuse a retry the person has since cancelled;
	// a cancellation with no reason (the legacy "context canceled" shape) says
	// nothing on its own, so it neither decides nor hides the entry behind it.
	seenCancellation := false
	for _, event := range events {
		if event.Type != "run.cancelled" {
			continue
		}
		seenCancellation = true
		var payload struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		reason := strings.TrimSpace(payload.Reason)
		if reason == "" {
			continue
		}
		return !strings.EqualFold(reason, "gateway shutdown")
	}
	// An unexplained cancellation is treated as the person's: recovery may not
	// reopen work when it cannot prove the stop was infrastructure.
	return seenCancellation
}

// blockExternalWatchFinalization records that a watcher verdict exists but its
// finalization could not be materialized. The watcher Run itself is parked as
// blocked so Attention surfaces it as resumable work; the Thread summary and
// the durable event carry the reason.
func (d *Server) blockExternalWatchFinalization(ctx context.Context, watch control.ExternalWatch, queued *control.QueuedTask) {
	reason := fmt.Sprintf("The external watch reached %s, but its finalization run did not complete after %d recovery attempts.", watch.Status, queued.Restarts)
	next := []string{"Review the recorded watcher evidence and resume this task to finish its release record."}
	if queued.Status == control.QueueStatusCancelled {
		reason = fmt.Sprintf("The external watch reached %s, but its finalization run was cancelled.", watch.Status)
	}
	_ = d.Control.UpdateThreadSummary(ctx, watch.TenantID, watch.TaskID, reason, next)
	runBlocked, err := d.Control.MarkExternalWatchRunBlocked(ctx, watch.TenantID, watch.RunID, reason)
	if err != nil {
		log.Warn("external watch run block failed", "watch_id", watch.ID, "run_id", watch.RunID, "error", err)
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"watch_id":    watch.ID,
		"queue_id":    queued.ID,
		"status":      queued.Status,
		"restarts":    queued.Restarts,
		"run_id":      watch.RunID,
		"run_blocked": runBlocked,
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
	if watch.OperationStatus == control.WatchOperationSucceeded && watch.VerificationStatus == control.WatchVerificationBlocked {
		message := fmt.Sprintf("%s completed, but verification was blocked by the checker environment.", description)
		if preview != "" {
			message += " " + preview
		}
		return message, []string{"Restore the verification environment, then verify the completed operation without rerunning it."}
	}
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
