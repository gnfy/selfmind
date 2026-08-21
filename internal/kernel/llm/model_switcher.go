package llm

// ModelSetter is implemented by adapters that support runtime model switching.
type ModelSetter interface {
	SetModel(model string)
}

// ModelGetter is implemented by adapters that expose their active model name.
// It is named rather than declared inline so the wrapper-conformance test can
// enumerate it alongside the other optional capabilities.
type ModelGetter interface {
	GetModel() string
}

// ProviderUnwrapper is implemented by TRANSPARENT wrapper providers — ones that
// delegate every call to a single fixed inner provider (the VCR/flight
// recorder). Capability probes walk this chain, so a wrapper does not have to
// hand-forward each optional interface and a newly added capability is not
// silently dropped the moment a wrapper sits in the path.
//
// Providers that resolve their target dynamically (RoleProvider picks a profile
// per role) must NOT implement this: there is no single inner provider to
// probe, so they forward each capability explicitly instead.
type ProviderUnwrapper interface {
	Unwrap() Provider
}

// unwrapProvider returns the inner provider of a transparent wrapper.
func unwrapProvider(provider Provider) (Provider, bool) {
	unwrapper, ok := provider.(ProviderUnwrapper)
	if !ok {
		return nil, false
	}
	inner := unwrapper.Unwrap()
	if inner == nil {
		return nil, false
	}
	return inner, true
}

// SetModelName attempts to change the model name on a provider adapter.
// Returns true if the provider supports dynamic model switching.
func SetModelName(provider Provider, model string) bool {
	if provider == nil {
		return false
	}
	if ms, ok := provider.(ModelSetter); ok {
		ms.SetModel(model)
		return true
	}
	if inner, ok := unwrapProvider(provider); ok {
		return SetModelName(inner, model)
	}
	return false
}

// GetModelName returns the current model name if the provider exposes it.
func GetModelName(provider Provider) string {
	if provider == nil {
		return ""
	}
	if mg, ok := provider.(ModelGetter); ok {
		return mg.GetModel()
	}
	if inner, ok := unwrapProvider(provider); ok {
		return GetModelName(inner)
	}
	return ""
}
