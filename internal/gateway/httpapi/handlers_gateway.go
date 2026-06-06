package httpapi

import (
	"context"
	"net/http"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
)

func (d *Server) handleGatewayStatus(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, d.GatewayStatus())
}

func (d *Server) handleGatewayShutdown(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	started := d.RequestGatewayShutdown(d.drainTimeout(), "api shutdown")
	status := http.StatusAccepted
	if !started {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]interface{}{
		"accepted": started,
		"status":   d.GatewayStatus(),
	})
}

func (d *Server) GatewayStatus() api.GatewayStatusResponse {
	runtime := api.GatewayRuntimeInfo{State: d.GatewayState()}
	if d.RuntimeStatusFunc != nil {
		runtime = d.RuntimeStatusFunc()
	}
	active := d.activeRunStatuses()
	state, draining, reason := d.gatewayStateParts()
	if runtime.State == "" {
		runtime.State = state
	}
	return api.GatewayStatusResponse{
		Runtime:        runtime,
		State:          state,
		Draining:       draining,
		DrainReason:    reason,
		ActiveRuns:     active,
		ActiveRunCount: len(active),
	}
}

func (d *Server) GatewayState() string {
	state, _, _ := d.gatewayStateParts()
	return state
}

func (d *Server) gatewayStateParts() (string, bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.draining {
		return "draining", true, d.drainReason
	}
	return "running", false, ""
}

func (d *Server) IsDraining() bool {
	_, draining, _ := d.gatewayStateParts()
	return draining
}

func (d *Server) RequestGatewayShutdown(timeout time.Duration, reason string) bool {
	started := false
	d.shutdownOnce.Do(func() {
		started = true
		go d.shutdownAfterDrain(timeout, reason)
	})
	return started
}

func (d *Server) shutdownAfterDrain(timeout time.Duration, reason string) {
	if timeout <= 0 {
		timeout = d.drainTimeout()
	}
	d.beginDraining(reason)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if !d.waitForIdle(ctx) {
		d.stopAllActive("gateway shutdown")
	}
	if d.ShutdownFunc != nil {
		d.ShutdownFunc()
	}
}

func (d *Server) beginDraining(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.draining = true
	d.drainReason = reason
}

func (d *Server) waitForIdle(ctx context.Context) bool {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if len(d.activeRunStatuses()) == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return len(d.activeRunStatuses()) == 0
		case <-ticker.C:
		}
	}
}

func (d *Server) stopAllActive(reason string) {
	d.mu.Lock()
	var runs []*activeRun
	for _, active := range d.active {
		copy := *active
		runs = append(runs, &copy)
		if active.Cancel != nil {
			active.Cancel()
		}
	}
	d.mu.Unlock()

	for _, active := range runs {
		if active.RunID != "" && d.Control != nil {
			_ = d.Control.RequestRunCancel(context.Background(), active.TenantID, active.RunID)
			_ = d.Control.FinishRun(context.Background(), active.TenantID, active.RunID, "cancelled")
		}
		if active.TaskID != "" && d.Control != nil {
			_ = d.Control.UpdateTaskStatus(context.Background(), active.TenantID, active.TaskID, "cancelled", reason, nil)
			_, _ = d.Control.AppendEvent(context.Background(), control.Event{
				TaskID:     active.TaskID,
				RunID:      active.RunID,
				Type:       "run.cancelled",
				Visibility: "task",
				Channel:    active.Channel,
				Payload:    mustJSON(map[string]string{"reason": reason}),
			})
		}
	}
}

func (d *Server) activeRunStatuses() []api.ActiveRunStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.active) == 0 {
		return nil
	}
	statuses := make([]api.ActiveRunStatus, 0, len(d.active))
	for _, active := range d.active {
		if active == nil {
			continue
		}
		status := formatActiveRunStatus(activeRunCopy(active))
		if status != nil {
			statuses = append(statuses, *status)
		}
	}
	return statuses
}

func activeRunCopy(active *activeRun) *activeRun {
	if active == nil {
		return nil
	}
	copy := *active
	return &copy
}

func (d *Server) drainTimeout() time.Duration {
	if d.DrainTimeout > 0 {
		return d.DrainTimeout
	}
	return 30 * time.Second
}
