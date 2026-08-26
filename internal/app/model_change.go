package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelchange"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// ValidateModelChange runs the same role-aware contract probes used by setup
// and doctor, but against an in-memory candidate configuration. It returns one
// bounded result per changed route and never exposes credentials.
func ValidateModelChange(ctx context.Context, cfg *config.Config, routes []modelchange.Route) []modelchange.ProbeResult {
	routes = expandedModelValidationRoutes(cfg, routes)
	results := make([]modelchange.ProbeResult, 0, len(routes))
	seen := make(map[string]struct{})
	for _, route := range routes {
		runtime, err := ResolveModelRuntime(ctx, cfg, string(route))
		if err != nil {
			results = append(results, modelchange.ProbeResult{
				Route: route, Error: tools.RedactSensitive(err.Error()),
				FailureClass: classifyModelProbeFailure(err),
			})
			continue
		}
		contract := "foreground"
		if route != modelchange.RoutePrimary {
			contract = "background_text"
		}
		if isMaintenanceProbeRole(string(route)) {
			contract = "maintenance_json"
		}
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s", runtime.Provider, runtime.Model, runtime.Protocol, runtime.BaseURL, runtime.ReasoningEffort, runtime.ServiceTier, contract)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		probe := ProbeResolvedModelForRole(ctx, runtime, string(route))
		result := modelchange.ProbeResult{
			Route: route, OK: probe.Err == nil, Provider: runtime.Provider,
			Model: runtime.Model, LatencyMS: probe.Latency.Milliseconds(),
		}
		if probe.Err != nil {
			result.Error = tools.RedactSensitive(probe.Err.Error())
			result.FailureClass = classifyModelProbeFailure(probe.Err)
		}
		results = append(results, result)
	}
	return results
}

func expandedModelValidationRoutes(cfg *config.Config, routes []modelchange.Route) []modelchange.Route {
	seen := make(map[modelchange.Route]struct{})
	result := make([]modelchange.Route, 0, len(routes)+len(modelchange.ManagedRoleRoutes()))
	appendRoute := func(route modelchange.Route) {
		if _, ok := seen[route]; ok {
			return
		}
		seen[route] = struct{}{}
		result = append(result, route)
	}
	for _, route := range routes {
		appendRoute(route)
		if route != modelchange.RouteAuxiliary {
			continue
		}
		for _, role := range modelchange.ManagedRoleRoutes() {
			var explicit config.ModelRoleConfig
			var ok bool
			if cfg != nil {
				explicit, ok = cfg.Models.Roles[string(role)]
			}
			if ok && !roleConfigEmpty(explicit) {
				continue
			}
			appendRoute(role)
		}
	}
	return result
}

// classifyModelProbeFailure is deliberately conservative. Only deterministic
// route incompatibility may trigger automatic rollback after restart; network,
// quota, cancellation, and unknown provider failures park for explicit
// recovery so an infrastructure incident is never misreported as a bad model.
func classifyModelProbeFailure(err error) modelchange.FailureClass {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return modelchange.FailureInfrastructure
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return modelchange.FailureInfrastructure
	}
	if info, ok := llm.ProviderErrorInfo(err); ok {
		switch info.Class {
		case llm.ProviderErrorInvalidRequest, llm.ProviderErrorAuth, llm.ProviderErrorEmptyResponse:
			return modelchange.FailureModel
		default:
			return modelchange.FailureInfrastructure
		}
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"connection refused", "connection reset", "broken pipe", "timeout", "timed out", "temporarily unavailable", "rate limit", "rate_limit", "quota", "overloaded", "http 500", "http 502", "http 503", "http 504"} {
		if strings.Contains(message, marker) {
			return modelchange.FailureInfrastructure
		}
	}
	return modelchange.FailureModel
}
