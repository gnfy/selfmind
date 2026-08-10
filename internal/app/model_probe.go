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
	Roles             []string
	Provider          string
	Model             string
	Latency           time.Duration
	NativeToolsTested bool
	Err               error
}

type roleProbeTarget struct {
	roles    []string
	runtime  modelruntime.Runtime
	provider llm.Provider
}

// ResolveModelRuntime resolves the primary model or one explicit role through
// the same selection path used by the daemon.
func ResolveModelRuntime(ctx context.Context, cfg *config.Config, role string) (modelruntime.Runtime, error) {
	if cfg == nil {
		return modelruntime.Runtime{}, fmt.Errorf("config is required")
	}
	role = strings.TrimSpace(role)
	selection := modelruntime.Selection{}
	if role != "" && !strings.EqualFold(role, "primary") {
		roleCfg, ok := cfg.Models.Roles[role]
		if !ok || roleConfigEmpty(roleCfg) {
			return modelruntime.Runtime{}, fmt.Errorf("model role %q is not configured", role)
		}
		selection = roleProviderSelection(llm.ModelRole(role), firstNonEmpty(roleCfg.Provider, defaultProviderName(cfg)), roleCfg)
	}
	return modelruntime.NewResolver(cfg).Resolve(ctx, selection)
}

// ProbeResolvedModel performs one bounded request against a resolved runtime.
// Native-tool transports receive an optional-only schema with a typed nil
// required slice so this check catches adapter serialization regressions.
func ProbeResolvedModel(ctx context.Context, rt modelruntime.Runtime) ModelRoleProbe {
	probe := ModelRoleProbe{Provider: rt.Provider, Model: rt.Model}
	provider := buildProviderFromRuntime(rt)
	start := time.Now()
	if provider == nil {
		probe.Err = fmt.Errorf("provider could not be built")
		probe.Latency = time.Since(start)
		return probe
	}
	probe.NativeToolsTested = llm.ProviderSupportsNativeTools(provider)
	probeCtx, cancel := context.WithTimeout(ctx, modelRoleProbeTimeout)
	defer cancel()
	resp, err := provider.Chat(probeCtx, modelProbeRequest(rt.Model, probe.NativeToolsTested))
	switch {
	case err != nil:
		probe.Err = err
	case resp == nil || strings.TrimSpace(resp.Content) == "":
		probe.Err = fmt.Errorf("model returned an empty response")
	}
	probe.Latency = time.Since(start)
	return probe
}

func modelProbeRequest(model string, includeTools bool) llm.ChatRequest {
	req := llm.ChatRequest{
		Model:        model,
		MaxTokens:    64,
		SystemPrompt: "This is a model health check. Return exactly OK and do not call tools.",
		Messages:     []llm.Message{{Role: "user", Content: "Reply with OK."}},
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

// ProbeConfiguredModelRoles performs a bounded live health check for explicitly
// configured model roles. It never invents a fallback to the primary model.
func ProbeConfiguredModelRoles(ctx context.Context, cfg *config.Config) []ModelRoleProbe {
	if cfg == nil || len(cfg.Models.Roles) == 0 {
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
	for _, roleName := range roleNames {
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
			probe.NativeToolsTested = llm.ProviderSupportsNativeTools(target.provider)
			resp, err := target.provider.Chat(probeCtx, modelProbeRequest(target.runtime.Model, probe.NativeToolsTested))
			cancel()
			switch {
			case err != nil:
				probe.Err = err
			case resp == nil || strings.TrimSpace(resp.Content) == "":
				probe.Err = fmt.Errorf("model returned an empty response")
			}
		}
		probe.Latency = time.Since(start)
		results = append(results, probe)
	}
	sort.Slice(results, func(i, j int) bool {
		return strings.Join(results[i].Roles, ",") < strings.Join(results[j].Roles, ",")
	})
	return results
}
