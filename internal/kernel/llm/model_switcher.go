package llm

// ModelSetter is implemented by adapters that support runtime model switching.
type ModelSetter interface {
	SetModel(model string)
}

// SetModelName attempts to change the model name on a provider adapter.
// Returns true if the provider supports dynamic model switching.
func SetModelName(provider Provider, model string) bool {
	if ms, ok := provider.(ModelSetter); ok {
		ms.SetModel(model)
		return true
	}
	return false
}

// GetModelName returns the current model name if the provider exposes it.
func GetModelName(provider Provider) string {
	type modelGetter interface {
		GetModel() string
	}
	if mg, ok := provider.(modelGetter); ok {
		return mg.GetModel()
	}
	return ""
}
