package httpapi

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/log"
	"selfmind/internal/promptassets"
	"selfmind/internal/tools"
)

func (d *Server) blockPromptRevisionJob(ctx context.Context, tenantID, runID string, version int, err error) bool {
	if d == nil || d.Control == nil || !promptassets.IsRevisionUnavailable(err) {
		return false
	}
	// A drain may cancel the worker context after the model call has already
	// exposed the missing revision. Persist the durable transition independently
	// so the row does not remain running and later lose its real failure reason.
	blockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	blocked, blockErr := d.Control.BlockMaintenanceJobForPromptRevision(blockCtx, tenantID, runID, version, err.Error())
	if blockErr != nil {
		log.Warn("gateway: block missing prompt revision", "run", runID, "error", blockErr)
		return false
	}
	return blocked
}

const (
	maintenanceBlockedNoticeSetting = "maintenance_provider_blocked_notice_at"
	maintenanceBlockedNoticeEvery   = 6 * time.Hour
)

// blockMaintenanceProviderJob is the fatal-provider half of post-run
// maintenance. The user's completed run remains untouched; only background
// label/memory learning pauses until provider configuration is repaired.
func (d *Server) blockMaintenanceProviderJob(ctx context.Context, identity *control.IdentityContext, task *control.Task, run *control.Run, version int, providerErr error) {
	if d == nil || d.Control == nil || identity == nil || task == nil || run == nil || providerErr == nil {
		return
	}
	info, _ := llm.ProviderErrorInfo(providerErr)
	blocked, err := d.Control.BlockMaintenanceJobForRoute(ctx, identity.TenantID, run.ID, version, info.RouteID, providerErr.Error())
	if err != nil || !blocked {
		return
	}
	lastNotice, _ := d.Control.GetPersonSetting(ctx, identity.TenantID, identity.PersonID, maintenanceBlockedNoticeSetting)
	if unix, parseErr := strconv.ParseInt(strings.TrimSpace(lastNotice), 10, 64); parseErr == nil && time.Since(time.Unix(unix, 0)) < maintenanceBlockedNoticeEvery {
		return
	}
	reason := truncate(toOneLine(tools.RedactSensitive(providerErr.Error())), 180)
	payload, _ := json.Marshal(map[string]interface{}{
		"status": "blocked_provider",
		"reason": reason,
	})
	_, _ = d.Control.AppendEvent(ctx, control.Event{
		TaskID: task.ID, RunID: run.ID, Type: "maintenance.blocked", Visibility: "task", Channel: run.Channel, Payload: payload,
	})
	message := "Background learning is paused because its model provider is unavailable. Your task result is safe and normal work can continue."
	if reason != "" {
		message += "\n\n" + reason
	}
	message += "\n\nRun /diag or selfmind doctor after updating the provider."
	// liveSurfaceInformed=true keeps this provider-health push on its existing
	// routing. It is not one of the human-wait events, so the new rule does not
	// apply; whether an attached CLI should still be pushed here is a separate
	// question from the approval/clarify visibility fix.
	d.coordinator().routePendingNotification(ctx, identity, run.Channel, delivery.Message{
		TenantID: identity.TenantID,
		PersonID: identity.PersonID,
		TaskID:   task.ID,
		RunID:    run.ID,
		Content:  message,
		Kind:     "maintenance_health",
	}, true)
	_ = d.Control.SetPersonSetting(ctx, identity.TenantID, identity.PersonID, maintenanceBlockedNoticeSetting, strconv.FormatInt(time.Now().Unix(), 10))
}
