package llm

import (
	"context"
	"reflect"
	"testing"
)

// optionalProviderCapabilities is the canonical list of optional Provider
// capability interfaces in this package. ADD EVERY NEW ONE HERE. The
// conformance test below walks it, so a capability that a transparent wrapper
// silently drops fails immediately instead of going dead in production — which
// is what happened to RequestFingerprinter (0 data points over 2,933 calls) and
// to runtime model switching.
var optionalProviderCapabilities = []struct {
	name string
	typ  reflect.Type
}{
	{"NativeToolsCapable", reflect.TypeOf((*NativeToolsCapable)(nil)).Elem()},
	{"RequestFingerprinter", reflect.TypeOf((*RequestFingerprinter)(nil)).Elem()},
	{"ModelSetter", reflect.TypeOf((*ModelSetter)(nil)).Elem()},
	{"ModelGetter", reflect.TypeOf((*ModelGetter)(nil)).Elem()},
}

// satisfiesCapability reports whether provider answers a capability either
// directly or through the transparent-wrapper unwrap chain.
func satisfiesCapability(provider Provider, capability reflect.Type) bool {
	for current := provider; current != nil; {
		if reflect.TypeOf(current).Implements(capability) {
			return true
		}
		inner, ok := unwrapProvider(current)
		if !ok {
			return false
		}
		current = inner
	}
	return false
}

// TestWrapperProvidersPreserveEveryOptionalCapability enumerates the declared
// capability list instead of hand-checking a couple of methods. A wrapper that
// neither forwards a capability nor exposes Unwrap now fails here.
func TestWrapperProvidersPreserveEveryOptionalCapability(t *testing.T) {
	inner := &ResponsesAdapter{Model: "gpt-test"}
	for _, capability := range optionalProviderCapabilities {
		if !reflect.TypeOf(Provider(inner)).Implements(capability.typ) {
			t.Fatalf("test fixture is wrong: ResponsesAdapter must implement %s for this test to mean anything", capability.name)
		}
	}

	gateway := NewPolicyGateway(ProviderProfile{Provider: inner, Model: "gpt-test"})
	wrappers := []struct {
		name     string
		provider Provider
	}{
		{"vcrProvider", &vcrProvider{inner: inner, mode: "record", dir: t.TempDir()}},
		{"RoleProvider", gateway.ProviderForRole(RoleCodingAgent)},
	}
	for _, wrapper := range wrappers {
		for _, capability := range optionalProviderCapabilities {
			if !satisfiesCapability(wrapper.provider, capability.typ) {
				t.Errorf("%s drops optional capability %s: forward it or expose Unwrap()", wrapper.name, capability.name)
			}
		}
	}
}

// TestVCRWrappedProviderKeepsRuntimeModelSwitching pins the concrete symptom.
// Flight recording is on in normal use, so a dropped ModelSetter/ModelGetter
// made `/model` report an empty model and switching silently do nothing.
func TestVCRWrappedProviderKeepsRuntimeModelSwitching(t *testing.T) {
	inner := &ResponsesAdapter{Model: "gpt-test"}
	wrapped := Provider(&vcrProvider{inner: inner, mode: "record", dir: t.TempDir()})

	if got := GetModelName(wrapped); got != "gpt-test" {
		t.Fatalf("GetModelName through the recorder = %q, want gpt-test", got)
	}
	if !SetModelName(wrapped, "gpt-switched") {
		t.Fatal("SetModelName through the recorder reported no support")
	}
	if inner.Model != "gpt-switched" {
		t.Fatalf("inner adapter model = %q, want gpt-switched", inner.Model)
	}
	if got := GetModelName(wrapped); got != "gpt-switched" {
		t.Fatalf("GetModelName after switch = %q", got)
	}
}

// TestUnsupportedInnerStillReportsNoCapability guards the unwrap chain from
// manufacturing support that the inner provider does not have.
func TestUnsupportedInnerStillReportsNoCapability(t *testing.T) {
	wrapped := Provider(&vcrProvider{inner: fakeStreamProvider{}, mode: "record", dir: t.TempDir()})
	if SetModelName(wrapped, "anything") {
		t.Error("a non-switchable inner must not report switching support")
	}
	if got := GetModelName(wrapped); got != "" {
		t.Errorf("GetModelName = %q, want empty", got)
	}
	if ProviderSupportsNativeTools(wrapped) {
		t.Error("a non-native inner must not report native tool support")
	}
	if _, ok := FingerprintProviderRequest(context.Background(), wrapped, ChatRequest{}, true); ok {
		t.Error("a non-fingerprinting inner must not report fingerprint support")
	}
}
