package cliapp

import (
	"fmt"
	"strings"

	"selfmind/internal/kernel/llm"
	"selfmind/internal/platform/config"
)

// Background model roles are what the daemon needs to run approval triage,
// post-run analysis, task labelling, and memory governance WITHOUT borrowing
// the foreground coding model. When they are absent the daemon does not fall
// back to the coding model — it disables those subsystems and logs a line at
// startup. That is the safe behavior, but on a fresh install it is also
// invisible: the person sees a working agent that silently never learns and
// asks for approval on every dangerous operation.
//
// Setup therefore asks once, at the same moment the foreground model is
// chosen. Reusing the foreground model stays an explicit, labelled choice —
// never a silent default.
type backgroundRoleSpec struct {
	role llm.ModelRole
	// what stays disabled while this role is unset, in user-facing words.
	disables string
}

var backgroundRoleSpecs = []backgroundRoleSpec{
	{llm.RoleFastClassifier, "smart approval triage (every dangerous operation asks a human)"},
	{llm.RoleBackgroundReview, "background review, and approval triage on pre-beta.10 builds"},
	{llm.RoleMemoryExtract, "post-run analysis, task labels, and memory governance (SelfMind never learns)"},
}

// missingBackgroundRoles lists the roles that carry no explicit provider/model.
// A role with a provider but no model is treated as missing: the resolver
// cannot route it, so it produces the same disabled subsystem.
func missingBackgroundRoles(cfg *config.Config) []llm.ModelRole {
	var missing []llm.ModelRole
	for _, spec := range backgroundRoleSpecs {
		roleCfg, ok := cfg.Models.Roles[string(spec.role)]
		if !ok || strings.TrimSpace(roleCfg.Model) == "" {
			missing = append(missing, spec.role)
		}
	}
	return missing
}

// ensureBackgroundRoleSetup offers to fill in any missing background role.
//
// It never fails setup: background roles are an enhancement, and a person who
// declines still gets a working foreground agent. Non-interactive runs print
// the exact keys to configure and move on.
func (a *App) ensureBackgroundRoleSetup(cfg *config.Config) *config.Config {
	if cfg == nil {
		return cfg
	}
	missing := missingBackgroundRoles(cfg)
	if len(missing) == 0 {
		return cfg
	}

	fmt.Fprintln(a.stdout)
	fmt.Fprintln(a.stdout, "Background models are not configured yet.")
	fmt.Fprintln(a.stdout, "SelfMind runs approval triage and memory work on a separate cheap model so it")
	fmt.Fprintln(a.stdout, "never spends your coding model on background jobs. Until they are set:")
	for _, spec := range backgroundRoleSpecs {
		if containsRole(missing, spec.role) {
			fmt.Fprintf(a.stdout, "  - %s is off: %s\n", spec.role, spec.disables)
		}
	}

	provider := strings.TrimSpace(cfg.EffectiveProvider())
	model := strings.TrimSpace(cfg.EffectiveModel())

	printGuidance := func() {
		fmt.Fprintln(a.stderr)
		fmt.Fprintln(a.stderr, "Add them under `models.roles` in the config, for example:")
		for _, role := range missing {
			fmt.Fprintf(a.stderr, "  models.roles.%s = %s/%s\n", role, blankAsDash(provider), blankAsDash(model))
		}
		fmt.Fprintln(a.stderr, "Then restart the gateway so the daemon picks them up.")
	}

	// `interactive` alone is not enough to prompt: a caller can mark the app
	// interactive while leaving stdin unset (the TUI bootstrap does exactly
	// this when it supplies its own model picker). Reading from a nil stdin
	// panics inside promptInput, so treat "no readable input" as the
	// non-interactive path and print guidance instead.
	if !a.interactive || (a.input == nil && a.stdin == nil) {
		printGuidance()
		return cfg
	}

	labels := []string{
		fmt.Sprintf("Reuse the foreground model (%s/%s)", blankAsDash(provider), blankAsDash(model)),
		"Choose a different provider and model for background work",
		"Skip for now",
	}
	choice, err := a.promptChoice("Configure background models?", labels)
	if err != nil {
		// Closed or exhausted stdin (piped input, cancelled terminal): fall
		// back to the same actionable guidance rather than exiting silently.
		printGuidance()
		return cfg
	}

	switch choice {
	case 0:
		if provider == "" || model == "" {
			fmt.Fprintln(a.stderr, "No foreground model is available to reuse; skipping background models.")
			return cfg
		}
	case 1:
		provider, err = a.promptInput("Background provider", provider)
		if err != nil {
			printGuidance()
			return cfg
		}
		model, err = a.promptInput("Background model", model)
		if err != nil {
			printGuidance()
			return cfg
		}
		provider = strings.TrimSpace(provider)
		model = strings.TrimSpace(model)
		if provider == "" || model == "" {
			fmt.Fprintln(a.stderr, "Provider and model are both required; skipping background models.")
			return cfg
		}
	default:
		fmt.Fprintln(a.stdout, "Skipped. Run `selfmind setup` again, or set `models.roles` in the config.")
		return cfg
	}

	if cfg.Models.Roles == nil {
		cfg.Models.Roles = make(map[string]config.ModelRoleConfig)
	}
	for _, role := range missing {
		// Only fill the gaps: a role the person already tuned by hand keeps
		// its own provider, model, base URL, and headers.
		existing := cfg.Models.Roles[string(role)]
		existing.Provider = provider
		existing.Model = model
		cfg.Models.Roles[string(role)] = existing
	}

	if err := config.SaveConfig(a.configPath, cfg); err != nil {
		fmt.Fprintf(a.stderr, "Could not save background models: %v\n", err)
		fmt.Fprintln(a.stderr, "Set `models.roles` in the config manually.")
		return cfg
	}

	fmt.Fprintf(a.stdout, "Background models: %s/%s for %s\n", provider, model, joinRoles(missing))
	return cfg
}

func containsRole(roles []llm.ModelRole, target llm.ModelRole) bool {
	for _, role := range roles {
		if role == target {
			return true
		}
	}
	return false
}

func joinRoles(roles []llm.ModelRole) string {
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		names = append(names, string(role))
	}
	return strings.Join(names, ", ")
}
