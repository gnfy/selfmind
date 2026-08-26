package modelchange

import (
	"fmt"
	"strings"

	"selfmind/internal/modelruntime"
	"selfmind/internal/platform/config"
)

// OptionalValue distinguishes an omitted flag (preserve when compatible) from
// an explicit auto value (clear the override).
type OptionalValue struct {
	Set   bool
	Value string
}

type SelectionPatch struct {
	Route       Route
	Provider    string
	Model       string
	Reasoning   OptionalValue
	ServiceTier OptionalValue
}

type CandidateResult struct {
	Snapshot Snapshot
	Notices  []string
}

func BuildCandidate(current Snapshot, patch SelectionPatch) (CandidateResult, error) {
	current = normalizeSnapshot(current)
	patch.Provider = strings.TrimSpace(patch.Provider)
	patch.Model = strings.TrimSpace(patch.Model)
	if patch.Route != RoutePrimary && patch.Route != RouteAuxiliary && !IsManagedRoleRoute(patch.Route) {
		return CandidateResult{}, fmt.Errorf("unsupported model route %q", patch.Route)
	}
	if patch.Provider == "" || patch.Model == "" {
		return CandidateResult{}, fmt.Errorf("provider and model are required")
	}
	if descriptor, ok := modelruntime.DiscoverModelDescriptor(patch.Provider, patch.Model); ok {
		if err := validateRequestedOption("reasoning", patch.Reasoning, descriptor.SupportedReasoning); err != nil {
			return CandidateResult{}, err
		}
		if err := validateRequestedOption("service tier", patch.ServiceTier, descriptor.SupportedServiceTiers); err != nil {
			return CandidateResult{}, err
		}
	}
	selection := selectionForRoute(current, patch.Route)
	modelChanged := !strings.EqualFold(selection.Provider, patch.Provider) || selection.Model != patch.Model
	selection.Provider = patch.Provider
	selection.Model = patch.Model
	var notices []string
	selection.Reasoning, notices = reconcileOption(
		"reasoning", selection.Reasoning, patch.Reasoning, modelChanged,
		discoveredValues(patch.Provider, patch.Model, true), notices,
	)
	selection.ServiceTier, notices = reconcileOption(
		"service tier", selection.ServiceTier, patch.ServiceTier, modelChanged,
		discoveredValues(patch.Provider, patch.Model, false), notices,
	)
	setSelectionForRoute(&current, patch.Route, selection)
	if patch.Route == RoutePrimary {
		if strings.TrimSpace(current.Auxiliary.Provider) == "" && strings.TrimSpace(current.Auxiliary.Model) == "" {
			current.Auxiliary.Provider = selection.Provider
			current.Auxiliary.Model = selection.Model
		}
	}
	return CandidateResult{Snapshot: normalizeSnapshot(current), Notices: notices}, nil
}

// ResetRoleCandidate removes the selection-level override for one managed
// background role. Advanced YAML-only transport fields remain owned by config
// and are preserved when the snapshot is applied.
func ResetRoleCandidate(current Snapshot, route Route) (CandidateResult, error) {
	if !IsManagedRoleRoute(route) {
		return CandidateResult{}, fmt.Errorf("%s is not a background role", route)
	}
	current = normalizeSnapshot(current)
	setSelectionForRoute(&current, route, config.ModelSelectionConfig{})
	return CandidateResult{Snapshot: current}, nil
}

func validateRequestedOption(label string, requested OptionalValue, supported []string) error {
	if !requested.Set || normalizeAuto(requested.Value) == "" || len(supported) == 0 {
		return nil
	}
	if containsFold(supported, requested.Value) {
		return nil
	}
	return fmt.Errorf("%s %q is not supported; supported: %s", label, strings.TrimSpace(requested.Value), strings.Join(supported, ", "))
}

func reconcileOption(label, existing string, requested OptionalValue, modelChanged bool, supported []string, notices []string) (string, []string) {
	if requested.Set {
		return normalizeAuto(requested.Value), notices
	}
	existing = normalizeAuto(existing)
	if existing == "" || !modelChanged {
		return existing, notices
	}
	if len(supported) > 0 && containsFold(supported, existing) {
		return existing, notices
	}
	notices = append(notices, fmt.Sprintf("%s %q is not known to be supported by the new model; using auto", label, existing))
	return "", notices
}

func discoveredValues(provider, model string, reasoning bool) []string {
	descriptor, ok := modelruntime.DiscoverModelDescriptor(provider, model)
	if !ok {
		return nil
	}
	if reasoning {
		return append([]string(nil), descriptor.SupportedReasoning...)
	}
	return append([]string(nil), descriptor.SupportedServiceTiers...)
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func SelectionForRoute(snapshot Snapshot, route Route) config.ModelSelectionConfig {
	return selectionForRoute(normalizeSnapshot(snapshot), route)
}
