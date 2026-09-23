package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelchange"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

// ValidateModelChange runs the same role-aware contract probes used by setup
// and doctor, but against an in-memory candidate configuration. It returns one
// bounded result per changed route and never exposes credentials.
func ValidateModelChange(ctx context.Context, cfg *config.Config, routes []modelchange.Route) []modelchange.ProbeResult {
	return validateModelChange(ctx, cfg, routes, nil)
}

// modelValidationEvidenceTTL bounds how long a passing probe stands in for
// the identical request: long enough to choose models and apply the draft,
// short enough that apply never trusts a stale observation.
const modelValidationEvidenceTTL = 10 * time.Minute

// ModelChangeValidator is the daemon's model-change validator. It remembers
// each passing probe briefly, keyed by the exact probe request, endpoint, and
// credential, so the proof a person watched while choosing a model is not
// repeated when the same draft is applied. Failures are never remembered.
type ModelChangeValidator struct {
	ttl    time.Duration
	now    func() time.Time
	mu     sync.Mutex
	passed map[string]rememberedProbe
}

type rememberedProbe struct {
	result modelchange.ProbeResult
	at     time.Time
}

func NewModelChangeValidator() *ModelChangeValidator {
	return &ModelChangeValidator{ttl: modelValidationEvidenceTTL, now: time.Now, passed: make(map[string]rememberedProbe)}
}

func (v *ModelChangeValidator) Validate(ctx context.Context, cfg *config.Config, routes []modelchange.Route) []modelchange.ProbeResult {
	return validateModelChange(ctx, cfg, routes, v)
}

func (v *ModelChangeValidator) recall(key string) (modelchange.ProbeResult, bool) {
	if v == nil {
		return modelchange.ProbeResult{}, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	remembered, ok := v.passed[key]
	if !ok || v.now().Sub(remembered.at) > v.ttl {
		delete(v.passed, key)
		return modelchange.ProbeResult{}, false
	}
	result := remembered.result
	result.Reused = true
	return result, true
}

func (v *ModelChangeValidator) remember(key string, result modelchange.ProbeResult) {
	if v == nil || !result.OK {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.passed[key] = rememberedProbe{result: result, at: v.now()}
}

// probeEvidenceKey extends the request identity with a digest of the
// credential that sent it, so evidence never outlives a key change. The
// digest stays in memory and is never logged.
func probeEvidenceKey(requestKey string, runtime modelruntime.Runtime) string {
	sum := sha256.Sum256([]byte(runtime.CredentialSource + "\x00" + runtime.APIKey))
	return requestKey + "\x00" + fmt.Sprintf("%x", sum[:8])
}

func validateModelChange(ctx context.Context, cfg *config.Config, routes []modelchange.Route, evidence *ModelChangeValidator) []modelchange.ProbeResult {
	routes = expandedModelValidationRoutes(cfg, routes)
	results := make([]modelchange.ProbeResult, len(routes))
	type probeTarget struct {
		runtime  modelruntime.Runtime
		role     modelchange.Route
		indices  []int
		provider llm.Provider
		evidence string
	}
	seen := make(map[string]*probeTarget)
	ordered := make([]*probeTarget, 0, len(routes))
	for index, route := range routes {
		if route != modelchange.RoutePrimary && cfg != nil && !cfg.AuxiliaryEnabled() {
			results[index] = modelchange.ProbeResult{Route: route, OK: true, Model: "disabled"}
			continue
		}
		runtime, err := ResolveModelRuntime(ctx, cfg, string(route))
		if err != nil {
			results[index] = modelchange.ProbeResult{
				Route: route, Error: tools.RedactSensitive(err.Error()),
				FailureClass: classifyModelProbeFailure(err),
			}
			continue
		}
		contract := modelProbeContractForRole(string(route))
		if route != modelchange.RoutePrimary && contract == modelProbeContractPlain {
			contract = "background_text"
		}
		provider := buildProviderFromRuntime(runtime)
		key := modelValidationProbeKey(ctx, runtime, provider, contract)
		if target, ok := seen[key]; ok {
			target.indices = append(target.indices, index)
			continue
		}
		target := &probeTarget{runtime: runtime, role: route, indices: []int{index}, provider: provider, evidence: probeEvidenceKey(key, runtime)}
		seen[key] = target
		ordered = append(ordered, target)
	}

	// Distinct physical endpoints are independent read-only lanes. Probe those
	// lanes concurrently, but serialize contracts within one endpoint so model
	// validation does not create a burst the configured service cannot sustain.
	// Results are projected back into the original route order after all bounded
	// probes return.
	type probeOutcome struct {
		target *probeTarget
		result modelchange.ProbeResult
	}
	outcomes := make(chan probeOutcome, len(ordered))
	lanes := make(map[string][]*probeTarget)
	for _, target := range ordered {
		if result, ok := evidence.recall(target.evidence); ok {
			outcomes <- probeOutcome{target: target, result: result}
			continue
		}
		lane := modelValidationProbeLane(target.runtime)
		lanes[lane] = append(lanes[lane], target)
	}
	for _, laneTargets := range lanes {
		go func(targets []*probeTarget) {
			for _, target := range targets {
				probe := probeResolvedModelForRole(ctx, target.runtime, string(target.role), target.provider)
				result := modelchange.ProbeResult{
					OK: probe.Err == nil, Provider: target.runtime.Provider,
					Model: target.runtime.Model, LatencyMS: probe.Latency.Milliseconds(),
					ThinkingMode: probe.ApprovalThinkingMode, Notice: probe.ApprovalNotice,
				}
				if probe.Err != nil {
					result.Error = tools.RedactSensitive(probe.Err.Error())
					result.FailureClass = classifyModelProbeFailure(probe.Err)
				}
				evidence.remember(target.evidence, result)
				outcomes <- probeOutcome{target: target, result: result}
			}
		}(laneTargets)
	}
	for range ordered {
		outcome := <-outcomes
		for _, index := range outcome.target.indices {
			result := outcome.result
			result.Route = routes[index]
			results[index] = result
		}
	}
	return results
}

func modelValidationProbeKey(ctx context.Context, runtime modelruntime.Runtime, provider llm.Provider, contract string) string {
	nativeTools := provider != nil && llm.ProviderSupportsNativeTools(provider) &&
		(contract == modelProbeContractPlain || contract == "background_text")
	request := modelProbeRequest(runtime, nativeTools, contract)
	if fingerprint, ok := llm.FingerprintProviderRequest(ctx, provider, request, false); ok {
		return strings.Join([]string{
			runtime.Provider, runtime.Protocol, runtime.BaseURL, contract,
			fingerprint.Protocol, fingerprint.RequestHash,
		}, "\x00")
	}
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s", runtime.Provider, runtime.Model, runtime.Protocol, runtime.BaseURL, runtime.ReasoningEffort, runtime.ServiceTier, contract)
}

// Model validation is rare control-plane work. Distinct physical endpoints may
// be probed concurrently, but contracts sharing one endpoint run in one lane.
// Bursting several probes at one constrained account produced timeouts that
// falsely looked like model incompatibility.
func modelValidationProbeLane(runtime modelruntime.Runtime) string {
	endpoint := strings.TrimSpace(runtime.BaseURL)
	if endpoint == "" {
		endpoint = strings.TrimSpace(runtime.Provider)
	}
	return strings.ToLower(strings.TrimSpace(runtime.Protocol)) + "\x00" + strings.ToLower(endpoint)
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
		if route != modelchange.RouteAuxiliary || (cfg != nil && !cfg.AuxiliaryEnabled()) {
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
