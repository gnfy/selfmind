package cliapp

import (
	"fmt"
	"strings"

	"selfmind/internal/platform/config"
)

// ensureBackgroundRoleSetup materializes the simple local-install default:
// auxiliary starts on the primary provider/model and may be customized later.
// Logical background roles remain internal during onboarding and inherit this
// one auxiliary route unless an advanced models.roles override is present.
func (a *App) ensureBackgroundRoleSetup(cfg *config.Config) *config.Config {
	if cfg == nil {
		return cfg
	}
	cfg.InitializeAuxiliaryFromPrimary()
	auxiliary := cfg.EffectiveAuxiliary()
	if strings.TrimSpace(auxiliary.Provider) == "" || strings.TrimSpace(auxiliary.Model) == "" {
		return cfg
	}
	fmt.Fprintf(a.stdout, "Auxiliary model: %s/%s (background default; override later with models.auxiliary or models.roles)\n",
		auxiliary.Provider, auxiliary.Model)
	return cfg
}
