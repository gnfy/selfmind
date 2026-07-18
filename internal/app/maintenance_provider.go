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
	ID       string
	Provider string
	Model    string
}

// namedMaintenanceProvider is an explicitly configured cheap/background
// provider. The chain never invents a fallback to the primary coding model;
// every role in it must be listed under models.roles.
type namedMaintenanceProvider struct {
	role     llm.ModelRole
	provider llm.Provider
	route    maintenanceRouteIdentity
}

type maintenanceProviderChain struct {
	providers    []namedMaintenanceProvider
	control      *control.Store
	tenantID     string
	probeInitial time.Duration
	probeMax     time.Duration
	probeLease   time.Duration
}

// configuredMaintenanceProvider builds one quota-aware provider chain from
// explicitly configured maintenance roles. Roles resolving to the same
// endpoint and credential are de-duplicated before any request is sent.
func configuredMaintenanceProvider(mem *memory.MemoryManager, cfg *config.Config, tenantID string,
	controlStore *control.Store, roles ...llm.ModelRole) (llm.Provider, []maintenanceRouteIdentity) {
	providers := make([]namedMaintenanceProvider, 0, len(roles))
	routes := make([]maintenanceRouteIdentity, 0, len(roles))
	seenRoutes := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		provider := explicitRoleProvider(mem, cfg, tenantID, role)
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
	return &maintenanceProviderChain{
		providers: providers, control: controlStore, tenantID: tenantID,
		probeInitial: initial, probeMax: maximum, probeLease: 2 * time.Minute,
	}, routes
}

func (c *maintenanceProviderChain) ChatCompletion(ctx context.Context, messages []llm.Message) (string, error) {
	var failures []string
	var lastErr error
	anyRetryable := false
	for _, candidate := range c.providers {
		allowed, probe, err := c.allow(ctx, candidate)
		if err != nil {
			log.Warn("maintenance route circuit lookup failed; failing open", "provider", candidate.route.Provider, "error", err)
			allowed = true
		}
		if !allowed {
			lastErr = c.openCircuitError(candidate, err)
			failures = append(failures, string(candidate.role)+": circuit open")
			continue
		}
		content, callErr := candidate.provider.ChatCompletion(ctx, messages)
		if callErr == nil && strings.TrimSpace(content) != "" {
			c.recordSuccess(ctx, candidate)
			return content, nil
		}
		if callErr == nil {
			callErr = &llm.ProviderError{Provider: candidate.route.Provider, RouteID: candidate.route.ID,
				Class: llm.ProviderErrorEmptyResponse, Message: "maintenance provider returned no usable content"}
		}
		callErr = llm.WithProviderRoute(callErr, candidate.route.Provider, candidate.route.ID)
		c.recordFailure(ctx, candidate, callErr, probe)
		anyRetryable = anyRetryable || llm.IsRetryableError(callErr)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		lastErr = callErr
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.role, callErr))
		log.Warn("maintenance provider unavailable; trying explicit fallback", "role", candidate.role, "provider", candidate.route.Provider, "error", callErr)
	}
	return "", maintenanceAggregateError(failures, lastErr, anyRetryable)
}

func (c *maintenanceProviderChain) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	var failures []string
	var lastErr error
	anyRetryable := false
	for _, candidate := range c.providers {
		allowed, probe, err := c.allow(ctx, candidate)
		if err != nil {
			log.Warn("maintenance route circuit lookup failed; failing open", "provider", candidate.route.Provider, "error", err)
			allowed = true
		}
		if !allowed {
			lastErr = c.openCircuitError(candidate, err)
			failures = append(failures, string(candidate.role)+": circuit open")
			continue
		}
		resp, callErr := candidate.provider.Chat(ctx, req)
		if callErr == nil && resp != nil && (strings.TrimSpace(resp.Content) != "" || len(resp.ToolCalls) > 0) {
			c.recordSuccess(ctx, candidate)
			return resp, nil
		}
		if callErr == nil {
			callErr = &llm.ProviderError{Provider: candidate.route.Provider, RouteID: candidate.route.ID,
				Class: llm.ProviderErrorEmptyResponse, Message: "maintenance provider returned no usable content"}
		}
		callErr = llm.WithProviderRoute(callErr, candidate.route.Provider, candidate.route.ID)
		c.recordFailure(ctx, candidate, callErr, probe)
		anyRetryable = anyRetryable || llm.IsRetryableError(callErr)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = callErr
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.role, callErr))
		log.Warn("maintenance provider unavailable; trying explicit fallback", "role", candidate.role, "provider", candidate.route.Provider, "error", callErr)
	}
	return nil, maintenanceAggregateError(failures, lastErr, anyRetryable)
}

func (c *maintenanceProviderChain) StreamChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamEvent, error) {
	var failures []string
	var lastErr error
	anyRetryable := false
	for _, candidate := range c.providers {
		allowed, probe, err := c.allow(ctx, candidate)
		if err != nil {
			log.Warn("maintenance route circuit lookup failed; failing open", "provider", candidate.route.Provider, "error", err)
			allowed = true
		}
		if !allowed {
			lastErr = c.openCircuitError(candidate, err)
			failures = append(failures, string(candidate.role)+": circuit open")
			continue
		}
		stream, callErr := candidate.provider.StreamChat(ctx, req)
		if callErr == nil && stream != nil {
			return c.observeStream(ctx, candidate, stream, probe), nil
		}
		if callErr == nil {
			callErr = &llm.ProviderError{Provider: candidate.route.Provider, RouteID: candidate.route.ID,
				Class: llm.ProviderErrorEmptyResponse, Message: "maintenance provider returned a nil stream"}
		}
		callErr = llm.WithProviderRoute(callErr, candidate.route.Provider, candidate.route.ID)
		c.recordFailure(ctx, candidate, callErr, probe)
		anyRetryable = anyRetryable || llm.IsRetryableError(callErr)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = callErr
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.role, callErr))
		log.Warn("maintenance provider unavailable; trying explicit fallback", "role", candidate.role, "provider", candidate.route.Provider, "error", callErr)
	}
	return nil, maintenanceAggregateError(failures, lastErr, anyRetryable)
}

// observeStream keeps the physical-route circuit honest when a provider
// accepts the HTTP request but reports quota or an empty response inside SSE.
// It also closes a half-open probe only after the stream produced semantic
// output, never merely because request setup succeeded.
func (c *maintenanceProviderChain) observeStream(ctx context.Context, candidate namedMaintenanceProvider,
	stream <-chan llm.StreamEvent, probe bool) <-chan llm.StreamEvent {
	out := make(chan llm.StreamEvent)
	go func() {
		defer close(out)
		sawSemantic := false
		for event := range stream {
			if strings.TrimSpace(event.Content) != "" || len(event.ToolCalls) > 0 ||
				strings.TrimSpace(event.ToolName) != "" || strings.TrimSpace(event.ToolResult) != "" {
				sawSemantic = true
			}
			if event.Err != nil {
				event.Err = llm.WithProviderRoute(event.Err, candidate.route.Provider, candidate.route.ID)
				c.recordFailure(ctx, candidate, event.Err, probe)
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
			return
		}
		err := &llm.ProviderError{Provider: candidate.route.Provider, RouteID: candidate.route.ID,
			Class: llm.ProviderErrorEmptyResponse, Message: "maintenance provider stream returned no usable content"}
		c.recordFailure(ctx, candidate, err, probe)
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

func (c *maintenanceProviderChain) openCircuitError(candidate namedMaintenanceProvider, lookupErr error) error {
	message := "provider quota circuit is open; waiting for the next scheduled probe"
	if lookupErr != nil {
		message = "provider route is unavailable"
	}
	return &llm.ProviderError{Provider: candidate.route.Provider, RouteID: candidate.route.ID,
		Class: llm.ProviderErrorQuota, Message: message}
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
	if probe {
		if deferErr := c.control.DeferProviderRouteProbe(ctx, c.routeTenant(ctx), candidate.route.ID, time.Now(), c.probeInitial); deferErr != nil {
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
