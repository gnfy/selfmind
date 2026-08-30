package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"selfmind/internal/buildinfo"
	"selfmind/internal/executionenv"
	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelchange"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
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

func (d *Server) handleGatewayToolCatalogProbe(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !d.localCLIRequest(r, "cli") {
		http.Error(w, "provider catalogue probe is available only to the authenticated local CLI", http.StatusForbidden)
		return
	}
	if d.ToolCatalogProbeFunc == nil {
		http.Error(w, "provider catalogue probe is unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, d.ToolCatalogProbeFunc(r.Context()))
}

func (d *Server) handleGatewayModelProbe(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !d.localCLIRequest(r, "cli") {
		http.Error(w, "model probe is available only to the authenticated local CLI", http.StatusForbidden)
		return
	}
	if d.ModelProbeFunc == nil {
		http.Error(w, "model probe is unavailable", http.StatusServiceUnavailable)
		return
	}
	var req api.ModelProbeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	role := strings.ToLower(strings.TrimSpace(req.Role))
	if role != "primary" && role != "auxiliary" {
		http.Error(w, "role must be primary or auxiliary", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, d.ModelProbeFunc(r.Context(), role))
}

func (d *Server) handleGatewayModelChange(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !d.localCLIRequest(r, "cli") {
		http.Error(w, "model changes through this endpoint require the authenticated local CLI", http.StatusForbidden)
		return
	}
	if d.ModelChanges == nil {
		http.Error(w, "model management is unavailable", http.StatusServiceUnavailable)
		return
	}
	var req api.ModelChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	result := api.ModelChangeResponse{ProtocolVersion: api.ModelControlProtocolVersion}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	switch action {
	case "status":
		status, err := d.ModelChanges.Inspect()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		result.Status = &status
	case "validate":
		candidate, _, notices, err := buildModelDraft(d.ModelChanges, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		routes := make([]modelchange.Route, 0, len(req.ValidateRoutes))
		for _, value := range req.ValidateRoutes {
			route, routeErr := normalizeManagedModelRoute(value)
			if routeErr != nil {
				http.Error(w, routeErr.Error(), http.StatusBadRequest)
				return
			}
			routes = append(routes, route)
		}
		credentials := modelDraftCredentials(req)
		providerChanges, providerErr := buildProviderDraft(d.ModelChanges, req)
		if providerErr != nil {
			http.Error(w, providerErr.Error(), http.StatusBadRequest)
			return
		}
		cfg, loadErr := config.LoadConfig(config.Options{Path: d.ModelChanges.ConfigPath})
		if loadErr != nil {
			http.Error(w, loadErr.Error(), http.StatusInternalServerError)
			return
		}
		store := modelruntime.NewCredentialStore(cfg.Auth.CredentialsFile)
		probes, err := d.ModelChanges.ValidateCandidateWithConfig(r.Context(), candidate, routes, func(cfg *config.Config) error {
			modelchange.ApplyProviderChanges(cfg, providerChanges, true)
			if err := store.OverlayStage(req.CredentialStage, cfg); err != nil {
				return err
			}
			return applyModelDraftCredentials(cfg, credentials)
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result.CredentialStage = strings.TrimSpace(req.CredentialStage)
		if modelProbesPassed(probes) && len(credentials) > 0 {
			stage, stageErr := store.StageAPIKeys(result.CredentialStage, credentials)
			if stageErr != nil {
				http.Error(w, stageErr.Error(), http.StatusInternalServerError)
				return
			}
			result.CredentialStage = stage
		}
		result.Notices = notices
		result.Probes = probes
	case "prepare", "apply":
		candidate, generation, notices, err := buildModelDraft(d.ModelChanges, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		providerChanges, providerErr := buildProviderDraft(d.ModelChanges, req)
		if providerErr != nil {
			http.Error(w, providerErr.Error(), http.StatusBadRequest)
			return
		}
		prepare := modelchange.PrepareRequest{
			Candidate: candidate, ExpectedGeneration: generation,
			ReplacePending: req.ReplacePending, ForceRevalidate: true,
			CredentialStage: req.CredentialStage,
			ProviderChanges: providerChanges,
		}
		prepare.Source = "local-cli"
		prepare.RequireConfirmation = action == "prepare"
		prepared, err := d.ModelChanges.Prepare(r.Context(), prepare)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result.Change = &prepared.Change
		result.Notices = notices
		result.NeedsConfirm = prepared.NeedsConfirm
		result.NeedsRestart = prepared.NeedsRestart
		if prepared.NeedsRestart {
			if _, restartErr := d.scheduleModelRestart(prepared.Change.ID); restartErr != nil {
				result.Notices = append(result.Notices, "automatic restart was not scheduled: "+restartErr.Error()+"; run `selfmind gateway restart --drain`")
			} else {
				result.RestartScheduled = true
			}
		}
	case "confirm":
		prepared, err := d.ModelChanges.Confirm(r.Context(), req.ChangeID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result.Change = &prepared.Change
		result.NeedsRestart = prepared.NeedsRestart
		if _, restartErr := d.scheduleModelRestart(prepared.Change.ID); restartErr != nil {
			result.Notices = append(result.Notices, "automatic restart was not scheduled: "+restartErr.Error()+"; run `selfmind gateway restart --drain`")
		} else {
			result.RestartScheduled = true
		}
	case "cancel":
		if err := d.ModelChanges.Cancel(req.ChangeID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if status, inspectErr := d.ModelChanges.Inspect(); inspectErr == nil && status.ModelReady() {
			d.drainQueuedWhenReady(context.Background())
		}
	case "rollback":
		prepared, err := d.ModelChanges.PrepareRollback(r.Context(), "local-cli", true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result.Change = &prepared.Change
		result.NeedsConfirm = prepared.NeedsConfirm
		result.NeedsRestart = prepared.NeedsRestart
	default:
		http.Error(w, "action must be status, validate, prepare, apply, confirm, cancel, or rollback", http.StatusBadRequest)
		return
	}
	statusCode := http.StatusOK
	if result.RestartScheduled && result.NeedsRestart {
		statusCode = http.StatusAccepted
	}
	writeJSON(w, statusCode, result)
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
	var payload struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&payload)
	reason := strings.TrimSpace(payload.Reason)
	if reason == "" {
		reason = "api shutdown"
	}
	if len(reason) > 160 {
		reason = reason[:160]
	}
	if _, ok := modelChangeShutdownID(reason); ok && d.ModelChanges == nil {
		http.Error(w, "model change service is unavailable", http.StatusConflict)
		return
	}
	drainTimeout := shutdownTimeoutForReason(d.drainTimeout(), reason)
	responseReady := make(chan struct{})
	started := d.requestGatewayShutdown(drainTimeout, reason, responseReady)
	if started {
		defer close(responseReady)
	}
	status := http.StatusAccepted
	if !started {
		status = http.StatusOK
	}
	_ = writeJSONFlushed(w, status, map[string]interface{}{
		"accepted": started,
		"status":   d.GatewayStatus(),
	})
}

func modelChangeShutdownID(reason string) (string, bool) {
	const prefix = "model_change:"
	reason = strings.TrimSpace(reason)
	if !strings.HasPrefix(strings.ToLower(reason), prefix) {
		return "", false
	}
	id := strings.TrimSpace(reason[len(prefix):])
	return id, id != ""
}

func shutdownTimeoutForReason(defaultTimeout time.Duration, reason string) time.Duration {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(reason)), "model_change:") {
		// Zero denotes that the model-change coordinator owns an unbounded safe
		// boundary wait. shutdownAfterDrain never converts it into a forced stop.
		return 0
	}
	return defaultTimeout
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
	build := buildinfo.Current()
	runtime.Version = build.Version
	runtime.Commit = build.Commit
	runtime.BuiltAt = build.BuiltAt
	runtime.BuildFingerprint = build.Fingerprint
	if snapshot := executionenv.DefaultRegistry().Current(); snapshot != nil {
		runtime.EnvironmentGeneration = snapshot.Generation
		runtime.EnvironmentSnapshotID = snapshot.ID
		runtime.PrincipalFingerprint = snapshot.PrincipalFingerprint
		runtime.EnvironmentFingerprint = snapshot.EnvironmentFingerprint
		runtime.CredentialSourceHash = snapshot.CredentialSourceHash
	}
	toolSchemas := api.ToolSchemaHealth{}
	if d.ToolSchemaReportFunc != nil {
		for _, report := range d.ToolSchemaReportFunc() {
			switch report.Status {
			case tools.ToolSchemaRepaired:
				toolSchemas.Repaired++
				toolSchemas.RegisteredActive++
			case tools.ToolSchemaQuarantined:
				toolSchemas.Quarantined++
			default:
				toolSchemas.RegisteredActive++
			}
			if report.Status != tools.ToolSchemaQuarantined && report.Exposure == tools.ToolExposureHidden {
				toolSchemas.Hidden++
			}
		}
	}
	toolSchemas.Active = toolSchemas.RegisteredActive
	toolCatalog := llm.ToolCatalogPreview{}
	if d.ToolCatalogPreviewFunc != nil {
		toolCatalog = d.ToolCatalogPreviewFunc(context.Background())
	}
	toolSchemas.ProviderVisible = toolCatalog.Count
	mcpHealth := api.MCPHealth{}
	if d.MCPHealthFunc != nil {
		health := d.MCPHealthFunc()
		mcpHealth.Configured = health.Configured
		mcpHealth.Connected = health.Connected
		mcpHealth.Failed = health.Failed
		for _, failure := range health.Failures {
			mcpHealth.Failures = append(mcpHealth.Failures, api.MCPServerFailure{Name: failure.Name, Error: failure.Error})
		}
	}
	storeSchema := api.StoreSchemaHealth{}
	if d.Control != nil {
		status := d.Control.SchemaStatus()
		storeSchema.Version = status.Version
		storeSchema.CurrentVersion = status.CurrentVersion
		storeSchema.BackupCreated = status.MigrationBackup != ""
	}
	return api.GatewayStatusResponse{
		Runtime:        runtime,
		State:          state,
		Draining:       draining,
		DrainReason:    reason,
		ActiveRuns:     active,
		ActiveRunCount: len(active),
		ToolSchemas:    toolSchemas,
		ToolCatalog:    toolCatalog,
		MCP:            mcpHealth,
		StoreSchema:    storeSchema,
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

// ActiveRunCount is the shutdown boundary's lightweight read surface. It lets
// the process owner distinguish long-lived observer connections from real work
// without rebuilding the full diagnostic status payload.
func (d *Server) ActiveRunCount() int {
	return len(d.coordinator().activeRunStatuses())
}

func (d *Server) RequestGatewayShutdown(timeout time.Duration, reason string) bool {
	return d.requestGatewayShutdown(timeout, reason, nil)
}

// requestGatewayShutdown optionally waits for the HTTP acceptance response to
// be fully written before the process owner is told to close the server. The
// local shutdown endpoint uses this gate because a no-work drain completes in
// the same scheduler turn; without it, runner's immediate Server.Close can cut
// off the 202 response and leave the restart client with EOF after a successful
// shutdown. Signal- and context-originated shutdowns pass nil and start at once.
func (d *Server) requestGatewayShutdown(timeout time.Duration, reason string, responseReady <-chan struct{}) bool {
	d.mu.Lock()
	if d.shutdownPending {
		// Replacing a candidate before the safe boundary keeps the existing
		// drain coordinator but updates the reason shown to clients. The
		// coordinator reads the current pending transaction once the run is idle.
		if _, ok := modelChangeShutdownID(reason); ok {
			d.drainReason = reason
		}
		d.mu.Unlock()
		return false
	}
	d.shutdownPending = true
	d.draining = true
	d.drainReason = reason
	d.mu.Unlock()
	go func() {
		if responseReady != nil {
			<-responseReady
		}
		d.shutdownAfterDrain(timeout, reason)
	}()
	return true
}

func (d *Server) shutdownAfterDrain(timeout time.Duration, reason string) {
	if _, modelChange := modelChangeShutdownID(reason); modelChange {
		if !d.waitForModelSafeBoundary() {
			d.cancelPendingShutdown()
			d.DrainQueuedAtBoot(context.Background())
			return
		}
		status, err := d.ModelChanges.Inspect()
		if err != nil || status.Pending == nil {
			d.cancelPendingShutdown()
			d.DrainQueuedAtBoot(context.Background())
			return
		}
		if _, err := d.ModelChanges.BeginDraining(status.Pending.ID); err != nil {
			_, _ = d.ModelChanges.MarkRecoveryRequired(status.Pending.ID, err)
			d.cancelPendingShutdown()
			return
		}
		if d.ShutdownFunc != nil {
			d.ShutdownFunc()
		}
		return
	}
	if timeout <= 0 {
		timeout = d.drainTimeout()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if !d.waitForIdle(ctx) {
		if strings.EqualFold(strings.TrimSpace(reason), api.ShutdownReasonServiceReconcile) {
			d.cancelPendingShutdown()
			d.DrainQueuedAtBoot(context.Background())
			return
		}
		d.coordinator().stopAllActive("gateway shutdown")
	}
	if d.ShutdownFunc != nil {
		d.ShutdownFunc()
	}
}

func (d *Server) waitForModelSafeBoundary() bool {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := d.ModelChanges.Inspect()
		if err != nil || status.Pending == nil {
			return false
		}
		switch status.Pending.Status {
		case modelchange.StatusAwaitingSafeBoundary, modelchange.StatusValidating:
			// still cancellable/replacable
		case modelchange.StatusCommitting, modelchange.StatusDraining, modelchange.StatusRestarting:
			// a crash-recovered helper may already have crossed the boundary
		default:
			return false
		}
		if len(d.coordinator().activeRunStatuses()) == 0 {
			return true
		}
		<-ticker.C
	}
}

func (d *Server) cancelPendingShutdown() {
	d.mu.Lock()
	d.draining = false
	d.drainReason = ""
	d.shutdownPending = false
	d.mu.Unlock()
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
