package config

import (
	"strings"
	"testing"
)

// The generated config explains allow_network as a permission, and says exactly
// what turning it off does: trust stops permitting the network, a grant or
// approval a person gave still does. "A network-less sandbox" claimed more than
// that, because a grant lifts it.
func TestConfigTemplateDescribesTheNetworkPolicyExactly(t *testing.T) {
	if strings.Contains(defaultConfigTemplate, "network-less") {
		t.Fatal("the template still calls allow_network: false a network-less sandbox, which a network:shared grant contradicts")
	}
	for _, want := range []string{"allow_network PERMITS", "allow_network: false", "trust alone", "network:shared grant or approval"} {
		if !strings.Contains(defaultConfigTemplate, want) {
			t.Errorf("the template does not say %q", want)
		}
	}
}
