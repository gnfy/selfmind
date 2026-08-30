package llm

import "testing"

func TestProviderTransportUsesRouteAwareProxyResolution(t *testing.T) {
	transport := newProviderTransport()
	if transport.Proxy == nil {
		t.Fatal("provider transport must install route-aware proxy resolution")
	}
}
