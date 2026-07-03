package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

// handleRunSteer (POST /v1/runs/steer) forwards mid-turn user guidance into
// the caller's active run. It exists for thin clients (the default TUI path):
// their process-local steering channel can never reach a run executing inside
// the daemon, so without this endpoint mid-run input would be silently
// dropped. Identity resolution mirrors handleApprovalRespond.
func (d *Server) handleRunSteer(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req api.RunSteerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	identity, err := d.Control.ResolveOrCreateAccount(
		r.Context(),
		d.tenantID(req.TenantID),
		fallback(req.Platform, "cli"),
		fallback(req.PlatformUserID, "local"),
		req.DisplayName,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	active := d.coordinator().currentActive(identity.PersonID)
	if active == nil || active.Steer == nil {
		// 409: nothing to steer — the run may have finished between the user's
		// keystroke and this request. Clients must surface this honestly.
		writeError(w, http.StatusConflict, fmt.Errorf("no active run to steer"))
		return
	}
	// Non-blocking hand-off: the agent loop drains at iteration boundaries, so
	// a full buffer means guidance is arriving faster than the loop consumes.
	// Report back-pressure instead of blocking the handler or dropping text.
	select {
	case active.Steer <- text:
	default:
		writeError(w, http.StatusTooManyRequests, fmt.Errorf("steering buffer is full; try again in a moment"))
		return
	}
	appendRunSteeredEvent(r.Context(), d.Control, active, fallback(req.Channel, active.Channel), text)
	writeJSON(w, http.StatusOK, api.RunSteerResponse{Identity: identity, Accepted: true})
}

// appendRunSteeredEvent records that guidance entered the run, with a bounded
// preview only (never the raw full text — events are durable context). Skipped
// when the run has not resolved its task yet: TaskID is assigned after
// StartRun, and an event without a task cannot be attached anywhere.
func appendRunSteeredEvent(ctx context.Context, store *control.Store, active *activeRun, channel, text string) {
	if store == nil || active == nil || active.TaskID == "" {
		return
	}
	_, _ = store.AppendEvent(ctx, control.Event{
		TaskID:     active.TaskID,
		RunID:      active.RunID,
		Type:       "run.steered",
		Visibility: "task",
		Channel:    channel,
		Payload:    mustJSON(map[string]string{"text": truncate(text, 120), "channel": channel}),
	})
}
