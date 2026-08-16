package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
)

// maintenanceRouteIdentity names one physical quota route. ID is a one-way
// digest of provider/endpoint/credential; raw credentials never leave the
// runtime resolver.
type maintenanceRouteIdentity struct {
	ID         string
	ContractID string
	Provider   string
	Model      string
}

// namedMaintenanceProvider is a configured cheap/background provider. The
// chain resolves explicit role overrides before models.auxiliary and never
// invents a fallback to the primary coding model.
type namedMaintenanceProvider struct {
	role     llm.ModelRole
	provider llm.Provider
	route    maintenanceRouteIdentity
}

type maintenanceProviderChain struct {
	providers        []namedMaintenanceProvider
	control          *control.Store
	tenantID         string
	probeInitial     time.Duration
	probeMax         time.Duration
	softProbeInitial time.Duration
	softProbeMax     time.Duration
	probeLease       time.Duration
}

func maintenanceRouteIDs(routes []maintenanceRouteIdentity) []string {
	ids := make([]string, 0, len(routes))
	for _, route := range routes {
		if strings.TrimSpace(route.ID) != "" {
			ids = append(ids, route.ID)
		}
	}
	return ids
}

// configuredMaintenanceProvider builds one quota-aware provider chain from
// configured maintenance roles. Roles resolving to the same
// endpoint and credential are de-duplicated before any request is sent.
func configuredMaintenanceProvider(mem *memory.MemoryManager, cfg *config.Config, tenantID string,
	controlStore *control.Store, roles ...llm.ModelRole) (llm.Provider, []maintenanceRouteIdentity) {
	providers := make([]namedMaintenanceProvider, 0, len(roles))
	routes := make([]maintenanceRouteIdentity, 0, len(roles))
	seenRoutes := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		provider := configuredAuxiliaryRoleProvider(mem, cfg, tenantID, role)
		if provider == nil {
			continue
		}
		route := maintenanceRoleRouteIdentity(cfg, role)
		if route.ID != "" {
			if _, exists := seenRoutes[route.ID]; exists {
				log.Info("maintenance provider: skipping duplicate physical route", "role", role)
				continue
			}
			seenRoutes[route.ID] = struct{}{}
			routes = append(routes, route)
		}
		providers = append(providers, namedMaintenanceProvider{role: role, provider: provider, route: route})
	}
	if len(providers) == 0 {
		return nil, routes
	}
	if controlStore == nil {
		return providers[0].provider, routes
	}
	initial, maximum := cfg.Tasks.MaintenanceQuotaCircuitPolicy()
	softInitial, softMaximum := cfg.Tasks.MaintenanceSoftCircuitPolicy()
	return &maintenanceProviderChain{
		providers: providers, control: controlStore, tenantID: tenantID,
		probeInitial: initial, probeMax: maximum,
		softProbeInitial: softInitial, softProbeMax: softMaximum,
		probeLease: 2 * time.Minute,
	}, routes
}

func (c *maintenanceProviderChain) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	var failures []string
	var lastErr error
	anyRetryable := false
	triggerClass := ""
	for index, candidate := range c.providers {
		allowed, probe, err := c.allow(ctx, candidate)
		if err != nil {
			log.Warn("maintenance route circuit lookup failed; failing open", "provider", candidate.route.Provider, "error", err)
			allowed = true
		}
		if !allowed {
			lastErr = c.openCircuitError(ctx, candidate, err)
			c.recordProviderCall(ctx, candidate, index, control.MaintenanceProviderCallCircuitOpen,
				triggerClass, lastErr, llm.UsageStats{}, "", 1, time.Time{})
			failures = append(failures, string(candidate.role)+": circuit open")
			triggerClass = providerErrorClass(lastErr)
			continue
		}
		started := time.Now()
		content, callErr := candidate.provider.ChatCompletion(ctx, messages)
		if callErr == nil && strings.TrimSpace(content) != "" {
			c.recordSuccess(ctx, candidate)
			c.recordProviderCall(ctx, candidate, index, control.MaintenanceProviderCallSucceeded,
				triggerClass, nil, llm.UsageStats{}, "", 1, started)
			return content, nil
		}
		if callErr == nil {
			callErr = &llm.ProviderError{Provider: candidate.route.Provider, RouteID: candidate.route.ID,
				Class: llm.ProviderErrorEmptyResponse, Message: "maintenance provider returned no usable content"}
		}
		callErr = llm.WithProviderRoute(callErr, candidate.route.Provider, candidate.route.ID)
		c.recordFailure(ctx, candidate, callErr, probe)
		c.recordProviderCall(ctx, candidate, index, control.MaintenanceProviderCallFailed,
			triggerClass, callErr, providerErrorUsage(callErr), providerErrorFinishReason(callErr), 1, started)
		anyRetryable = anyRetryable || llm.IsRetryableError(callErr)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		lastErr = callErr
		triggerClass = providerErrorClass(callErr)
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.role, callErr))
		log.Warn("maintenance provider unavailable; trying explicit fallback", "role", candidate.role, "provider", candidate.route.Provider, "error", callErr)
	}
	return "", maintenanceAggregateError(failures, lastErr, anyRetryable)
}

func (c *maintenanceProviderChain) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	req = boundedMaintenanceRequest(req)
	var failures []string
	var lastErr error
	anyRetryable := false
	triggerClass := ""
	batchSize := maintenanceBatchSize(req)
	for index, candidate := range c.providers {
		allowed, probe, err := c.allow(ctx, candidate)
		if err != nil {
			log.Warn("maintenance route circuit lookup failed; failing open", "provider", candidate.route.Provider, "error", err)
			allowed = true
		}
		if !allowed {
			lastErr = c.openCircuitError(ctx, candidate, err)
			c.recordProviderCall(ctx, candidate, index, control.MaintenanceProviderCallCircuitOpen,
				triggerClass, lastErr, llm.UsageStats{}, "", batchSize, time.Time{})
			failures = append(failures, string(candidate.role)+": circuit open")
			triggerClass = providerErrorClass(lastErr)
			continue
		}
		started := time.Now()
		resp, callErr := candidate.provider.Chat(ctx, req)
		if callErr == nil && resp != nil && (strings.TrimSpace(resp.Content) != "" || len(resp.ToolCalls) > 0) {
			c.recordSuccess(ctx, candidate)
			c.recordProviderCall(ctx, candidate, index, control.MaintenanceProviderCallSucceeded,
				triggerClass, nil, resp.Usage, resp.FinishReason, batchSize, started)
			return resp, nil
		}
		if callErr == nil && resp != nil && maintenanceFinishReasonTruncated(resp.FinishReason) {
			contractErr := &llm.ProviderError{
				Provider: candidate.route.Provider, RouteID: candidate.route.ID,
				Class:      llm.ProviderErrorEmptyResponse,
				Message:    "maintenance provider exhausted its output budget before producing usable content",
				StopReason: resp.FinishReason, Usage: resp.Usage,
			}
			c.recordProviderCall(ctx, candidate, index, control.MaintenanceProviderCallFailed,
				triggerClass, contractErr, resp.Usage, resp.FinishReason, batchSize, started)
			// A truncated empty body is an output-contract failure, not evidence
			// that the physical provider or credential is unhealthy. Let the
			// analyzer increase its budget once before moving to an independent
			// fallback; otherwise this path opens the quota circuit and the
			// adaptive retry in post_run_analyzer can never run.
			if maintenanceContractAttempt(req) <= 1 || index == len(c.providers)-1 {
				return resp, nil
			}
			lastErr = contractErr
			triggerClass = "output_contract"
			failures = append(failures, fmt.Sprintf("%s: %v", candidate.role, contractErr))
			continue
		}
		if callErr == nil {
			usage := llm.UsageStats{}
			finishReason := ""
			if resp != nil {
				usage = resp.Usage
				finishReason = resp.FinishReason
			}
			callErr = &llm.ProviderError{Provider: candidate.route.Provider, RouteID: candidate.route.ID,
				Class: llm.ProviderErrorEmptyResponse, Message: "maintenance provider returned no usable content",
				StopReason: finishReason, Usage: usage}
		}
		callErr = llm.WithProviderRoute(callErr, candidate.route.Provider, candidate.route.ID)
		c.recordFailure(ctx, candidate, callErr, probe)
		c.recordProviderCall(ctx, candidate, index, control.MaintenanceProviderCallFailed,
			triggerClass, callErr, providerErrorUsage(callErr), providerErrorFinishReason(callErr), batchSize, started)
		anyRetryable = anyRetryable || llm.IsRetryableError(callErr)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = callErr
		triggerClass = providerErrorClass(callErr)
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.role, callErr))
		log.Warn("maintenance provider unavailable; trying explicit fallback", "role", candidate.role, "provider", candidate.route.Provider, "error", callErr)
	}
	return nil, maintenanceAggregateError(failures, lastErr, anyRetryable)
}

func maintenanceContractAttempt(req llm.ChatRequest) int {
	if req.Options == nil {
		return 0
	}
	switch value := req.Options["maintenance_contract_attempt"].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func (c *maintenanceProviderChain) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	req = boundedMaintenanceRequest(req)
	var failures []string
	var lastErr error
	anyRetryable := false
	triggerClass := ""
	for index, candidate := range c.providers {
		allowed, probe, err := c.allow(ctx, candidate)
		if err != nil {
			log.Warn("maintenance route circuit lookup failed; failing open", "provider", candidate.route.Provider, "error", err)
			allowed = true
		}
		if !allowed {
			lastErr = c.openCircuitError(ctx, candidate, err)
			c.recordProviderCall(ctx, candidate, index, control.MaintenanceProviderCallCircuitOpen,
				triggerClass, lastErr, llm.UsageStats{}, "", maintenanceBatchSize(req), time.Time{})
			failures = append(failures, string(candidate.role)+": circuit open")
			triggerClass = providerErrorClass(lastErr)
			continue
		}
		started := time.Now()
		stream, callErr := candidate.provider.StreamChat(ctx, req)
		if callErr == nil && stream != nil {
			return c.observeStream(ctx, candidate, index, triggerClass, stream, probe, maintenanceBatchSize(req), started), nil
		}
		if callErr == nil {
			callErr = &llm.ProviderError{Provider: candidate.route.Provider, RouteID: candidate.route.ID,
				Class: llm.ProviderErrorEmptyResponse, Message: "maintenance provider returned a nil stream"}
		}
		callErr = llm.WithProviderRoute(callErr, candidate.route.Provider, candidate.route.ID)
		c.recordFailure(ctx, candidate, callErr, probe)
		c.recordProviderCall(ctx, candidate, index, control.MaintenanceProviderCallFailed,
			triggerClass, callErr, providerErrorUsage(callErr), providerErrorFinishReason(callErr), maintenanceBatchSize(req), started)
		anyRetryable = anyRetryable || llm.IsRetryableError(callErr)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = callErr
		triggerClass = providerErrorClass(callErr)
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.role, callErr))
		log.Warn("maintenance provider unavailable; trying explicit fallback", "role", candidate.role, "provider", candidate.route.Provider, "error", callErr)
	}
	return nil, maintenanceAggregateError(failures, lastErr, anyRetryable)
}

// boundedMaintenanceRequest keeps background control work predictable even
// when models.auxiliary is configured with deep reasoning for ad-hoc use. The
// foreground primary route is untouched; only this maintenance-only chain
// installs the protocol-neutral disabled value translated by each adapter.
func boundedMaintenanceRequest(req llm.ChatRequest) llm.ChatRequest {
	options := make(map[string]interface{}, len(req.Options)+1)
	for key, value := range req.Options {
		options[key] = value
	}
	options["reasoning_effort"] = maintenanceReasoningEffort
	req.Options = options
	return req
}

// observeStream keeps the physical-route circuit honest when a provider
// accepts the HTTP request but reports quota or an empty response inside SSE.
// It also closes a half-open probe only after the stream produced semantic
// output, never merely because request setup succeeded.
func (c *maintenanceProviderChain) observeStream(ctx context.Context, candidate namedMaintenanceProvider,
	index int, triggerClass string, stream <-chan llm.StreamEvent, probe bool, batchSize int, started time.Time) <-chan llm.StreamEvent {
	out := make(chan llm.StreamEvent)
	go func() {
		defer close(out)
		sawSemantic := false
		usage := llm.UsageStats{}
		finishReason := ""
		for event := range stream {
			if event.Usage != nil {
				usage = *event.Usage
			}
			if strings.TrimSpace(event.FinishReason) != "" {
				finishReason = event.FinishReason
			}
			if strings.TrimSpace(event.Content) != "" || len(event.ToolCalls) > 0 ||
				strings.TrimSpace(event.ToolName) != "" || strings.TrimSpace(event.ToolResult) != "" {
				sawSemantic = true
			}
			if event.Err != nil {
				event.Err = llm.WithProviderRoute(event.Err, candidate.route.Provider, candidate.route.ID)
				c.recordFailure(ctx, candidate, event.Err, probe)
				c.recordProviderCall(ctx, candidate, index, control.MaintenanceProviderCallFailed,
					triggerClass, event.Err, providerErrorUsageOr(event.Err, usage), providerErrorFinishReasonOr(event.Err, finishReason), batchSize, started)
				select {
				case out <- event:
				case <-ctx.Done():
				}
				return
			}
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
		if sawSemantic {
			c.recordSuccess(ctx, candidate)
			c.recordProviderCall(ctx, candidate, index, control.MaintenanceProviderCallSucceeded,
				triggerClass, nil, usage, finishReason, batchSize, started)
			return
		}
		err := &llm.ProviderError{Provider: candidate.route.Provider, RouteID: candidate.route.ID,
			Class: llm.ProviderErrorEmptyResponse, Message: "maintenance provider stream returned no usable content",
			StopReason: finishReason, Usage: usage}
		c.recordFailure(ctx, candidate, err, probe)
		c.recordProviderCall(ctx, candidate, index, control.MaintenanceProviderCallFailed,
			triggerClass, err, usage, finishReason, batchSize, started)
		select {
		case out <- llm.StreamEvent{Err: err}:
		case <-ctx.Done():
		}
	}()
	return out
}

func (c *maintenanceProviderChain) allow(ctx context.Context, candidate namedMaintenanceProvider) (bool, bool, error) {
	if c == nil || c.control == nil || candidate.route.ID == "" {
		return true, false, nil
	}
	allowed, probe, _, err := c.control.ClaimProviderRoute(ctx, c.routeTenant(ctx), candidate.route.ID,
		candidate.route.Provider, candidate.route.Model, time.Now(), c.probeLease)
	return allowed, probe, err
}

func (c *maintenanceProviderChain) openCircuitError(ctx context.Context, candidate namedMaintenanceProvider, lookupErr error) error {
	class := llm.ProviderErrorQuota
	message := "provider circuit is open; waiting for the next scheduled probe"
	if lookupErr != nil {
		message = "provider route is unavailable"
	} else if c != nil && c.control != nil && candidate.route.ID != "" {
		if health, err := c.control.GetProviderRouteHealth(ctx, c.routeTenant(ctx), candidate.route.ID); err == nil && health != nil {
			if parsed := llm.ProviderErrorClass(strings.TrimSpace(health.FailureClass)); parsed != "" {
				class = parsed
			}
		}
	}
	return &llm.ProviderError{Provider: candidate.route.Provider, RouteID: candidate.route.ID,
		Class: class, Message: message}
}

func (c *maintenanceProviderChain) recordSuccess(ctx context.Context, candidate namedMaintenanceProvider) {
	if c == nil || c.control == nil || candidate.route.ID == "" {
		return
	}
	if requeued, err := c.control.CloseProviderRoute(ctx, c.routeTenant(ctx), candidate.route.ID, time.Now()); err != nil {
		log.Warn("maintenance route circuit close failed", "provider", candidate.route.Provider, "error", err)
	} else if requeued > 0 {
		log.Info("maintenance provider route recovered; replaying blocked jobs", "provider", candidate.route.Provider, "jobs", requeued)
	}
}

func (c *maintenanceProviderChain) recordFailure(ctx context.Context, candidate namedMaintenanceProvider, err error, probe bool) {
	if c == nil || c.control == nil || candidate.route.ID == "" {
		return
	}
	info, _ := llm.ProviderErrorInfo(err)
	if llm.IsQuotaError(err) {
		next, openErr := c.control.OpenProviderRoute(ctx, c.routeTenant(ctx), candidate.route.ID,
			candidate.route.Provider, candidate.route.Model, string(llm.ProviderErrorQuota), err.Error(), info.RequestID,
			time.Now(), c.probeInitial, c.probeMax)
		if openErr != nil {
			log.Warn("maintenance quota circuit open failed", "provider", candidate.route.Provider, "error", openErr)
			return
		}
		log.Warn("maintenance provider quota circuit opened", "provider", candidate.route.Provider, "next_probe", next)
		return
	}
	if isMaintenanceOutputExhausted(err) {
		next, openErr := c.control.OpenProviderRoute(ctx, c.routeTenant(ctx), candidate.route.ID,
			candidate.route.Provider, candidate.route.Model, string(llm.ProviderErrorEmptyResponse), err.Error(), info.RequestID,
			time.Now(), c.softProbeInitial, c.softProbeMax)
		if openErr != nil {
			log.Warn("maintenance output-exhaustion circuit open failed", "provider", candidate.route.Provider, "error", openErr)
			return
		}
		log.Warn("maintenance provider output-exhaustion circuit opened", "provider", candidate.route.Provider, "next_probe", next)
		return
	}
	if probe {
		if deferErr := c.control.DeferProviderRouteProbe(ctx, c.routeTenant(ctx), candidate.route.ID, time.Now(), c.softProbeInitial); deferErr != nil {
			log.Warn("maintenance quota probe defer failed", "provider", candidate.route.Provider, "error", deferErr)
		}
	}
}

func (c *maintenanceProviderChain) routeTenant(ctx context.Context) string {
	if c != nil && strings.TrimSpace(c.tenantID) != "" {
		return c.tenantID
	}
	return llm.ModelContextFrom(ctx).TenantID
}

func maintenanceAggregateError(failures []string, lastErr error, anyRetryable bool) error {
	if lastErr == nil {
		lastErr = fmt.Errorf("no configured maintenance provider was available")
	}
	err := fmt.Errorf("maintenance providers unavailable (%s): %w", strings.Join(failures, "; "), lastErr)
	if !anyRetryable && len(failures) > 0 {
		return llm.NonRetryable(err)
	}
	return err
}
