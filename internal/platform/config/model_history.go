package config

import "strings"

const (
	maxRememberedModels    = 24
	maxRememberedReasoning = 8
)

// RememberModel records one validated model selection as presentation history.
// The list is MRU-ordered and bounded; it does not affect effective routing.
func (m *ModelsConfig) RememberModel(provider, model, reasoning string) {
	if m == nil {
		return
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return
	}
	entry := RememberedModelConfig{Provider: provider, Model: model}
	for _, existing := range m.Remembered {
		if strings.EqualFold(strings.TrimSpace(existing.Provider), provider) && strings.EqualFold(strings.TrimSpace(existing.Model), model) {
			entry.Reasoning = append(entry.Reasoning, existing.Reasoning...)
			break
		}
	}
	if value := normalizedRememberedReasoning(reasoning); value != "" {
		entry.Reasoning = append([]string{value}, entry.Reasoning...)
	}
	result := []RememberedModelConfig{entry}
	for _, existing := range m.Remembered {
		if strings.EqualFold(strings.TrimSpace(existing.Provider), provider) && strings.EqualFold(strings.TrimSpace(existing.Model), model) {
			continue
		}
		result = append(result, existing)
	}
	m.Remembered = normalizeRememberedModels(result)
}

// ForgetModel removes only SelfMind's remembered entry. A model still present
// in a live provider catalog or selected by a route remains available there.
func (m *ModelsConfig) ForgetModel(provider, model string) bool {
	if m == nil {
		return false
	}
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	removed := false
	result := make([]RememberedModelConfig, 0, len(m.Remembered))
	for _, entry := range m.Remembered {
		if strings.EqualFold(strings.TrimSpace(entry.Provider), provider) && strings.EqualFold(strings.TrimSpace(entry.Model), model) {
			removed = true
			continue
		}
		result = append(result, entry)
	}
	m.Remembered = result
	return removed
}

func normalizeRememberedModels(entries []RememberedModelConfig) []RememberedModelConfig {
	seen := make(map[string]struct{}, len(entries))
	result := make([]RememberedModelConfig, 0, min(len(entries), maxRememberedModels))
	for _, entry := range entries {
		entry.Provider = strings.TrimSpace(entry.Provider)
		entry.Model = strings.TrimSpace(entry.Model)
		if entry.Provider == "" || entry.Model == "" {
			continue
		}
		key := strings.ToLower(entry.Provider) + "\x00" + strings.ToLower(entry.Model)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		entry.Reasoning = normalizeRememberedReasoning(entry.Reasoning)
		result = append(result, entry)
		if len(result) == maxRememberedModels {
			break
		}
	}
	return result
}

func normalizeRememberedReasoning(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, min(len(values), maxRememberedReasoning))
	for _, value := range values {
		value = normalizedRememberedReasoning(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
		if len(result) == maxRememberedReasoning {
			break
		}
	}
	return result
}

func normalizedRememberedReasoning(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "auto") {
		return ""
	}
	return strings.ToLower(value)
}
