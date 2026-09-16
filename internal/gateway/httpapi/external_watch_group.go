package httpapi

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/platform/log"
)

// externalWatchAggregate is shared by live completion, crash compensation,
// queue reconciliation and delivery. Only the durable winning member owns
// the aggregate's products, including when recovery follows a committed verdict.
func (d *Server) externalWatchAggregate(ctx context.Context, watch control.ExternalWatch) (control.ExternalWatch, bool) {
	if strings.TrimSpace(watch.WaitGroupID) == "" {
		return watch, true
	}
	group, err := d.Control.ResolveExternalWatchGroup(ctx, watch.TenantID, watch.WaitGroupID, watch.ID)
	if err != nil {
		log.Warn("external watch group resolution failed", "watch_id", watch.ID, "error", err)
		return watch, false
	}
	if !group.Terminal || group.Group.WinnerWatchID != watch.ID {
		return watch, false
	}
	watch.Status = group.Status
	if group.Status == control.ExternalWatchBlocked {
		watch.LastError = parkedWatchReason(watchReasonInvalidCheck, "wait group is missing declared members; no aggregate external outcome was established", "")
	}
	watch.Description = fmt.Sprintf("wait group %s (%s)", group.Group.GroupKey, group.Group.Mode)
	if group.Status == control.ExternalWatchFailed && strings.TrimSpace(watch.LastError) == "" {
		watch.LastError = "the aggregate wait-group condition could not be satisfied"
	}
	_, err = d.Control.AppendEvent(ctx, control.Event{
		TaskID: watch.TaskID, RunID: watch.RunID, Type: "external_watch.group_resolved",
		Visibility: "task", Channel: watch.Channel,
		Payload: mustJSON(map[string]interface{}{
			"group_id": group.Group.ID, "group_key": group.Group.GroupKey,
			"mode": group.Group.Mode, "status": group.Status, "winner_watch_id": watch.ID,
		}),
		IdempotencyKey: "external-watch-group-resolved:" + group.Group.ID,
	})
	if err != nil {
		log.Warn("external watch group event failed", "watch_id", watch.ID, "error", err)
		return watch, false
	}
	return watch, true
}
