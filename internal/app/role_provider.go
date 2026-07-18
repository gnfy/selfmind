package app

import (
	"strings"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/kernel/memory"
	"selfmind/internal/platform/config"
)

// explicitRoleProvider builds a provider only when the role is explicitly
// configured. Management judges/labelers run automatically in the background;
// falling back to the main coding provider would hide cost and latency.
func explicitRoleProvider(mem *memory.MemoryManager, cfg *config.Config, tenantID string, role llm.ModelRole) llm.Provider {
	if cfg == nil {
		return nil
	}
	roleCfg, ok := cfg.Models.Roles[string(role)]
	if !ok || roleConfigEmpty(roleCfg) {
		return nil
	}
	roleProviderName := firstNonEmpty(roleCfg.Provider, defaultProviderName(cfg))
	provider := buildRoleProvider(cfg, role, roleProviderName, roleCfg)
	if provider == nil {
		return nil
	}
	if strings.TrimSpace(tenantID) == "" {
		tenantID = "default"
	}
	applyDynamicKeyGetter(provider, mem, tenantID, roleProviderName)
	return provider
}
