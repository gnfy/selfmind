package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/gateway/api"
	"selfmind/internal/modelchange"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

func (d *Server) handleModelControl(ctx context.Context, source, commandText string) (string, error) {
	if d == nil || d.ModelChanges == nil {
		if d != nil && d.Gateway != nil {
			return d.Gateway.ModelStatusReply(), nil
		}
		return "SelfMind is running, but the model gateway is not configured.", nil
	}
	fields := strings.Fields(strings.TrimSpace(commandText))
	if len(fields) != 1 {
		return "Usage: /model", nil
	}
	status, err := d.ModelChanges.Inspect()
	if err != nil {
		return "", err
	}
	return formatModelStatus(status), nil
}

func (d *Server) scheduleModelRestart(changeID string) (bool, error) {
	if d == nil || d.ModelRestartFunc == nil || d.ModelChanges == nil {
		return false, fmt.Errorf("restart helper is unavailable")
	}
	claimed, _, err := d.ModelChanges.ClaimRestart(changeID)
	if err != nil {
		return false, err
	}
	if !claimed {
		return true, nil
	}
	// The helper is spawned synchronously so launch failures are visible. It
	// performs its own short delay before asking this daemon to drain, allowing
	// the current HTTP/IM response to leave the process first.
	if err := d.ModelRestartFunc(changeID); err != nil {
		_ = d.ModelChanges.ReleaseRestartClaim(changeID, err)
		return false, err
	}
	return true, nil
}

func normalizeManagedModelRoute(value string) (modelchange.Route, error) {
	route := modelchange.Route(strings.ToLower(strings.TrimSpace(value)))
	if route == "background" {
		route = modelchange.RouteAuxiliary
	}
	if route == modelchange.RoutePrimary || route == modelchange.RouteAuxiliary || modelchange.IsManagedRoleRoute(route) {
		return route, nil
	}
	return "", fmt.Errorf("unsupported model route %q", value)
}

func buildModelDraft(service *modelchange.Service, req api.ModelChangeRequest) (modelchange.Snapshot, int64, []string, error) {
	status, err := service.Inspect()
	if err != nil {
		return modelchange.Snapshot{}, 0, nil, err
	}
	patches := append([]api.ModelSelectionPatch(nil), req.Patches...)
	if len(patches) == 0 && strings.TrimSpace(req.Route) != "" {
		patches = []api.ModelSelectionPatch{{
			Route: req.Route, Provider: req.Provider, Model: req.Model,
			Reasoning: req.Reasoning, ServiceTier: req.ServiceTier,
		}}
	}
	if len(patches) == 0 {
		return modelchange.Snapshot{}, 0, nil, fmt.Errorf("at least one model selection is required")
	}
	candidate := status.Configured
	var notices []string
	for _, patch := range patches {
		route, routeErr := normalizeManagedModelRoute(patch.Route)
		if routeErr != nil {
			return modelchange.Snapshot{}, 0, nil, routeErr
		}
		if patch.Reset {
			reset, resetErr := modelchange.ResetRoleCandidate(candidate, route)
			if resetErr != nil {
				return modelchange.Snapshot{}, 0, nil, resetErr
			}
			candidate = reset.Snapshot
			continue
		}
		reasoning := modelchange.OptionalValue{}
		if patch.Reasoning != nil {
			reasoning = modelchange.OptionalValue{Set: true, Value: *patch.Reasoning}
		}
		serviceTier := modelchange.OptionalValue{}
		if patch.ServiceTier != nil {
			serviceTier = modelchange.OptionalValue{Set: true, Value: *patch.ServiceTier}
		}
		built, buildErr := modelchange.BuildCandidate(candidate, modelchange.SelectionPatch{
			Route: route, Provider: patch.Provider, Model: patch.Model,
			Reasoning: reasoning, ServiceTier: serviceTier, Enabled: patch.Enabled,
		})
		if buildErr != nil {
			return modelchange.Snapshot{}, 0, nil, buildErr
		}
		candidate = built.Snapshot
		notices = append(notices, built.Notices...)
	}
	generation := req.ExpectedGeneration
	if generation == 0 {
		generation = status.Generation
	}
	return candidate, generation, notices, nil
}

func modelDraftCredentials(req api.ModelChangeRequest) map[string]string {
	credentials := make(map[string]string)
	for _, patch := range req.Patches {
		provider := modelruntime.NormalizeProviderID(patch.Provider)
		if provider == "" || strings.TrimSpace(patch.APIKey) == "" {
			continue
		}
		credentials[provider] = strings.TrimSpace(patch.APIKey)
	}
	return credentials
}

func buildProviderDraft(service *modelchange.Service, req api.ModelChangeRequest) ([]modelchange.ProviderChange, error) {
	if len(req.ProviderPatches) == 0 {
		return nil, nil
	}
	if service == nil {
		return nil, fmt.Errorf("model management is unavailable")
	}
	cfg, err := config.LoadConfig(config.Options{Path: service.ConfigPath})
	if err != nil {
		return nil, err
	}
	return modelchange.BuildProviderChanges(cfg, req.ProviderPatches)
}

func applyModelDraftCredentials(cfg *config.Config, credentials map[string]string) error {
	if cfg == nil {
		return fmt.Errorf("config is required")
	}
	for provider, apiKey := range credentials {
		if _, custom := cfg.Providers.CustomProvider(provider); custom {
			name := modelruntime.NormalizeProviderID(strings.TrimPrefix(provider, "custom:"))
			for index := range cfg.Providers.Custom {
				if modelruntime.NormalizeProviderID(cfg.Providers.Custom[index].Name) == name {
					cfg.Providers.Custom[index].APIKey = apiKey
				}
			}
			continue
		}
		endpoint, _ := cfg.Providers.BuiltinEndpoint(provider)
		endpoint.APIKey = apiKey
		cfg.Providers.SetBuiltinEndpoint(provider, endpoint)
	}
	return nil
}

func modelProbesPassed(probes []modelchange.ProbeResult) bool {
	if len(probes) == 0 {
		return false
	}
	for _, probe := range probes {
		if !probe.OK {
			return false
		}
	}
	return true
}

func formatModelStatus(status modelchange.Status) string {
	var out strings.Builder
	fmt.Fprintln(&out, "Model routes")
	fmt.Fprintf(&out, "Foreground readiness: %s\n", readinessLabel(status.ForegroundReady(), status.Readiness.ForegroundReason))
	if status.Readiness.BackgroundEnabled {
		fmt.Fprintf(&out, "Background readiness: %s\n", readinessLabel(status.BackgroundReady(), status.Readiness.BackgroundReason))
	} else {
		fmt.Fprintln(&out, "Background readiness: disabled")
	}
	formatRouteLine(&out, "Running primary", status.Running.Primary)
	formatRouteLine(&out, "Running background", status.Running.Auxiliary)
	if status.Configured != status.Running {
		formatRouteLine(&out, "Configured primary", status.Configured.Primary)
		formatRouteLine(&out, "Configured background", status.Configured.Auxiliary)
	}
	if status.Pending != nil {
		fmt.Fprintf(&out, "Pending: %s (%s), routes=%s\n", status.Pending.ID, status.Pending.Status, joinModelRoutes(status.Pending.ChangedRoutes))
		if !status.Pending.ConfirmBy.IsZero() && status.Pending.Status == modelchange.StatusAwaitingConfirmation {
			fmt.Fprintf(&out, "Confirm by: %s\n", status.Pending.ConfirmBy.Local().Format(time.RFC3339))
		}
	}
	for _, route := range modelchange.ManagedRoleRoutes() {
		selection := modelchange.SelectionForRoute(status.Configured, route)
		if strings.TrimSpace(selection.Provider) == "" && strings.TrimSpace(selection.Model) == "" {
			fmt.Fprintf(&out, "%s: uses background model · %s\n", route,
				readinessLabel(status.RouteReady(route), status.RouteReadiness(route).Reason))
			continue
		}
		formatRouteLine(&out, string(route), selection)
		fmt.Fprintf(&out, "%s readiness: %s\n", route,
			readinessLabel(status.RouteReady(route), status.RouteReadiness(route).Reason))
	}
	fmt.Fprintf(&out, "Generation: %d\n", status.Generation)
	fmt.Fprint(&out, "Open the interactive Model Manager with `selfmind model` or bare `/model` in the TUI.")
	return strings.TrimSpace(out.String())
}

func readinessLabel(ready bool, reason string) string {
	if ready {
		return "ready"
	}
	if reason = strings.TrimSpace(reason); reason != "" {
		return "not ready — " + reason
	}
	return "not ready"
}

func formatRouteLine(out *strings.Builder, label string, selection config.ModelSelectionConfig) {
	if selection.Enabled != nil && !*selection.Enabled {
		fmt.Fprintf(out, "%s: disabled\n", label)
		return
	}
	fmt.Fprintf(out, "%s: %s/%s reasoning=%s", label, dash(selection.Provider), dash(selection.Model), auto(selection.Reasoning))
	if strings.TrimSpace(selection.ServiceTier) != "" {
		fmt.Fprintf(out, " service_tier=%s", selection.ServiceTier)
	}
	fmt.Fprintln(out)
}

func joinModelRoutes(routes []modelchange.Route) string {
	parts := make([]string, 0, len(routes))
	for _, route := range modelchange.SortedRoutes(routes) {
		parts = append(parts, string(route))
	}
	return strings.Join(parts, ",")
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return strings.TrimSpace(value)
}

func auto(value string) string {
	if strings.TrimSpace(value) == "" {
		return "auto"
	}
	return strings.TrimSpace(value)
}
