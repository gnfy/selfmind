package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
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

// auxiliaryFloorSlot names the shared models.auxiliary fallback in logs and
// maintenance telemetry. It is a slot, not a role: no models.roles entry
// carries this name and it never overrides a role's own configuration.
const auxiliaryFloorSlot = "auxiliary"

// namedMaintenanceProvider is a configured cheap/background provider. The
// chain resolves a role's own configuration before the models.auxiliary floor
// and never invents a fallback to the primary coding model.
//
// slot is the display and telemetry name of the chain position: a role name
// for the role's own configuration, auxiliaryFloorSlot for the floor. role
// stays the logical role being served so telemetry attributes a floor call to
// the work that needed it. maxOutputTokens is the resolved model's output
// ceiling; the chain clamps each request to the route that actually serves it.
type namedMaintenanceProvider struct {
	slot            string
	role            llm.ModelRole
	provider        llm.Provider
	route           maintenanceRouteIdentity
	maxOutputTokens int
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

// maintenanceCandidateSlot is one unresolved chain position: a slot name plus
// the role configuration that position should be built from.
type maintenanceCandidateSlot struct {
	slot    string
	role    llm.ModelRole
	roleCfg config.ModelRoleConfig
}

// maintenanceCandidateSlots returns the ordered chain for one maintenance
// role: the role's own configuration, then the models.auxiliary floor.
//
// Deprecated tasks.maintenance_fallback_roles slots are inserted between the
// two so existing installations keep their explicit provider hops. They pre-date
// the auxiliary floor and are not needed to obtain a fallback.
func maintenanceCandidateSlots(cfg *config.Config, role llm.ModelRole) []maintenanceCandidateSlot {
	if cfg == nil {
		return nil
	}
	slots := make([]maintenanceCandidateSlot, 0, 3)
	appendRole := func(candidate llm.ModelRole) {
		name := strings.TrimSpace(string(candidate))
		if name == "" {
			return
		}
		for _, existing := range slots {
			if existing.slot == name {
				return
			}
		}
		roleCfg, _, ok := cfg.ResolveAuxiliaryRole(name)
		if !ok || roleConfigEmpty(roleCfg) {
			return
		}
		slots = append(slots, maintenanceCandidateSlot{slot: name, role: candidate, roleCfg: roleCfg})
	}
	appendRole(role)
	if len(cfg.Tasks.MaintenanceFallbackRoles) > 0 {
		warnLegacyMaintenanceFallbackRoles(cfg)
		for _, legacy := range cfg.Tasks.MaintenanceFallbackRoles {
			appendRole(llm.ModelRole(strings.TrimSpace(legacy)))
		}
	}
	if floor, ok := cfg.AuxiliaryRoleFloor(); ok {
		slots = append(slots, maintenanceCandidateSlot{slot: auxiliaryFloorSlot, role: role, roleCfg: floor})
	}
	return slots
}

var legacyMaintenanceFallbackWarning sync.Once

func warnLegacyMaintenanceFallbackRoles(cfg *config.Config) {
	legacyMaintenanceFallbackWarning.Do(func() {
		log.Warn("tasks.maintenance_fallback_roles is deprecated: every background role now falls back to models.auxiliary. Remove the key unless you deliberately want extra provider hops between the two.",
			"roles", strings.Join(cfg.Tasks.MaintenanceFallbackRoles, ","))
	})
}

// buildMaintenanceCandidate resolves one chain position into a live provider.
func buildMaintenanceCandidate(mem *memory.MemoryManager, cfg *config.Config, tenantID string,
	slot maintenanceCandidateSlot) (namedMaintenanceProvider, bool) {
	providerName := firstNonEmpty(slot.roleCfg.Provider, defaultProviderName(cfg))
	provider := buildRoleProvider(cfg, slot.role, providerName, slot.roleCfg)
	if provider == nil {
		return namedMaintenanceProvider{}, false
	}
	if strings.TrimSpace(tenantID) == "" {
		tenantID = "default"
	}
	applyDynamicKeyGetter(provider, mem, tenantID, providerName)
	route, maxOutput := maintenanceRouteIdentityFor(cfg, slot.role, slot.roleCfg)
	return namedMaintenanceProvider{
		slot: slot.slot, role: slot.role, provider: provider,
		route: route, maxOutputTokens: maxOutput,
	}, true
}

// configuredMaintenanceProvider builds one quota-aware provider chain for a
// maintenance role: the role's own configuration, then the models.auxiliary
// floor. Positions resolving to the same endpoint and credential are
// de-duplicated before any request is sent, so an installation with a single
// provider honestly ends up with a single-entry chain.
func configuredMaintenanceProvider(mem *memory.MemoryManager, cfg *config.Config, tenantID string,
	controlStore *control.Store, role llm.ModelRole) (llm.Provider, []maintenanceRouteIdentity) {
	slots := maintenanceCandidateSlots(cfg, role)
	providers := make([]namedMaintenanceProvider, 0, len(slots))
	routes := make([]maintenanceRouteIdentity, 0, len(slots))
	seenRoutes := make(map[string]struct{}, len(slots))
	for _, slot := range slots {
		candidate, ok := buildMaintenanceCandidate(mem, cfg, tenantID, slot)
		if !ok {
			continue
		}
		if candidate.route.ID != "" {
			if _, exists := seenRoutes[candidate.route.ID]; exists {
				log.Info("maintenance provider: skipping duplicate physical route",
					"role", role, "slot", slot.slot, "provider", candidate.route.Provider)
				continue
			}
			seenRoutes[candidate.route.ID] = struct{}{}
			routes = append(routes, candidate.route)
		}
		providers = append(providers, candidate)
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
			failures = append(failures, candidate.slot+": circuit open")
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
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.slot, callErr))
		log.Warn("maintenance provider unavailable; trying the next chain position", "role", candidate.role, "slot", candidate.slot, "provider", candidate.route.Provider, "error", callErr)
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
			failures = append(failures, candidate.slot+": circuit open")
			triggerClass = providerErrorClass(lastErr)
			continue
		}
		started := time.Now()
		resp, callErr := candidate.provider.Chat(ctx, boundOutputForCandidate(req, candidate))
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
			failures = append(failures, fmt.Sprintf("%s: %v", candidate.slot, contractErr))
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
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.slot, callErr))
		log.Warn("maintenance provider unavailable; trying the next chain position", "role", candidate.role, "slot", candidate.slot, "provider", candidate.route.Provider, "error", callErr)
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
			failures = append(failures, candidate.slot+": circuit open")
			triggerClass = providerErrorClass(lastErr)
			continue
		}
		started := time.Now()
		stream, callErr := candidate.provider.StreamChat(ctx, boundOutputForCandidate(req, candidate))
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
		failures = append(failures, fmt.Sprintf("%s: %v", candidate.slot, callErr))
		log.Warn("maintenance provider unavailable; trying the next chain position", "role", candidate.role, "slot", candidate.slot, "provider", candidate.route.Provider, "error", callErr)
	}
	return nil, maintenanceAggregateError(failures, lastErr, anyRetryable)
}

// boundOutputForCandidate clamps the output budget to what this chain position
// can actually produce. The analyzer sizes its contract from the role's own
// route; a fallback route with a smaller ceiling must not be handed that
// number, or it reports a truncated empty body that looks like a provider
// fault instead of a budget mismatch. The clamp only ever lowers the budget,
// so a deliberately small request stays small.
func boundOutputForCandidate(req llm.ChatRequest, candidate namedMaintenanceProvider) llm.ChatRequest {
	if candidate.maxOutputTokens <= 0 || req.MaxTokens <= candidate.maxOutputTokens {
		return req
	}
	req.MaxTokens = candidate.maxOutputTokens
	return req
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

// MaintenanceFallbackSummary describes the fallback position resolved for one
// background role, so an operator can see whether a configured fallback is
// real. A chain whose positions share one endpoint and credential collapses to
// a single provider; that is honest behaviour, but it must not look like
// redundancy the installation does not have.
type MaintenanceFallbackSummary struct {
	// Chained reports whether this role runs through the maintenance chain at
	// all. Roles outside it resolve to exactly one provider.
	Chained bool
	// Slot, Provider, and Model name the first position that is a genuinely
	// different physical route. They stay empty when there is none.
	Slot     string
	Provider string
	Model    string
	// Collapsed reports that fallback positions exist but every one of them
	// resolves to the role's own endpoint and credential.
	Collapsed bool
}

// DescribeMaintenanceFallback resolves the fallback position for a role
// without building providers or contacting any endpoint.
func DescribeMaintenanceFallback(cfg *config.Config, role string) MaintenanceFallbackSummary {
	name := strings.TrimSpace(role)
	if cfg == nil || name == "" {
		return MaintenanceFallbackSummary{}
	}
	target := llm.ModelRole(name)
	if !containsModelRole(maintenanceChainedRoles(cfg), target) {
		return MaintenanceFallbackSummary{}
	}
	summary := MaintenanceFallbackSummary{Chained: true}
	slots := maintenanceCandidateSlots(cfg, target)
	if len(slots) < 2 {
		return summary
	}
	seen := make(map[string]struct{}, len(slots))
	if route, _ := maintenanceRouteIdentityFor(cfg, slots[0].role, slots[0].roleCfg); route.ID != "" {
		seen[route.ID] = struct{}{}
	}
	for _, slot := range slots[1:] {
		route, _ := maintenanceRouteIdentityFor(cfg, slot.role, slot.roleCfg)
		if route.ID == "" {
			continue
		}
		if _, duplicate := seen[route.ID]; duplicate {
			summary.Collapsed = true
			continue
		}
		summary.Slot, summary.Provider, summary.Model = slot.slot, route.Provider, route.Model
		summary.Collapsed = false
		return summary
	}
	return summary
}
