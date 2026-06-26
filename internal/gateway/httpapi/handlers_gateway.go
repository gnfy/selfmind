package httpapi

import (
	"context"
	"net/http"
	"time"

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
	active := d.coordinator().activeRunStatuses()
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
		d.coordinator().stopAllActive("gateway shutdown")
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
	coord := d.coordinator()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if len(coord.activeRunStatuses()) == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return len(coord.activeRunStatuses()) == 0
		case <-ticker.C:
		}
	}
}

func (d *Server) drainTimeout() time.Duration {
	if d.DrainTimeout > 0 {
		return d.DrainTimeout
	}
	return 30 * time.Second
}
