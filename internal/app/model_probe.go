package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

const modelRoleProbeTimeout = 20 * time.Second

// ModelRoleProbe reports one live request for one resolved runtime. Roles that
// share the same provider/model endpoint are intentionally grouped so doctor
// does not spend duplicate quota merely because several maintenance roles use
// the same Coding Plan profile.
type ModelRoleProbe struct {
	Roles                     []string
	Provider                  string
	Model                     string
	Latency                   time.Duration
	NativeToolsTested         bool
	ThinkingToolLoopTested    bool
	ThinkingToolLoopPassed    bool
	MaintenanceContractTested bool
	MaintenanceContractPassed bool
	Err                       error
}

type roleProbeTarget struct {
	roles    []string
	runtime  modelruntime.Runtime
	provider llm.Provider
}

// ResolveModelRuntime resolves primary, auxiliary, or one logical role through
// the same precedence used by the daemon.
func ResolveModelRuntime(ctx context.Context, cfg *config.Config, role string) (modelruntime.Runtime, error) {
	if cfg == nil {
		return modelruntime.Runtime{}, fmt.Errorf("config is required")
	}
	role = strings.TrimSpace(role)
	selection := modelruntime.Selection{}
	if role != "" && !strings.EqualFold(role, "primary") {
		if strings.EqualFold(role, "auxiliary") {
			if !cfg.AuxiliaryEnabled() {
				return modelruntime.Runtime{}, fmt.Errorf("background model work is disabled")
			}
			auxiliary := cfg.EffectiveAuxiliary()
			if strings.TrimSpace(auxiliary.Provider) == "" && strings.TrimSpace(auxiliary.Model) == "" {
				return modelruntime.Runtime{}, fmt.Errorf("auxiliary model is not configured")
			}
			selection = modelruntime.Selection{
				Provider: auxiliary.Provider, Model: auxiliary.Model,
				ContextLength: auxiliary.ContextLength, ReasoningEffort: auxiliary.Reasoning,
				ServiceTier: auxiliary.ServiceTier,
			}
		} else {
			var roleCfg config.ModelRoleConfig
			var ok bool
			if isAuxiliaryModelRole(llm.ModelRole(role)) {
				roleCfg, _, ok = cfg.ResolveAuxiliaryRole(role)
			} else {
				roleCfg, ok = cfg.Models.Roles[role]
				ok = ok && !roleConfigEmpty(roleCfg)
			}
			if !ok || roleConfigEmpty(roleCfg) {
				return modelruntime.Runtime{}, fmt.Errorf("model role %q is not configured", role)
			}
			selection = roleProviderSelection(llm.ModelRole(role), firstNonEmpty(roleCfg.Provider, defaultProviderName(cfg)), roleCfg)
		}
	}
	return modelruntime.NewResolver(cfg).Resolve(ctx, selection)
}

// ProbeResolvedModel performs one bounded request against a resolved runtime.
// Native-tool transports receive an optional-only schema with a typed nil
// required slice so this check catches adapter serialization regressions.
func ProbeResolvedModel(ctx context.Context, rt modelruntime.Runtime) ModelRoleProbe {
	return ProbeResolvedModelForRole(ctx, rt, "")
}

// ProbeResolvedModelForRole validates the actual maintenance JSON contract for
// auxiliary/background roles while preserving the lightweight OK probe for the
// foreground coding model.
func ProbeResolvedModelForRole(ctx context.Context, rt modelruntime.Runtime, role string) ModelRoleProbe {
	probe := ModelRoleProbe{Provider: rt.Provider, Model: rt.Model}
	provider := buildProviderFromRuntime(rt)
	start := time.Now()
	if provider == nil {
		probe.Err = fmt.Errorf("provider could not be built")
		probe.Latency = time.Since(start)
		return probe
	}
	probe.MaintenanceContractTested = isMaintenanceProbeRole(role)
	// A maintenance probe must mirror the real analyzer request, which never
	// carries agent tools. Combining an optional tool schema with the JSON
	// contract lets a provider legitimately return a tool-call-only response
	// and turns a healthy maintenance route into a false negative.
	probe.NativeToolsTested = llm.ProviderSupportsNativeTools(provider) && !probe.MaintenanceContractTested
	probeCtx, cancel := context.WithTimeout(ctx, modelRoleProbeTimeout)
	defer cancel()
	resp, err := provider.Chat(probeCtx, modelProbeRequest(rt, probe.NativeToolsTested, probe.MaintenanceContractTested))
	switch {
	case err != nil:
		probe.Err = err
	case modelProbeContentError(resp) != nil:
		probe.Err = modelProbeContentError(resp)
	case probe.MaintenanceContractTested:
		if maintenanceFinishReasonTruncated(resp.FinishReason) {
			probe.Err = fmt.Errorf("maintenance contract was truncated (finish_reason=%s)", resp.FinishReason)
		} else if _, err := decodePostRunAnalysis(resp.Content); err != nil {
			probe.Err = fmt.Errorf("maintenance contract failed: %w", err)
		} else {
			probe.MaintenanceContractPassed = true
		}
	}
	if probe.Err == nil && !probe.MaintenanceContractTested && shouldProbeThinkingToolLoop(rt) {
		probe.ThinkingToolLoopTested = true
		if err := probeThinkingToolLoop(probeCtx, provider, rt); err != nil {
			probe.Err = fmt.Errorf("thinking tool loop failed: %w", err)
		} else {
			probe.ThinkingToolLoopPassed = true
		}
	}
	probe.Latency = time.Since(start)
	return probe
}

func shouldProbeThinkingToolLoop(rt modelruntime.Runtime) bool {
	if !strings.EqualFold(strings.TrimSpace(rt.Quirks.ThinkingMode), modelruntime.ThinkingModeDeepSeek) {
		return false
	}
	if kind, _ := rt.Thinking["type"].(string); strings.EqualFold(strings.TrimSpace(kind), "disabled") {
		return false
	}
	return true
}

func probeThinkingToolLoop(ctx context.Context, provider llm.Provider, rt modelruntime.Runtime) error {
	tool := llm.ToolDefinition{
		Name:        "selfmind_thinking_check",
		Description: "Required no-op tool for validating thinking-mode tool-call replay.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"value": map[string]interface{}{"type": "string"},
			},
			"required": []string{"value"},
		},
	}
	firstMessages := []llm.Message{{
		Role: "user", Content: "Call selfmind_thinking_check exactly once with value ping. Do not answer before calling it.",
	}}
	first, err := provider.Chat(ctx, llm.ChatRequest{Model: rt.Model, MaxTokens: 256, Messages: firstMessages, Tools: []llm.ToolDefinition{tool}})
	if err != nil {
		return err
	}
	if first == nil || len(first.ToolCalls) != 1 {
		return fmt.Errorf("model did not emit the required tool call")
	}
	if strings.TrimSpace(first.ReasoningContent) == "" {
		return fmt.Errorf("provider did not return reasoning_content for a thinking tool call")
	}
	call := first.ToolCalls[0]
	secondMessages := append(append([]llm.Message(nil), firstMessages...),
		llm.Message{Role: "assistant", Content: first.Content, ReasoningContent: first.ReasoningContent, ToolCalls: first.ToolCalls},
		llm.Message{Role: "tool", ToolCallID: call.ID, Name: call.Function, Content: `{"ok":true}`},
	)
	second, err := provider.Chat(ctx, llm.ChatRequest{Model: rt.Model, MaxTokens: 128, Messages: secondMessages, Tools: []llm.ToolDefinition{tool}})
	if err != nil {
		return err
	}
	if second == nil || strings.TrimSpace(second.Content) == "" {
		return fmt.Errorf("model returned no final answer after the tool result")
	}
	return nil
}

func modelProbeRequest(rt modelruntime.Runtime, includeTools, maintenanceContract bool) llm.ChatRequest {
	req := llm.ChatRequest{
		Model:        rt.Model,
		MaxTokens:    64,
		SystemPrompt: "This is a model health check. Return exactly OK and do not call tools.",
		Messages:     []llm.Message{{Role: "user", Content: "Reply with OK."}},
	}
	if maintenanceContract {
		req.MaxTokens = postRunAnalyzerMaxTokens
		if rt.MaxTokens > 0 && req.MaxTokens > rt.MaxTokens {
			req.MaxTokens = rt.MaxTokens
		}
		req.SystemPrompt = postRunAnalyzerSystemPrompt + "\nFor this health check, do not call tools."
		req.Messages = []llm.Message{{Role: "user", Content: "Health-check data only. Return task_decision KEEP and an empty memory_decisions array."}}
		req.Options = map[string]interface{}{
			"temperature": 0, "maintenance_contract_probe": true,
			"reasoning_effort": maintenanceReasoningEffort,
		}
	}
	if includeTools {
		var required []string
		req.Tools = []llm.ToolDefinition{{
			Name:        "selfmind_model_check",
			Description: "Optional no-op tool used only to validate tool schema compatibility.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"value": map[string]interface{}{"type": "string"},
				},
				"required": required,
			},
		}}
	}
	return req
}

func isMaintenanceProbeRole(role string) bool {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "auxiliary" {
		return true
	}
	return llm.ModelRole(role) == llm.RoleMemoryExtract
}

// ProbeConfiguredModelRoles performs a bounded live health check for the
// auxiliary route and explicit role overrides. Inherited auxiliary roles are
// represented by one probe so doctor does not multiply quota usage.
func ProbeConfiguredModelRoles(ctx context.Context, cfg *config.Config) []ModelRoleProbe {
	if cfg == nil {
		return nil
	}
	roleNames := make([]string, 0, len(cfg.Models.Roles))
	for roleName := range cfg.Models.Roles {
		roleNames = append(roleNames, roleName)
	}
	sort.Strings(roleNames)

	resolver := modelruntime.NewResolver(cfg)
	targets := make(map[string]*roleProbeTarget)
	var unresolved []ModelRoleProbe
	auxiliary := cfg.EffectiveAuxiliary()
	if cfg.AuxiliaryEnabled() && (strings.TrimSpace(auxiliary.Provider) != "" || strings.TrimSpace(auxiliary.Model) != "") {
		selection := modelruntime.Selection{
			Provider: auxiliary.Provider, Model: auxiliary.Model,
			ContextLength: auxiliary.ContextLength, ReasoningEffort: auxiliary.Reasoning,
			ServiceTier: auxiliary.ServiceTier,
		}
		rt, err := resolver.Resolve(ctx, selection)
		if err != nil {
			unresolved = append(unresolved, ModelRoleProbe{Roles: []string{"auxiliary"}, Provider: selection.Provider, Model: selection.Model, Err: err})
		} else {
			key := strings.Join([]string{rt.Provider, rt.Model, rt.Protocol, rt.BaseURL}, "\x00")
			targets[key] = &roleProbeTarget{roles: []string{"auxiliary"}, runtime: rt, provider: buildProviderForSelectionWithRuntime(cfg, selection)}
		}
	}
	for _, roleName := range roleNames {
		if !cfg.AuxiliaryEnabled() && isAuxiliaryModelRole(llm.ModelRole(roleName)) {
			continue
		}
		roleCfg := cfg.Models.Roles[roleName]
		if roleConfigEmpty(roleCfg) {
			continue
		}
		selection := roleProviderSelection(llm.ModelRole(roleName), firstNonEmpty(roleCfg.Provider, defaultProviderName(cfg)), roleCfg)
		rt, err := resolver.Resolve(ctx, selection)
		if err != nil {
			unresolved = append(unresolved, ModelRoleProbe{Roles: []string{roleName}, Provider: selection.Provider, Model: selection.Model, Err: err})
			continue
		}
		key := strings.Join([]string{rt.Provider, rt.Model, rt.Protocol, rt.BaseURL}, "\x00")
		if target := targets[key]; target != nil {
			target.roles = append(target.roles, roleName)
			continue
		}
		targets[key] = &roleProbeTarget{
			roles:    []string{roleName},
			runtime:  rt,
			provider: buildProviderForSelectionWithRuntime(cfg, selection),
		}
	}

	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	results := append([]ModelRoleProbe(nil), unresolved...)
	for _, key := range keys {
		target := targets[key]
		probe := ModelRoleProbe{Roles: append([]string(nil), target.roles...), Provider: target.runtime.Provider, Model: target.runtime.Model}
		start := time.Now()
		if target.provider == nil {
			probe.Err = fmt.Errorf("provider could not be built")
		} else {
			probeCtx, cancel := context.WithTimeout(ctx, modelRoleProbeTimeout)
			probeCtx = llm.WithModelContext(probeCtx, llm.ModelContext{Role: llm.ModelRole(target.roles[0])})
			probe.MaintenanceContractTested = false
			for _, role := range target.roles {
				probe.MaintenanceContractTested = probe.MaintenanceContractTested || isMaintenanceProbeRole(role)
			}
			probe.NativeToolsTested = llm.ProviderSupportsNativeTools(target.provider) && !probe.MaintenanceContractTested
			resp, err := target.provider.Chat(probeCtx, modelProbeRequest(target.runtime, probe.NativeToolsTested, probe.MaintenanceContractTested))
			switch {
			case err != nil:
				probe.Err = err
			case modelProbeContentError(resp) != nil:
				probe.Err = modelProbeContentError(resp)
			case probe.MaintenanceContractTested && maintenanceFinishReasonTruncated(resp.FinishReason):
				probe.Err = fmt.Errorf("maintenance contract was truncated (finish_reason=%s)", resp.FinishReason)
			case probe.MaintenanceContractTested:
				if _, decodeErr := decodePostRunAnalysis(resp.Content); decodeErr != nil {
					probe.Err = fmt.Errorf("maintenance contract failed: %w", decodeErr)
				} else {
					probe.MaintenanceContractPassed = true
				}
			}
			if probe.Err == nil && !probe.MaintenanceContractTested && shouldProbeThinkingToolLoop(target.runtime) {
				probe.ThinkingToolLoopTested = true
				if thinkingErr := probeThinkingToolLoop(probeCtx, target.provider, target.runtime); thinkingErr != nil {
					probe.Err = fmt.Errorf("thinking tool loop failed: %w", thinkingErr)
				} else {
					probe.ThinkingToolLoopPassed = true
				}
			}
			cancel()
		}
		probe.Latency = time.Since(start)
		results = append(results, probe)
	}
	sort.Slice(results, func(i, j int) bool {
		return strings.Join(results[i].Roles, ",") < strings.Join(results[j].Roles, ",")
	})
	return results
}

func modelProbeContentError(resp *llm.ChatResponse) error {
	if resp == nil {
		return fmt.Errorf("model returned an empty response")
	}
	if strings.TrimSpace(resp.Content) != "" {
		return nil
	}
	if len(resp.ToolCalls) > 0 {
		return fmt.Errorf("model returned an unexpected tool call instead of probe text")
	}
	return fmt.Errorf("model returned an empty response")
}
