package httpapi

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/platform/textutil"
)

func (d *Server) recoveryHandoffForRun(ctx context.Context, tenantID, personID, runID string) *control.RecoveryHandoff {
	if d == nil || d.Control == nil {
		return nil
	}
	handoff, err := d.Control.RecoveryHandoffForRun(ctx, tenantID, personID, runID)
	if err != nil || handoff == nil {
		return nil
	}
	if d.DisableAutomaticRunRecovery {
		handoff.Reason = "automatic_recovery_disabled"
		handoff.UnlockCondition = "Automatic recovery is disabled; review the durable evidence and resume explicitly when it is safe."
	}
	return handoff
}

func (d *Server) latestRecoveryHandoffForTask(ctx context.Context, identity *control.IdentityContext, taskID string) *control.RecoveryHandoff {
	if d == nil || d.Control == nil || identity == nil || strings.TrimSpace(taskID) == "" {
		return nil
	}
	runs, err := d.Control.ListTaskRuns(ctx, identity.TenantID, taskID, 10)
	if err != nil {
		return nil
	}
	for i := range runs {
		if runs[i].PersonID != identity.PersonID || runs[i].Status != "interrupted" {
			continue
		}
		if handoff := d.recoveryHandoffForRun(ctx, identity.TenantID, identity.PersonID, runs[i].ID); handoff != nil {
			return handoff
		}
	}
	return nil
}

func recoveryReasonText(reason string) string {
	switch strings.TrimSpace(reason) {
	case "automatic_recovery_disabled":
		return "automatic continuation is disabled"
	case "uncertain_effect_requires_observation":
		return "an external effect has an unknown outcome"
	case "known_effect_requires_user_resume":
		return "a recorded external effect requires explicit review"
	case "approval_recovery_owns_run", "specialist_origin":
		return "the exact approval flow owns this continuation"
	case "clarification_recovery_owns_run":
		return "the exact clarification flow owns this continuation"
	case "watcher_recovery_owns_run":
		return "the durable watcher owns this continuation"
	case "automatic_recovery_already_attempted":
		return "automatic recovery was already attempted once"
	case "historical_recovery_contract":
		return "this historical run keeps its original recovery semantics"
	case "parent_already_claimed":
		return "a child run already claimed this interruption"
	case "safe_pre_effect_interruption":
		return "the interruption happened before an external effect"
	case "missing_interruption_outcome", "invalid_interruption_outcome":
		return "the interruption record is incomplete"
	default:
		return "automatic continuation could not prove that another step is safe"
	}
}

func formatRecoveryHandoff(handoff *control.RecoveryHandoff) string {
	if handoff == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Recovery handoff:\n")
	if handoff.OriginalGoal != "" {
		fmt.Fprintf(&sb, "Original goal: %s\n", textutil.Truncate(toOneLine(handoff.OriginalGoal), 300))
	}
	if handoff.Cause != "" {
		fmt.Fprintf(&sb, "Cause: %s\n", textutil.Truncate(toOneLine(handoff.Cause), 120))
	}
	fmt.Fprintf(&sb, "Automatic continuation: not started — %s.\n", recoveryReasonText(handoff.Reason))
	if len(handoff.CompletedSteps) > 0 {
		sb.WriteString("Completed before interruption:\n")
		for _, step := range handoff.CompletedSteps[:min(len(handoff.CompletedSteps), 8)] {
			fmt.Fprintf(&sb, "- %s\n", textutil.Truncate(toOneLine(step), 180))
		}
	}
	if len(handoff.UnresolvedSteps) > 0 {
		sb.WriteString("Still unresolved:\n")
		for _, step := range handoff.UnresolvedSteps[:min(len(handoff.UnresolvedSteps), 8)] {
			fmt.Fprintf(&sb, "- %s\n", textutil.Truncate(toOneLine(step), 180))
		}
	}
	if len(handoff.UncertainEffects) > 0 {
		sb.WriteString("Uncertain effects — verify; do not replay:\n")
		for _, effect := range handoff.UncertainEffects[:min(len(handoff.UncertainEffects), 8)] {
			detail := effect.ToolName
			if effect.Strategy != "" {
				detail += " · " + effect.Strategy
			}
			if effect.EffectID != "" {
				detail += " · " + effect.EffectID
			}
			fmt.Fprintf(&sb, "- %s\n", textutil.Truncate(toOneLine(detail), 180))
		}
	}
	if len(handoff.AttemptedStrategies) > 0 {
		sb.WriteString("Attempted strategies:\n")
		for _, attempt := range handoff.AttemptedStrategies[:min(len(handoff.AttemptedStrategies), 8)] {
			strategy := fallback(attempt.Strategy, "unspecified")
			fmt.Fprintf(&sb, "- %s via %s · %s\n", strategy,
				textutil.Truncate(toOneLine(attempt.ToolName), 80), textutil.Truncate(toOneLine(attempt.Status), 40))
		}
	}
	if handoff.UnlockCondition != "" {
		fmt.Fprintf(&sb, "Unlock: %s\n", textutil.Truncate(toOneLine(handoff.UnlockCondition), 300))
	}
	if handoff.ResumePath != "" {
		fmt.Fprintf(&sb, "Resume: %s\n", handoff.ResumePath)
	}
	return strings.TrimSpace(sb.String())
}

func recoveryNotificationContent(title string, handoff *control.RecoveryHandoff) string {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "a task"
	}
	if handoff == nil {
		return fmt.Sprintf("SelfMind stopped while %q was running. Automatic continuation could not be verified.\n\nUse /task to inspect the durable task before resuming.", title)
	}
	return fmt.Sprintf("SelfMind stopped while %q was running and did not continue it automatically.\n\n%s", title, formatRecoveryHandoff(handoff))
}

func interruptedRunResponse(title string, outcome api.RunOutcome) (content, errorText string) {
	if outcome.Status != "interrupted" || !outcome.Resumable {
		return "", firstString(outcome.Risks)
	}
	if outcome.RecoveryScheduled {
		title = fallback(strings.TrimSpace(title), "the task")
		return fmt.Sprintf("SelfMind's model connection stopped while %q was running. One exact-parent recovery run is queued below new user work; completed steps and effect evidence remain durable. Use /task to inspect it.", title), ""
	}
	if outcome.Recovery != nil {
		return recoveryNotificationContent(title, outcome.Recovery), ""
	}
	return fallback(strings.TrimSpace(outcome.Summary), "The run was interrupted and requires review before resuming."), ""
}

// cancelDisabledRecoveryQueue is the claim-time half of the operational
// rollback. It first durably routes the structured handoff, then cancels the
// stale automatic row. A delivery failure leaves the row queued but inert so a
// later drain can retry the notice without ever launching the recovery child.
func (d *Server) cancelDisabledRecoveryQueue(ctx context.Context, identity *control.IdentityContext, queued *control.QueuedTask) bool {
	if d == nil || d.Control == nil || d.Delivery == nil || identity == nil || queued == nil ||
		queued.Class != control.QueueClassRecovery {
		return false
	}
	title := "a task"
	if task, err := d.Control.GetTask(ctx, queued.TenantID, queued.TaskID); err == nil && task != nil && task.PersonID == queued.PersonID {
		title = task.Title
	}
	route := d.routeIdentityForPerson(ctx, queued.TenantID, queued.PersonID, queued.Channel, queued.Platform, identity)
	handoff := d.recoveryHandoffForRun(ctx, queued.TenantID, queued.PersonID, queued.ReplyToRunID)
	// liveSurfaceInformed=true keeps recovery on its existing routing: this push
	// IS the deduplicated recovery notification, not a fallback for a live event
	// that may have failed.
	if !d.coordinator().routePendingNotification(ctx, route, queued.Channel, delivery.Message{
		TenantID: queued.TenantID, PersonID: queued.PersonID, TaskID: queued.TaskID, RunID: queued.ReplyToRunID,
		Content: recoveryNotificationContent(title, handoff), Kind: "recovery",
	}, true) {
		return false
	}
	return d.Control.MarkQueued(ctx, queued.TenantID, queued.ID, control.QueueStatusCancelled) == nil
}
