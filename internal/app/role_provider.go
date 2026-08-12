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

// configuredAuxiliaryRoleProvider resolves an automatic/background role from
// an explicit models.roles override first and models.auxiliary second. It
// deliberately returns nil when neither is configured so foreground coding
// capacity is never borrowed invisibly by recurring work.
func configuredAuxiliaryRoleProvider(mem *memory.MemoryManager, cfg *config.Config, tenantID string, role llm.ModelRole) llm.Provider {
	if cfg == nil {
		return nil
	}
	roleCfg, _, ok := cfg.ResolveAuxiliaryRole(string(role))
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

func auxiliaryModelRoles() []llm.ModelRole {
	return []llm.ModelRole{
		llm.RoleFastClassifier,
		llm.RoleMemoryExtract,
		llm.RoleBackgroundReview,
		llm.RoleSkillCurator,
		llm.RoleSemanticRecall,
		llm.RoleSummarizer,
	}
}

func isAuxiliaryModelRole(role llm.ModelRole) bool {
	for _, candidate := range auxiliaryModelRoles() {
		if role == candidate {
			return true
		}
	}
	return false
}
