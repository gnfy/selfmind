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
	if !watch.TimeoutAt.After(time.Now()) {
		d.completeExternalWatch(ctx, watch, control.ExternalWatchTimedOut, watch.LastOutput, "watch deadline reached")
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(watch.CommandTimeoutSeconds)*time.Second)
	defer cancel()
	output, commandErr := runExternalWatchCommand(checkCtx, watch.CWD, watch.Command)
	output = truncateExternalWatchOutput(tools.RedactSensitive(output))
	errText := ""
	if commandErr != nil {
		errText = tools.RedactSensitive(commandErr.Error())
	}

	if matchesExternalWatchPattern(watch.SuccessPattern, output) {
		d.completeExternalWatch(ctx, watch, control.ExternalWatchSucceeded, output, "")
		return
	}
	if watch.FailurePattern != "" && matchesExternalWatchPattern(watch.FailurePattern, output) {
		d.completeExternalWatch(ctx, watch, control.ExternalWatchFailed, output, errText)
		return
	}
	if !watch.TimeoutAt.After(time.Now()) {
		d.completeExternalWatch(ctx, watch, control.ExternalWatchTimedOut, output, firstNonEmpty(errText, "watch deadline reached"))
		return
	}
	if err := d.Control.RecordExternalWatchCheck(ctx, watch.TenantID, watch.ID, output, errText); err != nil {
		log.Warn("external watch checkpoint failed", "watch_id", watch.ID, "error", err)
	}
}

func runExternalWatchCommand(ctx context.Context, cwd, command string) (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = cwd
	bytes, err := cmd.CombinedOutput()
	return string(bytes), err
}

func matchesExternalWatchPattern(pattern, output string) bool {
	re, err := regexp.Compile(pattern)
	return err == nil && re.MatchString(output)
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
	summary, nextSteps := externalWatchOutcome(watch, status, output, lastError)
	taskStatus := "in_progress"
	if status != control.ExternalWatchSucceeded {
		taskStatus = "blocked"
	}
	if err := d.Control.UpdateTaskStatus(ctx, watch.TenantID, watch.TaskID, taskStatus, summary, nextSteps); err != nil {
		log.Warn("external watch task update failed", "watch_id", watch.ID, "error", err)
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"watch_id":    watch.ID,
		"description": watch.Description,
		"status":      status,
		"summary":     summary,
		"attempts":    watch.Attempts + 1,
	})
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID:     watch.TaskID,
		RunID:      watch.RunID,
		Type:       "external_watch.completed",
		Visibility: "task",
		Channel:    watch.Channel,
		Payload:    payload,
	})

	identity := &control.IdentityContext{TenantID: watch.TenantID, PersonID: watch.PersonID, Platform: "cli"}
	d.coordinator().routePendingNotification(ctx, identity, watch.Channel, delivery.Message{
		TenantID: watch.TenantID,
		PersonID: watch.PersonID,
		TaskID:   watch.TaskID,
		RunID:    watch.RunID,
		Content:  summary + "\n\nReply continue to resume the task.",
		Kind:     "external_watch",
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
		return fmt.Sprintf("%s did not reach a terminal state before the watch timed out.", description), []string{"Inspect the external service status, then continue or register a new watch."}
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
